//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx/fxtest"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/consumer"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/queue"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/wageringmetrics"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// This is seam 3a: a gateway-role client publishes to the real FIFO while
// the in-process Fx application consumes it. dlq-reader observes only the
// production DLQ; redrive-tester and consumer-fixture exercise the disposable
// redrive pair, all with pre-provisioned least-privilege test credentials.
// The ledger and inbox reads are assertions only; the observed contract is a
// single SQS message causing a single wallet debit, even on redelivery.
func TestSQSConsumer_BetAndRedelivery_DebitsOnce(t *testing.T) {
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	ledgerBefore := countLedgerEntries(t, ctx, h, wallet.ID)
	operationsBefore := wageringOperationsMetric(t, h, "SQS", "BET", "PROCESSED")
	duplicatesBefore := wageringDuplicateAttemptsMetric(t, h, "SQS")
	messageID := uniqueID("sqs-message")
	key := "idem-" + uniqueID("key")
	bodyInput := wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("ext"), playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}
	body := sqsWagerEnvelope(t, messageID, bodyInput, key)
	gateway := loadTestCreds(t)
	client := sqsClient(t, gateway.gatewayKey, gateway.gatewaySecret)

	sendWagerMessage(t, client, wallet.ID, "delivery-1-"+uniqueID("dedup"), body)
	waitForBalance(t, h, wallet.ID, 7000)
	waitForWageringOperationsMetric(t, h, "SQS", "BET", "PROCESSED", operationsBefore+1)

	// A new SQS message carrying the same operation must be an operation
	// replay, distinct from an inbox redelivery of the exact same messageId.
	replayMessageID := uniqueID("sqs-replay")
	sendWagerMessage(t, client, wallet.ID, "delivery-2-"+uniqueID("dedup"), sqsWagerEnvelope(t, replayMessageID, wageringBodyInput{
		providerID: "provider-a", externalID: bodyInput.externalID, playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: bodyInput.roundID, gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}, key))
	waitForWageringDuplicateAttemptsMetric(t, h, "SQS", duplicatesBefore+1)

	// An exact messageId redelivery remains an inbox duplicate, a separate
	// consumer transport metric from the domain-operation replay above.
	sendWagerMessage(t, client, wallet.ID, "delivery-3-"+uniqueID("dedup"), body)
	waitForSQSMetric(t, h, "sqs_consumer_duplicate_messages_total", 1)

	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != ledgerBefore+1 {
		t.Errorf("ledger entries = %d, want %d (opening plus one debit)", got, ledgerBefore+1)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != 7000 {
		t.Errorf("ledger net = %d, want 7000", net)
	}
	var completedAt *time.Time
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT completed_at FROM inbox_messages WHERE consumer_name = 'wager-transactions' AND message_id = $1`, messageID).Scan(&completedAt), "read committed inbox row")
	if completedAt == nil {
		t.Error("inbox completed_at = nil, want durable completion before SQS delete")
	}

	// A second wallet proves that the financial idempotency key crosses the
	// transport boundary too: HTTP commits first, and SQS later records only
	// its inbox completion while returning the already persisted operation.
	httpWallet := openWalletHTTP(t, h, "100.00")
	httpInput := wageringBodyInput{providerID: "provider-a", externalID: uniqueID("ext"), playerID: httpWallet.PlayerID, walletID: httpWallet.ID, roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency}
	httpKey := "idem-" + uniqueID("key")
	response, responseBody := doWagering(t, h, providerAToken(t), httpInput, httpKey, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("HTTP BET status = %d, want 200, body = %s", response.StatusCode, responseBody)
	}
	httpMessageID := uniqueID("sqs-message")
	sendWagerMessage(t, client, httpWallet.ID, "delivery-"+uniqueID("dedup"), sqsWagerEnvelope(t, httpMessageID, httpInput, httpKey))
	waitForInbox(t, h, httpMessageID)
	if balance, _, found := queryWalletRow(t, ctx, h, httpWallet.ID); !found || balance != 7000 {
		t.Errorf("wallet after HTTP + SQS equivalent BET = %d (found %v), want 7000", balance, found)
	}
	if got := countLedgerEntries(t, ctx, h, httpWallet.ID); got != 2 {
		t.Errorf("ledger entries = %d, want 2", got)
	}
}

// These are seam 3a tests: the gateway publishes to the real production
// input FIFO, the complete in-process Fx app consumes it, and dlq-reader is
// a fixture credential with only ReceiveMessage/DeleteMessage on the real
// DLQ. A unique marker in the body prevents stale DLQ residue from making an
// assertion pass; each found message is deleted before the test returns.
func TestSQSConsumer_InvalidOpening_GoesToDLQWithoutBlockingAnotherWallet(t *testing.T) {
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	dlqBefore := sqsDLQMetric(t, h, "KIND_NOT_ALLOWED")
	invalidWallet := openWalletHTTP(t, h, "100.00")
	validWallet := openWalletHTTP(t, h, "100.00")
	marker := uniqueID("invalid-opening")
	invalidExternalID := uniqueID("invalid-external")
	invalid := sqsWagerEnvelope(t, marker, wageringBodyInput{
		providerID: "provider-a", externalID: invalidExternalID, playerID: invalidWallet.PlayerID, walletID: invalidWallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "OPENING", amount: "30.00", currency: testCurrency,
	}, "idem-"+uniqueID("key"))
	valid := sqsWagerEnvelope(t, uniqueID("valid-message"), wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("valid-external"), playerID: validWallet.PlayerID, walletID: validWallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}, "idem-"+uniqueID("key"))

	rc := loadTestCreds(t)
	gateway := sqsClient(t, rc.gatewayKey, rc.gatewaySecret)
	sendWagerMessage(t, gateway, invalidWallet.ID, "invalid-"+marker, invalid)
	sendWagerMessage(t, gateway, validWallet.ID, "valid-"+uniqueID("dedup"), valid)

	waitForBalance(t, h, validWallet.ID, 7000)
	deleteMarkedDLQMessage(t, sqsClient(t, rc.dlqReaderKey, rc.dlqReaderSecret), dlqQueueName(), marker)
	waitForSqsDLQMetric(t, h, "KIND_NOT_ALLOWED", dlqBefore+1)

	if balance, _, found := queryWalletRow(t, ctx, h, invalidWallet.ID); !found || balance != 10000 {
		t.Errorf("invalid OPENING wallet balance = %d (found %v), want 10000", balance, found)
	}
	if got := countLedgerEntries(t, ctx, h, invalidWallet.ID); got != 1 {
		t.Errorf("invalid OPENING wallet ledger entries = %d, want 1 opening entry", got)
	}
	var domainRows int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`, invalidExternalID).Scan(&domainRows), "count invalid OPENING domain rows")
	if domainRows != 0 {
		t.Errorf("invalid OPENING domain rows = %d, want 0", domainRows)
	}
}

