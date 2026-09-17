// Package outboxpg implements outbox persistence with PostgreSQL.
package outboxpg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/outbox"
)

type store struct{ pool *pgxpool.Pool }

// NewStore builds the PostgreSQL adapter for the outbox publisher.
func NewStore(pool *pgxpool.Pool) outbox.Store { return &store{pool: pool} }

// Claim leases a publisher batch in its own short transaction. Its only
// purpose is atomically selecting and leasing rows before queue I/O, so it
// must commit before the publisher sends.
func (s *store) Claim(ctx context.Context, batch int, lease time.Duration) ([]outbox.Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("outboxpg: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// next_attempt_at is the backoff deadline and is never moved by a claim;
	// locked_until is the exclusive lease deadline. Both deadlines and their
	// updates use PostgreSQL's now(), so process clock drift cannot make an
	// active lease expire early. The 0007 partial pending index still
	// applies: it narrows unpublished rows by next_attempt_at.
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT event_id
			FROM outbox_events
			WHERE published_at IS NULL
				AND next_attempt_at <= now()
				AND (locked_until IS NULL OR locked_until <= now())
			ORDER BY occurred_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE outbox_events AS event
		SET locked_until = now() + $2::interval
		FROM candidates
		WHERE event.event_id = candidates.event_id
		RETURNING event.event_id, event.payload, event.attempts, event.occurred_at`, batch, lease.String())
	if err != nil {
		return nil, fmt.Errorf("outboxpg: claim records: %w", err)
	}
	defer rows.Close()

	var records []outbox.Record
	for rows.Next() {
		var record outbox.Record
		if err := rows.Scan(&record.EventID, &record.Payload, &record.Attempts, &record.OccurredAt); err != nil {
			return nil, fmt.Errorf("outboxpg: scan claimed record: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outboxpg: iterate claimed records: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("outboxpg: commit claim: %w", err)
	}
	return records, nil
}

func (s *store) MarkPublished(ctx context.Context, eventID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		SET published_at = now(), locked_until = NULL, last_error = NULL
		WHERE event_id = $1 AND published_at IS NULL`, eventID)
	if err != nil {
		return fmt.Errorf("outboxpg: mark event published: %w", err)
	}
	return nil
}

func (s *store) ScheduleRetry(ctx context.Context, eventID string, delay time.Duration, lastErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		SET attempts = attempts + 1,
			next_attempt_at = now() + $2::interval,
			locked_until = NULL,
			last_error = $3
		WHERE event_id = $1 AND published_at IS NULL`, eventID, delay.String(), lastErr)
	if err != nil {
		return fmt.Errorf("outboxpg: schedule retry: %w", err)
	}
	return nil
}

func (s *store) Stats(ctx context.Context) (int, *time.Time, error) {
	var pending int
	var oldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(occurred_at) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending, &oldest); err != nil {
		return 0, nil, fmt.Errorf("outboxpg: read stats: %w", err)
	}
	return pending, oldest, nil
}

var Module = fx.Module("outbox-postgres", fx.Provide(NewStore))
