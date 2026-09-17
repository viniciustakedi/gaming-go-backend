package walletpg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

type outboxRepository struct{ q pg.Querier }

func newOutboxRepository(q pg.Querier) walletapp.OutboxRepository {
	return &outboxRepository{q: q}
}

// Insert writes one outbox_events row with next_attempt_at left at its
// column default (now()), so a freshly committed event is immediately
// eligible for the outbox publisher's claim query.
func (r *outboxRepository) Insert(ctx context.Context, record walletapp.OutboxRecord) error {
	payload, err := json.Marshal(record.Payload)
	if err != nil {
		return fmt.Errorf("walletpg: marshal outbox payload: %w", err)
	}

	_, err = r.q.Exec(ctx, `
		INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		record.EventID, record.AggregateType, record.AggregateID, record.EventType, record.EventVersion, payload, record.OccurredAt)
	if err != nil {
		return fmt.Errorf("walletpg: insert outbox event: %w", err)
	}
	return nil
}
