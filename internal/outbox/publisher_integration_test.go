//go:build integration

package outbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/envfile"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/migrate"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/outbox"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/outboxpg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/queue"
)

// This is seam 3a against real Postgres and MiniStack. The worker's public
// observable contract is a durable pending row that later reaches the FIFO;
// the test only reads the database to distinguish that state from a lost row.
func TestPublisher_SendFailureKeepsEventPendingThenRetries(t *testing.T) {
	cfg, pool, queues := publisherTestDependencies(t)
	cfg.Outbox.PollInterval = 10 * time.Millisecond
	cfg.Outbox.Lease = 100 * time.Millisecond
	cfg.Outbox.RetryBase = 20 * time.Millisecond
	cfg.Outbox.RetryMax = 100 * time.Millisecond

	eventID, walletID, payload := pendingEvent(t)
	insertPendingEvent(t, pool, eventID, walletID, payload)

	publisher := outbox.NewPublisher(outboxpg.NewStore(pool), failingQueue{}, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), prometheus.NewRegistry())
	if err := publisher.PublishBatch(context.Background()); err != nil {
		t.Fatalf("publish unavailable event: %v", err)
	}
	var attempts int
	var publishedAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT attempts, published_at FROM outbox_events WHERE event_id = $1`, eventID).Scan(&attempts, &publishedAt); err != nil {
		t.Fatalf("read unavailable event: %v", err)
	}
	if attempts != 1 || publishedAt != nil {
		t.Fatalf("unavailable event state = attempts %d, published_at %v, want 1 and nil", attempts, publishedAt)
	}

	recoveredPublisher := outbox.NewPublisher(outboxpg.NewStore(pool), queue.NewOutboxPublisher(queues), cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), prometheus.NewRegistry())
	time.Sleep(cfg.Outbox.RetryBase + 10*time.Millisecond)
	if err := recoveredPublisher.PublishBatch(context.Background()); err != nil {
		t.Fatalf("publish recovered event: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT published_at FROM outbox_events WHERE event_id = $1`, eventID).Scan(&publishedAt); err != nil {
		t.Fatalf("read recovered event: %v", err)
	}
	if publishedAt == nil {
		t.Fatal("recovered event published_at = nil, want timestamp")
	}
}

