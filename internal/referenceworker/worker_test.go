package referenceworker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

func TestIsTransient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "connection", err: &pgconn.PgError{Code: "08006"}, want: true},
		{name: "serialization", err: &pgconn.PgError{Code: "40001"}, want: true},
		{name: "deadlock", err: &pgconn.PgError{Code: "40P01"}, want: true},
		{name: "lock timeout", err: &pgconn.PgError{Code: "55P03"}, want: true},
		{name: "statement timeout", err: &pgconn.PgError{Code: "57014"}, want: true},
		{name: "admin shutdown", err: &pgconn.PgError{Code: "57P01"}, want: true},
		{name: "crash shutdown", err: &pgconn.PgError{Code: "57P02"}, want: true},
		{name: "cannot connect now", err: &pgconn.PgError{Code: "57P03"}, want: true},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "canceled", err: context.Canceled, want: true},
		{name: "rehydration", err: walletapp.ErrInvalidPersistedTransaction, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransient(tt.err); got != tt.want {
				t.Errorf("isTransient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestProcessBatchRoutesOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		resumeErr      error
		wantReschedule int
		wantFail       int
	}{
		{name: "transient reschedules", resumeErr: context.DeadlineExceeded, wantReschedule: 1},
		{name: "permanent fails", resumeErr: walletapp.ErrInvalidPersistedTransaction, wantFail: 1},
		{name: "success does not reschedule", resumeErr: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &fakeProcessor{resumeErr: tt.resumeErr}
			worker := newWorker(
				fakeStore{claimed: []Record{{TransactionID: "pending-1", WalletID: "wallet-1", Attempts: 2}}},
				processor,
				workerConfig(),
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				prometheus.NewRegistry(),
				func(int64) int64 { return 0 },
			)
			if err := worker.ProcessBatch(context.Background()); err != nil {
				t.Fatalf("ProcessBatch() error = %v", err)
			}
			if processor.rescheduled != tt.wantReschedule {
				t.Errorf("ReschedulePendingAfterFailure calls = %d, want %d", processor.rescheduled, tt.wantReschedule)
			}
			if processor.failed != tt.wantFail {
				t.Errorf("FailPending calls = %d, want %d", processor.failed, tt.wantFail)
			}
		})
	}
}

func TestJitterStaysWithinBoundsAndUsesItsSource(t *testing.T) {
	t.Parallel()
	delay := 100 * time.Millisecond
	low := jitter(delay, func(int64) int64 { return 0 })
	high := jitter(delay, func(n int64) int64 { return n - 1 })
	if low != 100*time.Millisecond {
		t.Errorf("low jitter = %s, want 100ms", low)
	}
	if high != 150*time.Millisecond {
		t.Errorf("high jitter = %s, want 150ms", high)
	}
	if low < delay || low > delay+delay/2 || high < delay || high > delay+delay/2 {
		t.Errorf("jitter values = %s and %s, want both in [100ms,150ms]", low, high)
	}
}

type fakeStore struct {
	claimed []Record
	err     error
}

func (s fakeStore) Claim(context.Context, int, time.Duration) ([]Record, error) {
	return s.claimed, s.err
}
func (s fakeStore) Pending(context.Context) (int, error) { return 0, nil }

type fakeProcessor struct {
	resumeErr   error
	rescheduled int
	failed      int
}

func (p *fakeProcessor) ResumePending(context.Context, string, string, walletapp.PendingResumeSettings) (walletapp.PendingResumeOutcome, error) {
	return walletapp.PendingProcessed, p.resumeErr
}
func (p *fakeProcessor) ReschedulePendingAfterFailure(context.Context, string, string, time.Duration) error {
	p.rescheduled++
	return nil
}
func (p *fakeProcessor) FailPending(context.Context, string, string) error {
	p.failed++
	return nil
}

func workerConfig() config.ReferenceWorkerConfig {
	return config.ReferenceWorkerConfig{Enabled: true, BatchSize: 1, Lease: time.Second, RetryBase: 100 * time.Millisecond, RetryMax: time.Second, MaxAttempts: 3, PollInterval: time.Second, ShutdownTimeout: time.Second}
}
