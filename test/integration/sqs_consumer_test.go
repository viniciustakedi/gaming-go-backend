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
	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

// This is seam 3a: a gateway-role client publishes to the real FIFO while
// the in-process Fx application consumes it. dlq-reader observes only the
// production DLQ; redrive-tester and consumer-fixture exercise the disposable
// redrive pair, all with pre-provisioned least-privilege test credentials.
// The ledger and inbox reads are assertions only; the observed contract is a
// single SQS message causing a single wallet debit, even on redelivery.
func TestSQSConsumer_BetAndRedelivery_DebitsOnce(t *testing.T) {
	t.Setenv("SQS_CONSUMER_ENABLED", "true")
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
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}
	body := sqsWagerEnvelope(t, messageID, bodyInput, key)
	gateway := loadTestCreds(t)
	client := sqsClient(t, gateway.gatewayKey, gateway.gatewaySecret)

	sendWagerMessage(t, client, wallet.ID, "delivery-1-"+uniqueID("dedup"), body)
	waitForBalance(t, h, wallet.ID, 7000)
	waitForWageringOperationsMetric(t, h, "SQS", "BET", "PROCESSED", operationsBefore+1)
	var sqsTransactionID string
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT id FROM wager_transactions WHERE external_transaction_id = $1`, bodyInput.ExternalID).Scan(&sqsTransactionID), "read SQS transaction")
	events := queryOutboxEvents(t, ctx, h, sqsTransactionID)
	if len(events) != 1 {
		t.Fatalf("SQS transaction events = %+v, want one processed event", events)
	}
	var event eventEnvelopeJSON
	requireNoError(t, json.Unmarshal(events[0].payload, &event), "decode SQS event")
	if event.CorrelationID != messageID || event.CausationID != messageID {
		t.Errorf("SQS event correlation/causation = %q/%q, want %q/%q", event.CorrelationID, event.CausationID, messageID, messageID)
	}

	// A new SQS message carrying the same operation must be an operation
	// replay, distinct from an inbox redelivery of the exact same messageId.
	replayMessageID := uniqueID("sqs-replay")
	sendWagerMessage(t, client, wallet.ID, "delivery-2-"+uniqueID("dedup"), sqsWagerEnvelope(t, replayMessageID, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: bodyInput.ExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: bodyInput.RoundID, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
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
	httpInput := wageringBodyInput{ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: httpWallet.PlayerID, WalletID: httpWallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}
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

func TestSQSConsumer_RefundBeforeBet_IsCommittedPendingThenCompleted(t *testing.T) {
	t.Setenv("SQS_CONSUMER_ENABLED", "true")
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	round := uniqueID("round")
	betExternalID := uniqueID("bet")
	messageID := uniqueID("pending-refund-message")
	refundExternalID := uniqueID("refund")
	ref := betExternalID
	refund := wageringBodyInput{ProviderID: "provider-a", ExternalID: refundExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "REFUND", Amount: "30.00", Currency: testCurrency, ReferenceID: &ref}
	creds := loadTestCreds(t)
	sendWagerMessage(t, sqsClient(t, creds.gatewayKey, creds.gatewaySecret), wallet.ID, "pending-refund-"+uniqueID("dedup"), sqsWagerEnvelope(t, messageID, refund, "idem-"+uniqueID("key")))
	waitForInbox(t, h, messageID)

	var pendingID, status string
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT id, status FROM wager_transactions WHERE external_transaction_id = $1`, refundExternalID).Scan(&pendingID, &status), "read SQS pending refund")
	if status != "PENDING_REFERENCE" {
		t.Fatalf("SQS refund status = %s, want PENDING_REFERENCE", status)
	}

	betResp, betBody := doWagering(t, h, providerAToken(t), wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}, "idem-"+uniqueID("key"), "")
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("BET status = %d, want 200, body = %s", betResp.StatusCode, betBody)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		requireNoError(t, h.pool.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id = $1`, pendingID).Scan(&status), "read SQS pending refund completion")
		if status == "PROCESSED" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != "PROCESSED" {
		t.Fatalf("SQS refund status after reference = %s, want PROCESSED", status)
	}
	if balance, _, found := queryWalletRow(t, ctx, h, wallet.ID); !found || balance != 10000 {
		t.Errorf("wallet balance = %d (found %v), want 10000", balance, found)
	}
}

func TestSQSConsumer_RollbackAndWinBeforeBet_AreCommittedPendingThenCompleted(t *testing.T) {
	for _, kind := range []string{"ROLLBACK", "WIN"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("SQS_CONSUMER_ENABLED", "true")
			t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
			t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "20ms")
			h := newAppHarness(t)
			ctx := context.Background()
			wallet := openWalletHTTP(t, h, "100.00")
			round, betExternalID, messageID := uniqueID("round"), uniqueID("bet"), uniqueID("pending-message")
			ref := betExternalID
			pending := wageringBodyInput{ProviderID: "provider-a", ExternalID: uniqueID("pending"), PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: kind, Amount: "30.00", Currency: testCurrency, ReferenceID: &ref}
			sendWagerMessage(t, sqsClient(t, loadTestCreds(t).gatewayKey, loadTestCreds(t).gatewaySecret), wallet.ID, "pending-"+uniqueID("dedup"), sqsWagerEnvelope(t, messageID, pending, "idem-"+uniqueID("key")))
			waitForInbox(t, h, messageID)
			var pendingID, status string
			requireNoError(t, h.pool.QueryRow(ctx, `SELECT id, status FROM wager_transactions WHERE external_transaction_id = $1`, pending.ExternalID).Scan(&pendingID, &status), "read SQS pending operation")
			if status != "PENDING_REFERENCE" {
				t.Fatalf("SQS %s status = %s, want PENDING_REFERENCE", kind, status)
			}
			betResp, betBody := doWagering(t, h, providerAToken(t), wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}, "idem-"+uniqueID("key"), "")
			if betResp.StatusCode != http.StatusOK {
				t.Fatalf("BET status = %d, want 200, body = %s", betResp.StatusCode, betBody)
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				requireNoError(t, h.pool.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id = $1`, pendingID).Scan(&status), "read SQS pending completion")
				if status == "PROCESSED" {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if status != "PROCESSED" {
				t.Fatalf("SQS %s after reference = %s, want PROCESSED", kind, status)
			}
			if balance, _, found := queryWalletRow(t, ctx, h, wallet.ID); !found || balance != 10000 {
				t.Errorf("SQS %s wallet balance = %d (found %v), want 10000", kind, balance, found)
			}
		})
	}
}