func waitForWageringOperationsMetric(t *testing.T, h *appHarness, channel, kind, status string, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := wageringOperationsMetric(t, h, channel, kind, status); got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("wagering_operations_total{channel=%q,kind=%q,status=%q} did not reach %.0f", channel, kind, status, want)
}

func waitForWageringDuplicateAttemptsMetric(t *testing.T, h *appHarness, channel string, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := wageringDuplicateAttemptsMetric(t, h, channel); got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("wagering_duplicate_attempts_total{channel=%q} did not reach %.0f", channel, want)
}

func waitForSqsDLQMetric(t *testing.T, h *appHarness, reason string, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := sqsDLQMetric(t, h, reason); got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sqs_consumer_dlq_total{reason=%q} did not reach %.0f", reason, want)
}

func sqsDLQMetric(t *testing.T, h *appHarness, reason string) float64 {
	t.Helper()
	response, body := h.do(t, http.MethodGet, "/metrics", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", response.StatusCode)
	}
	prefix := fmt.Sprintf(`sqs_consumer_dlq_total{reason="%s"} `, reason)
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
		requireNoError(t, err, "parse sqs_consumer_dlq_total value")
		return value
	}
	return 0
}

func TestSQSConsumer_HashMismatchForMessageID_GoesToDLQ(t *testing.T) {
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	marker := uniqueID("hash-mismatch")
	first := wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("first-external"), playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}
	secondExternalID := uniqueID("second-external")
	second := first
	second.externalID = secondExternalID
	second.amount = "31.00"

	rc := loadTestCreds(t)
	gateway := sqsClient(t, rc.gatewayKey, rc.gatewaySecret)
	sendWagerMessage(t, gateway, wallet.ID, "first-"+uniqueID("dedup"), sqsWagerEnvelope(t, marker, first, "idem-"+uniqueID("key")))
	waitForBalance(t, h, wallet.ID, 7000)
	sendWagerMessage(t, gateway, wallet.ID, "second-"+uniqueID("dedup"), sqsWagerEnvelope(t, marker, second, "idem-"+uniqueID("key")))
	deleteMarkedDLQMessage(t, sqsClient(t, rc.dlqReaderKey, rc.dlqReaderSecret), dlqQueueName(), marker)

	if balance, _, found := queryWalletRow(t, ctx, h, wallet.ID); !found || balance != 7000 {
		t.Errorf("wallet balance after hash mismatch = %d (found %v), want 7000", balance, found)
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != 2 {
		t.Errorf("ledger entries after hash mismatch = %d, want 2", got)
	}
	var mismatchRows int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`, secondExternalID).Scan(&mismatchRows), "count hash-mismatch domain rows")
	if mismatchRows != 0 {
		t.Errorf("hash-mismatch domain rows = %d, want 0", mismatchRows)
	}
}

// The consumer here is the production Consumer wired to real MiniStack
// fixture queues. Its pool targets a closed local port, so Begin fails before
// the use case can touch storage; the observed retry counter and the marked
// message in the fixture DLQ prove ChangeMessageVisibility and broker redrive.
func TestSQSConsumer_UnavailableDatabase_RedrivesFixtureMessage(t *testing.T) {
	rc := loadTestCreds(t)
	registry := prometheus.NewRegistry()
	pool, err := pgxpool.New(context.Background(), "postgres://wallet_app:unused@127.0.0.1:1/wallet?sslmode=disable&connect_timeout=1")
	requireNoError(t, err, "create unreachable Postgres pool")
	t.Cleanup(pool.Close)

	queues := &queue.Queues{
		Consumer: sqsClient(t, rc.consumerFixtureKey, rc.consumerFixtureSecret),
		InputURL: queueURL(rc.redriveInputQueueName),
		DLQURL:   queueURL(rc.redriveDLQQueueName),
	}
	consumerCfg := config.Config{SQS: config.SQSConfig{Consumer: config.SQSConsumerConfig{
		Enabled: true, PollWait: time.Second, Concurrency: 1, VisibilityTimeout: 2 * time.Second,
		ProcessingTimeout: time.Second, RetryBase: time.Second, RetryMax: time.Second, ShutdownTimeout: 3 * time.Second,
		ProviderAllowlist: []string{"provider-a"},
	}}}
	useCase := walletapp.NewProcessOperationUseCase(nil, wageringmetrics.New(registry))
	c := consumer.New(queues, pool, useCase, consumerCfg, slog.Default(), registry)
	lifecycle := fxtest.NewLifecycle(t)
	consumer.RegisterLifecycle(lifecycle, c)
	lifecycle.RequireStart()
	t.Cleanup(lifecycle.RequireStop)

	marker := uniqueID("database-unavailable")
	message := sqsWagerEnvelope(t, marker, wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("external"), playerID: newUUID(t), walletID: newUUID(t),
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}, "idem-"+uniqueID("key"))
	redriveTester := sqsClient(t, rc.redriveKey, rc.redriveSecret)
	if _, err := redriveTester.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(queueURL(rc.redriveInputQueueName)), MessageBody: aws.String(string(message)),
		MessageGroupId: aws.String("database-" + marker), MessageDeduplicationId: aws.String(marker),
	}); err != nil {
		t.Fatalf("send fixture message: %v", err)
	}

	deleteMarkedDLQMessage(t, redriveTester, rc.redriveDLQQueueName, marker)
	if got := counterValue(t, registry, "sqs_consumer_retries_total"); got < 1 {
		t.Fatalf("sqs_consumer_retries_total = %.0f, want at least 1", got)
	}
}

func TestSQSConsumer_StopWithinDeadline_CommitsAndDeletesInFlightMessage(t *testing.T) {
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	lock := lockWallet(t, wallet.ID)
	messageID := uniqueID("stop-completes")
	input := wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("external"), playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}
	rc := loadTestCreds(t)
	sendWagerMessage(t, sqsClient(t, rc.gatewayKey, rc.gatewaySecret), wallet.ID, "stop-completes-"+messageID, sqsWagerEnvelope(t, messageID, input, "idem-"+uniqueID("key")))
	waitForWalletLockWait(t, lock.transactionID)

	stopped := make(chan error, 1)
	go func() { stopped <- h.stop(t, context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Fx stop finished while the wallet lock was still held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	lock.unlock()
	if err := <-stopped; err != nil {
		t.Fatalf("Fx stop within the drain deadline: %v", err)
	}

	restarted := newAppHarness(t)
	waitForBalance(t, restarted, wallet.ID, 7000)
	waitForInbox(t, restarted, messageID)
	if got := countLedgerEntries(t, ctx, restarted, wallet.ID); got != 2 {
		t.Errorf("ledger entries after graceful stop = %d, want 2", got)
	}
}

func TestSQSConsumer_StopDeadlineExpires_ReleasesAndReprocessesExactlyOnce(t *testing.T) {
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	t.Setenv("SQS_CONSUMER_VISIBILITY_TIMEOUT", "5s")
	t.Setenv("SQS_CONSUMER_PROCESSING_TIMEOUT", "2s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	lock := lockWallet(t, wallet.ID)
	messageID := uniqueID("stop-expires")
	input := wageringBodyInput{
		providerID: "provider-a", externalID: uniqueID("external"), playerID: wallet.PlayerID, walletID: wallet.ID,
		roundID: uniqueID("round"), gameID: "game-1", kind: "BET", amount: "30.00", currency: testCurrency,
	}
	rc := loadTestCreds(t)
	sendWagerMessage(t, sqsClient(t, rc.gatewayKey, rc.gatewaySecret), wallet.ID, "stop-expires-"+messageID, sqsWagerEnvelope(t, messageID, input, "idem-"+uniqueID("key")))
	waitForWalletLockWait(t, lock.transactionID)

	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	err := h.stop(t, stopCtx)
	cancelStop()
	if err == nil {
		t.Fatal("Fx stop with a blocked message = nil, want deadline error and visibility release")
	}
	lock.unlock()

	restarted := newAppHarness(t)
	waitForBalanceBefore(t, restarted, wallet.ID, 7000, 2*time.Second)
	waitForInbox(t, restarted, messageID)
	if got := countLedgerEntries(t, ctx, restarted, wallet.ID); got != 2 {
		t.Errorf("ledger entries after timeout and restart = %d, want 2", got)
	}
}

func sqsWagerEnvelope(t *testing.T, messageID string, in wageringBodyInput, key string) []byte {
	t.Helper()
	payload := map[string]any{"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": "2026-09-15T00:00:00Z", "data": map[string]any{"providerId": in.providerID, "externalTransactionId": in.externalID, "idempotencyKey": key, "playerId": in.playerID, "walletId": in.walletID, "roundId": in.roundID, "gameId": in.gameID, "kind": in.kind, "money": map[string]string{"amount": in.amount, "currency": in.currency}}}
	encoded, err := json.Marshal(payload)
	requireNoError(t, err, "marshal SQS wager envelope")
	return encoded
}

func sendWagerMessage(t *testing.T, client *sqs.Client, group, dedup string, body []byte) {
	t.Helper()
	_, err := client.SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(queueURL(inputQueueName())), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(dedup)})
	requireNoError(t, err, "gateway sends wager message")
}

func deleteMarkedDLQMessage(t *testing.T, client *sqs.Client, queueName, marker string) {
	t.Helper()
	const timeout = 30 * time.Second
	queue := queueURL(queueName)
	handle, err := receiveByMarker(context.Background(), client, queue, marker, time.Now().Add(timeout))
	if err != nil {
		t.Fatalf("receive marker %s from DLQ: %v", marker, err)
	}
	if handle == nil {
		t.Fatalf("marker %s did not reach the DLQ within %s", marker, timeout)
	}
	if _, err := client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queue), ReceiptHandle: handle}); err != nil {
		t.Fatalf("delete DLQ marker %s: %v", marker, err)
	}
}

func counterValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	requireNoError(t, err, "gather consumer metrics")
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		if len(family.GetMetric()) != 1 || family.GetMetric()[0].GetCounter() == nil {
			t.Fatalf("metric %s has unexpected shape", name)
		}
		return family.GetMetric()[0].GetCounter().GetValue()
	}
	t.Fatalf("metric %s was not registered", name)
	return 0
}

// lockWallet uses a real Postgres row lock instead of mocking the consumer's
// work. It pauses the public SQS-to-wallet path after inbox insertion and
// before the debit, which makes the two Fx shutdown outcomes observable.
type walletLock struct {
	transactionID string
	unlock        func()
}

func lockWallet(t *testing.T, walletID string) walletLock {
	t.Helper()
	conn := connectApp(t, context.Background())
	tx, err := conn.Begin(context.Background())
	requireNoError(t, err, "begin wallet lock transaction")
	var id string
	requireNoError(t, tx.QueryRow(context.Background(), `SELECT id FROM wallets WHERE id = $1 FOR UPDATE`, walletID).Scan(&id), "lock wallet row")
	if id != walletID {
		t.Fatalf("locked wallet id = %s, want %s", id, walletID)
	}
	var transactionID string
	requireNoError(t, tx.QueryRow(context.Background(), `SELECT txid_current()::text`).Scan(&transactionID), "read wallet lock transaction ID")
	var once sync.Once
	unlock := func() {
		once.Do(func() { requireNoError(t, tx.Rollback(context.Background()), "release wallet lock") })
	}
	t.Cleanup(unlock)
	return walletLock{transactionID: transactionID, unlock: unlock}
}

func waitForWalletLockWait(t *testing.T, transactionID string) {
	t.Helper()
	owner := connectOwner(t, context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := owner.QueryRow(context.Background(), `
			SELECT count(*)
			FROM pg_locks
			WHERE locktype = 'transactionid'
			  AND transactionid = $1::xid
			  AND NOT granted`, transactionID).Scan(&waiting)
		requireNoError(t, err, "observe consumer waiting on wallet lock")
		if waiting >= 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("consumer did not reach the in-flight wallet lock before deadline")
}

func waitForBalance(t *testing.T, h *appHarness, walletID string, want int64) {
	t.Helper()
	waitForBalanceBefore(t, h, walletID, want, 10*time.Second)
}

func waitForBalanceBefore(t *testing.T, h *appHarness, walletID string, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got, _, found := queryWalletRow(t, context.Background(), h, walletID); found && got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _, _ := queryWalletRow(t, context.Background(), h, walletID)
	t.Fatalf("wallet balance = %d before %s deadline, want %d", got, timeout, want)
}

func waitForInbox(t *testing.T, h *appHarness, messageID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var completed *time.Time
		err := h.pool.QueryRow(context.Background(), `SELECT completed_at FROM inbox_messages WHERE consumer_name = 'wager-transactions' AND message_id = $1`, messageID).Scan(&completed)
		if err == nil && completed != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("inbox message %s was not completed before deadline", messageID)
}

func waitForSQSMetric(t *testing.T, h *appHarness, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, body := h.do(t, http.MethodGet, "/metrics", nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET /metrics status = %d", response.StatusCode)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, name+" ") {
				continue
			}
			got, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 64)
			requireNoError(t, err, fmt.Sprintf("parse %s", name))
			if got >= want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("metric %s did not reach %.0f before deadline", name, want)
}
