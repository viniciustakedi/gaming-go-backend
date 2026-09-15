// Package outbox publishes committed integration-event snapshots to a queue.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/backoff"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

// Record is the immutable event snapshot and its delivery state returned by a claimed outbox row.
type Record struct {
	EventID    string
	Payload    []byte
	Attempts   int
	OccurredAt time.Time
}

// Store is the publisher's durable outbox boundary. Its implementation owns transaction and database details.
type Store interface {
	Claim(ctx context.Context, batch int, lease time.Duration) ([]Record, error)
	MarkPublished(ctx context.Context, eventID string) error
	ScheduleRetry(ctx context.Context, eventID string, delay time.Duration, lastErr string) error
	Stats(ctx context.Context) (pending int, oldest *time.Time, err error)
}

// Queue is the publisher's output boundary. The adapter maps the stable event identity and wallet group to its concrete FIFO client.
type Queue interface {
	Send(ctx context.Context, payload []byte, groupID, deduplicationID string) error
}

type eventBody struct {
	Data struct {
		WalletID string `json:"walletId"`
	} `json:"data"`
}

// Publisher coordinates a polling loop with its Fx lifecycle. It deliberately
// keeps a claim transaction short: queue I/O happens after commit, while the
// database lease lets another instance resume an abandoned record.
type Publisher struct {
	store   Store
	queue   Queue
	cfg     config.OutboxConfig
	logger  *slog.Logger
	metrics *publisherMetrics

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewPublisher(store Store, queue Queue, cfg config.Config, logger *slog.Logger, registry *prometheus.Registry) *Publisher {
	return &Publisher{store: store, queue: queue, cfg: cfg.Outbox, logger: logger, metrics: newPublisherMetrics(registry)}
}

func RegisterLifecycle(lc fx.Lifecycle, publisher *Publisher) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { publisher.start(); return nil },
		OnStop:  func(ctx context.Context) error { return publisher.stop(ctx) },
	})
}

func (p *Publisher) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() { defer close(p.done); p.run(ctx) }()
}

func (p *Publisher) stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("outbox: stop publisher: %w", ctx.Err())
	}
}

func (p *Publisher) run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := p.PublishBatch(ctx); err != nil && ctx.Err() == nil {
			p.logger.Error("outbox publish batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PublishBatch processes one bounded claimed batch. The lifecycle loop calls
// it repeatedly; exposing the bounded operation makes delivery testable at
// the Store and Queue seams without exposing database or queue clients.
func (p *Publisher) PublishBatch(ctx context.Context) error {
	records, err := p.store.Claim(ctx, p.cfg.BatchSize, p.cfg.Lease)
	if err != nil {
		return fmt.Errorf("outbox: claim records: %w", err)
	}
	p.refreshMetrics(ctx)
	for _, record := range records {
		if err := p.publish(ctx, record); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return nil
}

func (p *Publisher) publish(ctx context.Context, record Record) error {
	var envelope eventBody
	if err := json.Unmarshal(record.Payload, &envelope); err != nil || envelope.Data.WalletID == "" {
		if err == nil {
			err = errInvalidWalletID
		}
		return p.retry(ctx, record, fmt.Errorf("outbox: invalid committed payload: %w", err))
	}
	if err := p.queue.Send(ctx, record.Payload, envelope.Data.WalletID, record.EventID); err != nil {
		return p.retry(ctx, record, fmt.Errorf("outbox: send event %s: %w", record.EventID, err))
	}
	if err := p.store.MarkPublished(ctx, record.EventID); err != nil {
		p.recordStoreError("mark_published", record.EventID, err)
		return fmt.Errorf("outbox: confirm event %s: %w", record.EventID, err)
	}
	p.logger.Info("outbox event published", "eventId", record.EventID)
	p.refreshMetrics(ctx)
	return nil
}

var errInvalidWalletID = errors.New("payload data.walletId is empty")

func (p *Publisher) retry(ctx context.Context, record Record, sendErr error) error {
	delay := backoff.Exponential(record.Attempts, p.cfg.RetryBase, p.cfg.RetryMax)
	if err := p.store.ScheduleRetry(ctx, record.EventID, delay, sendErr.Error()); err != nil {
		p.recordStoreError("schedule_retry", record.EventID, err)
		return fmt.Errorf("outbox: schedule retry for event %s: %w", record.EventID, err)
	}
	p.metrics.retries.Inc()
	p.logger.Warn("outbox event send failed; retry scheduled", "eventId", record.EventID, "attempt", record.Attempts+1, "retryAfter", delay, "error", sendErr)
	p.refreshMetrics(ctx)
	return nil
}

func (p *Publisher) recordStoreError(operation, eventID string, err error) {
	p.metrics.storeErrors.WithLabelValues(operation).Inc()
	p.logger.Error("outbox store operation failed", "operation", operation, "eventId", eventID, "error", err)
}

func (p *Publisher) refreshMetrics(ctx context.Context) {
	pending, oldest, err := p.store.Stats(ctx)
	if err != nil {
		p.logger.Debug("outbox metrics refresh failed", "error", err)
		return
	}
	p.metrics.pending.Set(float64(pending))
	if oldest == nil {
		p.metrics.oldestAge.Set(0)
		return
	}
	p.metrics.oldestAge.Set(time.Since(*oldest).Seconds())
}

type publisherMetrics struct {
	pending     prometheus.Gauge
	oldestAge   prometheus.Gauge
	retries     prometheus.Counter
	storeErrors *prometheus.CounterVec
}

func newPublisherMetrics(registry *prometheus.Registry) *publisherMetrics {
	m := &publisherMetrics{
		pending:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "outbox_pending_events", Help: "Outbox events not confirmed as published."}),
		oldestAge:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "outbox_oldest_unpublished_age_seconds", Help: "Age of the oldest unconfirmed outbox event."}),
		retries:     prometheus.NewCounter(prometheus.CounterOpts{Name: "outbox_retries_total", Help: "Outbox event send failures scheduled for retry."}),
		storeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "outbox_store_errors_total", Help: "Outbox store operation failures."}, []string{"operation"}),
	}
	registry.MustRegister(m.pending, m.oldestAge, m.retries, m.storeErrors)
	return m
}

// Module starts after queue and Postgres dependencies have started, and Fx
// stops it before closing those clients because it depends on both of them.
var Module = fx.Module("outbox", fx.Provide(NewPublisher), fx.Invoke(RegisterLifecycle))