func TestPublishers_CompeteWithoutRetryingTheSameClaim(t *testing.T) {
	cfg, pool, queues := publisherTestDependencies(t)
	cfg.Outbox.PollInterval = 10 * time.Millisecond
	cfg.Outbox.Lease = 3 * time.Second
	cfg.Outbox.BatchSize = 1

	eventID, walletID, payload := pendingEvent(t)
	insertPendingEvent(t, pool, eventID, walletID, payload)

	const publisherCount = 4
	publishers := make([]*outbox.Publisher, publisherCount)
	start := make(chan struct{})
	var ready, finished sync.WaitGroup
	ready.Add(publisherCount)
	finished.Add(publisherCount)
	for i := range publishers {
		publishers[i] = outbox.NewPublisher(outboxpg.NewStore(pool), queue.NewOutboxPublisher(queues), cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), prometheus.NewRegistry())
		go func(publisher *outbox.Publisher) {
			ready.Done()
			<-start
			if err := publisher.PublishBatch(context.Background()); err != nil {
				t.Errorf("publish contested batch: %v", err)
			}
			finished.Done()
		}(publishers[i])
	}
	ready.Wait()
	close(start)
	finished.Wait()

	received := receiveEvent(t, eventsReader(t), queues.OutputURL, eventID, 2*time.Second)
	if received != 1 {
		t.Errorf("messages with eventId %s read from wallet-events.fifo = %d, want exactly 1", eventID, received)
	}

	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT attempts FROM outbox_events WHERE event_id = $1`, eventID).Scan(&attempts); err != nil {
		t.Fatalf("read publisher attempts: %v", err)
	}
	if attempts != 0 {
		t.Errorf("attempts for concurrently contested event = %d, want 0", attempts)
	}
}

func TestPublisher_ExpiredLeaseRepublishesSameSnapshot(t *testing.T) {
	cfg, pool, queues := publisherTestDependencies(t)
	cfg.Outbox.Lease = 200 * time.Millisecond
	cfg.Outbox.BatchSize = 1

	eventID, walletID, payload := pendingEvent(t)
	insertPendingEvent(t, pool, eventID, walletID, payload)

	slow := &slowQueue{started: make(chan struct{}), release: make(chan struct{})}
	first := outbox.NewPublisher(outboxpg.NewStore(pool), slow, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), prometheus.NewRegistry())
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.PublishBatch(context.Background()) }()
	<-slow.started

	var nextAttemptDue, locked bool
	if err := pool.QueryRow(context.Background(), `
		SELECT next_attempt_at <= now(), locked_until > now()
		FROM outbox_events WHERE event_id = $1`, eventID).Scan(&nextAttemptDue, &locked); err != nil {
		t.Fatalf("read claimed event deadlines: %v", err)
	}
	if !nextAttemptDue || !locked {
		t.Fatalf("claimed deadlines: next_attempt_at <= now() = %v, locked_until > now() = %v, want true, true", nextAttemptDue, locked)
	}

	time.Sleep(cfg.Outbox.Lease + 50*time.Millisecond)
	second := outbox.NewPublisher(outboxpg.NewStore(pool), queue.NewOutboxPublisher(queues), cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), prometheus.NewRegistry())
	if err := second.PublishBatch(context.Background()); err != nil {
		t.Fatalf("publish reclaimed event: %v", err)
	}
	close(slow.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("finish abandoned publisher batch: %v", err)
	}

	reader := eventsReader(t)
	body, received := receiveEventBody(t, reader, queues.OutputURL, eventID, 2*time.Second)
	if received != 1 {
		t.Fatalf("messages with reclaimed eventId %s = %d, want exactly 1", eventID, received)
	}
	var snapshot []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE event_id = $1`, eventID).Scan(&snapshot); err != nil {
		t.Fatalf("read reclaimed event snapshot: %v", err)
	}
	if string(body) != string(snapshot) {
		t.Errorf("reclaimed message payload = %s, want committed snapshot %s", body, snapshot)
	}

	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT attempts FROM outbox_events WHERE event_id = $1`, eventID).Scan(&attempts); err != nil {
		t.Fatalf("read reclaimed event attempts: %v", err)
	}
	if attempts != 0 {
		t.Errorf("attempts after lease recovery = %d, want 0", attempts)
	}
}

func publisherTestDependencies(t *testing.T) (config.Config, *pgxpool.Pool, *queue.Queues) {
	t.Helper()
	host, port, database, sslmode := publisherTestDatabase(t)
	t.Setenv("DATABASE_HOST", host)
	t.Setenv("DATABASE_PORT", port)
	t.Setenv("DATABASE_NAME", database)
	t.Setenv("DATABASE_SSLMODE", sslmode)
	t.Setenv("DATABASE_APP_CREDENTIALS_FILE", filepath.Join("..", "..", "deploy", "postgres", ".runtime", "credentials.env"))
	t.Setenv("SQS_ENDPOINT_URL", "http://localhost:4566")
	t.Setenv("AUTH_ISSUER_URL", "http://localhost:8081/realms/wallet")

	credentialsPath := filepath.Join("..", "..", "deploy", "ministack", ".runtime", "app-credentials.env")
	values, err := envfile.Read(credentialsPath)
	if err != nil {
		t.Fatalf("read %s: %v", credentialsPath, err)
	}
	for _, key := range []string{"SQS_CONSUMER_ACCESS_KEY_ID", "SQS_CONSUMER_SECRET_ACCESS_KEY", "SQS_PUBLISHER_ACCESS_KEY_ID", "SQS_PUBLISHER_SECRET_ACCESS_KEY"} {
		value := values[key]
		if value == "" {
			t.Fatalf("missing %s in %s", key, credentialsPath)
		}
		t.Setenv(key, value)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	pool, err := pg.New(cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	queues, err := queue.New(cfg)
	if err != nil {
		t.Fatalf("build queue clients: %v", err)
	}
	output, err := queues.Publisher.GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String(cfg.SQS.OutputQueueName)})
	if err != nil {
		t.Fatalf("resolve output queue: %v", err)
	}
	queues.OutputURL = aws.ToString(output.QueueUrl)
	return cfg, pool, queues
}

func publisherTestDatabase(t *testing.T) (host, port, database, sslmode string) {
	t.Helper()
	ownerDSN := os.Getenv("DATABASE_URL")
	if ownerDSN == "" {
		ownerDSN = "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"
	}
	ownerURL, err := url.Parse(ownerDSN)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}

	database = "wallet_outbox_test_" + uuid.NewString()
	owner, err := pgx.Connect(context.Background(), ownerDSN)
	if err != nil {
		t.Fatalf("connect migration owner: %v", err)
	}
	if _, err := owner.Exec(context.Background(), `CREATE DATABASE `+pgx.Identifier{database}.Sanitize()); err != nil {
		_ = owner.Close(context.Background())
		t.Fatalf("create disposable publisher database: %v", err)
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatalf("close migration owner: %v", err)
	}
	t.Cleanup(func() {
		owner, err := pgx.Connect(context.Background(), ownerDSN)
		if err != nil {
			t.Errorf("connect migration owner to drop disposable publisher database: %v", err)
			return
		}
		defer owner.Close(context.Background())
		if _, err := owner.Exec(context.Background(), `DROP DATABASE `+pgx.Identifier{database}.Sanitize()); err != nil {
			t.Errorf("drop disposable publisher database: %v", err)
		}
	})

	ownerURL.Path = "/" + database
	if err := migrate.Up(ownerURL.String()); err != nil {
		t.Fatalf("migrate disposable publisher database: %v", err)
	}

	port = ownerURL.Port()
	if port == "" {
		port = "5432"
	}
	sslmode = ownerURL.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	return ownerURL.Hostname(), port, database, sslmode
}

type failingQueue struct{}

func (failingQueue) Send(context.Context, []byte, string, string) error {
	return fmt.Errorf("simulated SQS unavailable")
}

type slowQueue struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (q *slowQueue) Send(ctx context.Context, _ []byte, _, _ string) error {
	q.once.Do(func() { close(q.started) })
	select {
	case <-q.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func pendingEvent(t *testing.T) (string, string, []byte) {
	t.Helper()
	eventID, walletID := uuid.NewString(), uuid.NewString()
	payload, err := json.Marshal(map[string]any{
		"eventId": eventID, "eventType": "WalletBalanceChanged", "aggregateId": walletID,
		"correlationId": "publisher-integration-test", "occurredAt": "2026-09-15T00:00:00Z", "version": 1,
		"data": map[string]any{"walletId": walletID},
	})
	if err != nil {
		t.Fatalf("marshal outbox fixture: %v", err)
	}
	return eventID, walletID, payload
}

func insertPendingEvent(t *testing.T, pool *pgxpool.Pool, eventID, walletID string, payload []byte) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version, payload, occurred_at)
		VALUES ($1, 'Wallet', $2, 'WalletBalanceChanged', 1, $3::jsonb, now())`, eventID, walletID, payload)
	if err != nil {
		t.Fatalf("insert pending outbox event: %v", err)
	}
}

