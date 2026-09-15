package outbox

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

func TestPublisher_LogsAndCountsMarkPublishedStoreFailure(t *testing.T) {
	var logs bytes.Buffer
	registry := prometheus.NewRegistry()
	store := &fakeStore{
		records: []Record{{
			EventID: "event-1",
			Payload: []byte(`{"data":{"walletId":"wallet-1"}}`),
		}},
		markPublishedErr: errors.New("database unavailable"),
	}
	publisher := NewPublisher(store, fakeQueue{}, config.Config{Outbox: config.OutboxConfig{
		RetryBase: time.Second,
		RetryMax:  time.Minute,
	}}, slog.New(slog.NewTextHandler(&logs, nil)), registry)

	err := publisher.PublishBatch(context.Background())
	if err == nil {
		t.Fatal("publish batch error = nil, want store failure")
	}
	if got := logs.String(); !strings.Contains(got, "eventId=event-1") || !strings.Contains(got, "database unavailable") {
		t.Errorf("store failure log = %q, want eventId and error", got)
	}
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, metric := range metrics {
		if metric.GetName() == "outbox_store_errors_total" && len(metric.Metric) == 1 && metric.Metric[0].GetCounter().GetValue() == 1 && metric.Metric[0].Label[0].GetName() == "operation" && metric.Metric[0].Label[0].GetValue() == "mark_published" {
			return
		}
	}
	t.Error("outbox_store_errors_total{operation=mark_published} = 1 not found")
}

type fakeStore struct {
	records          []Record
	markPublishedErr error
}

func (s *fakeStore) Claim(context.Context, int, time.Duration) ([]Record, error) {
	return s.records, nil
}
func (s *fakeStore) MarkPublished(context.Context, string) error { return s.markPublishedErr }
func (s *fakeStore) ScheduleRetry(context.Context, string, time.Duration, string) error {
	return nil
}
func (s *fakeStore) Stats(context.Context) (int, *time.Time, error) { return 0, nil, nil }

type fakeQueue struct{}

func (fakeQueue) Send(context.Context, []byte, string, string) error { return nil }