// These are seam 3a tests: the gateway publishes to the real production
// input FIFO, the complete in-process Fx app consumes it, and dlq-reader is
// a fixture credential with only ReceiveMessage/DeleteMessage on the real
// DLQ. A unique marker in the body prevents stale DLQ residue from making an
// assertion pass; each found message is deleted before the test returns.
func TestSQSConsumer_InvalidOpening_GoesToDLQWithoutBlockingAnotherWallet(t *testing.T) {
	t.Setenv("SQS_CONSUMER_ENABLED", "true")
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	dlqBefore := sqsDLQMetric(t, h, "KIND_NOT_ALLOWED")
	invalidWallet := openWalletHTTP(t, h, "100.00")
	validWallet := openWalletHTTP(t, h, "100.00")
	marker := uniqueID("invalid-opening")
	invalidExternalID := uniqueID("invalid-external")
	invalid := sqsWagerEnvelope(t, marker, wageringBodyInput{
		ProviderID: "provider-a", ExternalID: invalidExternalID, PlayerID: invalidWallet.PlayerID, WalletID: invalidWallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "OPENING", Amount: "30.00", Currency: testCurrency,
	}, "idem-"+uniqueID("key"))
	valid := sqsWagerEnvelope(t, uniqueID("valid-message"), wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("valid-external"), PlayerID: validWallet.PlayerID, WalletID: validWallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
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
	t.Setenv("SQS_CONSUMER_ENABLED", "true")
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	marker := uniqueID("hash-mismatch")
	first := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("first-external"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}
	secondExternalID := uniqueID("second-external")
	second := first
	second.ExternalID = secondExternalID
	second.Amount = "31.00"

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
		ProviderID: "provider-a", ExternalID: uniqueID("external"), PlayerID: newUUID(t), WalletID: newUUID(t),
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
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
	t.Setenv("SQS_CONSUMER_ENABLED", "true")
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	lock := lockWallet(t, wallet.ID)
	messageID := uniqueID("stop-completes")
	input := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("external"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
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
	t.Setenv("SQS_CONSUMER_ENABLED", "true")
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	t.Setenv("SQS_CONSUMER_VISIBILITY_TIMEOUT", "5s")
	t.Setenv("SQS_CONSUMER_PROCESSING_TIMEOUT", "2s")
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	lock := lockWallet(t, wallet.ID)
	messageID := uniqueID("stop-expires")
	input := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("external"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
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

func TestSQSConsumer_StopWaitsForLongPollAndLeavesLaterMessageVisible(t *testing.T) {
	t.Setenv("SQS_CONSUMER_ENABLED", "true")
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	h := newAppHarness(t)

	time.Sleep(250 * time.Millisecond)
	started := time.Now()
	if err := h.stop(t, context.Background()); err != nil {
		t.Fatalf("Fx stop during long poll: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 1500*time.Millisecond {
		t.Fatalf("Fx stop during 1s long poll took %s, want less than 1.5s", elapsed)
	}

	rc := loadTestCreds(t)
	marker := uniqueID("after-stop")
	message, err := json.Marshal(map[string]string{"probe": marker})
	requireNoError(t, err, "marshal post-stop probe")
	gateway := sqsClient(t, rc.gatewayKey, rc.gatewaySecret)
	sendWagerMessage(t, gateway, "after-stop-"+marker, marker, message)
	reader := sqsClient(t, rc.consumerKey, rc.consumerSecret)
	queue := queueURL(inputQueueName())
	handle, err := receiveByMarker(context.Background(), reader, queue, marker, time.Now().Add(2*time.Second))
	requireNoError(t, err, "receive post-stop probe")
	if handle == nil {
		t.Fatalf("post-stop probe %s remained hidden after the consumer stopped", marker)
	}
	_, err = reader.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queue), ReceiptHandle: handle})
	requireNoError(t, err, "delete post-stop probe")
}

func sqsWagerEnvelope(t *testing.T, messageID string, in wageringBodyInput, key string) []byte {
	return testclient.SQSWagerEnvelope(t, messageID, in, key, "2026-09-15T00:00:00Z")
}

func sendWagerMessage(t *testing.T, client *sqs.Client, group, dedup string, body []byte) {
	testclient.SendWagerMessage(t, client, group, dedup, body)
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