func eventsReader(t *testing.T) *sqs.Client {
	t.Helper()
	values, err := envfile.Read(filepath.Join("..", "..", "deploy", "ministack", ".runtime", "test-credentials.env"))
	if err != nil {
		t.Fatalf("read events-reader credentials: %v", err)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(values["EVENTS_READER_ACCESS_KEY_ID"], values["EVENTS_READER_SECRET_ACCESS_KEY"], "")),
	)
	if err != nil {
		t.Fatalf("build events-reader client: %v", err)
	}
	return sqs.NewFromConfig(awsCfg, func(options *sqs.Options) { options.BaseEndpoint = aws.String("http://localhost:4566") })
}

func receiveEvent(t *testing.T, reader *sqs.Client, queueURL, eventID string, timeout time.Duration) int {
	t.Helper()
	_, received := receiveEventBody(t, reader, queueURL, eventID, timeout)
	return received
}

func receiveEventBody(t *testing.T, reader *sqs.Client, queueURL, eventID string, timeout time.Duration) ([]byte, int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var body []byte
	received := 0
	for time.Now().Before(deadline) {
		messages, err := reader.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		})
		if err != nil {
			t.Fatalf("receive outbox event as events-reader: %v", err)
		}
		for _, message := range messages.Messages {
			var envelope struct {
				EventID string `json:"eventId"`
			}
			if message.Body == nil || json.Unmarshal([]byte(*message.Body), &envelope) != nil || envelope.EventID != eventID {
				continue
			}
			received++
			body = []byte(*message.Body)
			if _, err := reader.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: message.ReceiptHandle}); err != nil {
				t.Fatalf("delete observed outbox event: %v", err)
			}
		}
	}
	return body, received
}
