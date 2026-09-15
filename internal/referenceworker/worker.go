// Package referenceworker resumes durable operations that arrived before
// their referenced transaction. Claims are deliberately short: SKIP LOCKED
// lets instances divide ready work without waiting, while next_attempt_at is
// also a lease, so a crash merely makes the row eligible again.
package referenceworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/backoff"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// Record is one pending transaction claimed by the durable store.
type Record struct {
	TransactionID string
	WalletID      string
	Attempts      int
}

// Store owns durable claim and pending-count operations. Its pgx adapter keeps
// the claim transaction, lease and SKIP LOCKED query outside the worker.
type Store interface {
	Claim(ctx context.Context, batch int, lease time.Duration) ([]Record, error)
	Pending(ctx context.Context) (int, error)
}

type pendingProcessor interface {
	ResumePending(ctx context.Context, transactionID, walletID string, settings walletapp.PendingResumeSettings) (walletapp.PendingResumeOutcome, error)
	ReschedulePendingAfterFailure(ctx context.Context, transactionID, walletID string, delay time.Duration) error
	FailPending(ctx context.Context, transactionID, walletID string) error
}

type Worker struct {
	store   Store
	useCase pendingProcessor
	cfg     config.ReferenceWorkerConfig
	logger  *slog.Logger
	metrics *workerMetrics
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	random  func(int64) int64
}

func New(store Store, useCase *walletapp.ProcessOperationUseCase, cfg config.Config, logger *slog.Logger, registry *prometheus.Registry) *Worker {
	useCase.SetPendingReferenceTTL(cfg.ReferenceWorker.TTL)
	return newWorker(store, useCase, cfg.ReferenceWorker, logger, registry, rand.Int64N)
}

func newWorker(store Store, useCase pendingProcessor, cfg config.ReferenceWorkerConfig, logger *slog.Logger, registry *prometheus.Registry, random func(int64) int64) *Worker {
	return &Worker{store: store, useCase: useCase, cfg: cfg, logger: logger, metrics: newWorkerMetrics(registry), random: random}
}

func RegisterLifecycle(lc fx.Lifecycle, worker *Worker) {
	lc.Append(fx.Hook{OnStart: func(context.Context) error { worker.start(); return nil }, OnStop: worker.stop})
}

func (w *Worker) start() {
	if !w.cfg.Enabled {
		w.logger.Info("pending reference worker disabled")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	go func() { defer close(w.done); w.run(ctx) }()
}

func (w *Worker) stop(ctx context.Context) error {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	deadline := w.cfg.ShutdownTimeout
	if stopDeadline, ok := ctx.Deadline(); ok && time.Until(stopDeadline) < deadline {
		deadline = time.Until(stopDeadline)
	}
	wait, cancelWait := context.WithTimeout(context.Background(), deadline)
	defer cancelWait()
	select {
	case <-done:
		return nil
	case <-wait.Done():
		return fmt.Errorf("referenceworker: stop: %w", wait.Err())
	}
}

func (w *Worker) run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.ProcessBatch(ctx); err != nil && ctx.Err() == nil {
			w.logger.Error("pending reference batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessBatch exposes one bounded polling pass for the composition seam.
func (w *Worker) ProcessBatch(ctx context.Context) error {
	claimed, err := w.store.Claim(ctx, w.cfg.BatchSize, w.cfg.Lease)
	if err != nil {
		return fmt.Errorf("referenceworker: claim pending references: %w", err)
	}
	for _, record := range claimed {
		delay := backoff.Exponential(record.Attempts, w.cfg.RetryBase, w.cfg.RetryMax)
		outcome, err := w.useCase.ResumePending(ctx, record.TransactionID, record.WalletID, walletapp.PendingResumeSettings{MaxAttempts: w.cfg.MaxAttempts, RetryDelay: w.jitter(delay)})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !isPermanent(err) {
				err = w.useCase.ReschedulePendingAfterFailure(ctx, record.TransactionID, record.WalletID, w.jitter(delay))
				if err == nil {
					w.metrics.retries.Inc()
					w.logger.Warn("pending reference processing retried", "transactionId", record.TransactionID, "attempt", record.Attempts+1)
					continue
				}
				w.logger.Error("pending reference transient retry scheduling failed", "transactionId", record.TransactionID, "attempt", record.Attempts+1, "error", err)
				continue
			}
			if failErr := w.useCase.FailPending(ctx, record.TransactionID, record.WalletID); failErr != nil {
				w.logger.Error("pending reference permanent failure could not be recorded", "transactionId", record.TransactionID, "attempt", record.Attempts+1, "error", failErr)
				continue
			}
			w.metrics.failures.Inc()
			w.logger.Error("pending reference marked permanently failed", "transactionId", record.TransactionID, "attempt", record.Attempts+1, "error", err)
			continue
		}
		if outcome == walletapp.PendingRescheduled {
			w.metrics.retries.Inc()
		}
		w.logger.Info("pending reference processed", "transactionId", record.TransactionID, "attempt", record.Attempts+1, "outcome", outcome)
	}
	w.refreshMetrics(ctx)
	return nil
}

// isTransient classifies by an explicit permanent list: only persisted data
// that cannot be rehydrated is terminal. Connection, serialization, deadlock,
// lock and statement cancellation, and restart/unavailable (57P01-57P03)
// errors, like any other infrastructure failure, are retried.
func isTransient(err error) bool {
	return !errors.Is(err, walletapp.ErrInvalidPersistedTransaction) && !errors.Is(err, walletapp.ErrCorruptedResultingBalance)
}

// Only irrecoverable persisted-data failures are terminal. Every other
// database or infrastructure failure is retried so a transient outage cannot
// turn an otherwise valid operation into FAILED.
func isPermanent(err error) bool { return !isTransient(err) }

func (w *Worker) refreshMetrics(ctx context.Context) {
	pending, err := w.store.Pending(ctx)
	if err != nil {
		w.logger.Debug("pending reference metric refresh failed", "error", err)
		return
	}
	w.metrics.pending.Set(float64(pending))
}

// jitter adds a random [0, delay/2] interval to the exponential delay. The
// injected source lets tests use fixed endpoints while production spreads rows
// claimed in the same batch.
func jitter(delay time.Duration, random func(int64) int64) time.Duration {
	if delay <= 0 {
		return delay
	}
	return delay + time.Duration(random(int64(delay/2)+1))
}

func (w *Worker) jitter(delay time.Duration) time.Duration {
	return jitter(delay, w.random)
}

type workerMetrics struct {
	retries  prometheus.Counter
	pending  prometheus.Gauge
	failures prometheus.Counter
}

func newWorkerMetrics(registry *prometheus.Registry) *workerMetrics {
	m := &workerMetrics{retries: prometheus.NewCounter(prometheus.CounterOpts{Name: "pending_reference_retries_total", Help: "Pending references rescheduled for another lookup."}), pending: prometheus.NewGauge(prometheus.GaugeOpts{Name: "pending_reference_active", Help: "Transactions currently awaiting a reference."}), failures: prometheus.NewCounter(prometheus.CounterOpts{Name: "pending_reference_failures_total", Help: "Pending references marked failed after an explicit permanent error."})}
	registry.MustRegister(m.retries, m.pending, m.failures)
	return m
}
