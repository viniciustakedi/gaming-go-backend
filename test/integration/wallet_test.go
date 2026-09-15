//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/pg"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletpg"
	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

// seam 3a - internal/app + internal/httpapi's real HTTP contract for
// POST /wallets and GET /wallets/{walletId}, driven through appHarness
// (apphttp_test.go) against the same Postgres and MiniStack every other
// test in this package uses.

// walletHTTPResponse and moneyJSON are test/testclient's shared DTOs (spec,
// seam 3: "o mesmo cliente de teste roda em dois harnesses") -
// test/multiinstance uses the same types.
type walletHTTPResponse = testclient.WalletHTTPResponse

type moneyJSON = testclient.MoneyJSON

// eventEnvelopeJSON mirrors internal/domain/wallet.eventEnvelope, the
// shared envelope every outbox payload in this package carries.
type eventEnvelopeJSON struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	AggregateID   string `json:"aggregateId"`
	CorrelationID string `json:"correlationId"`
	CausationID   string `json:"causationId"`
	OccurredAt    string `json:"occurredAt"`
	Version       int    `json:"version"`
}

// requireUTCEnvelope checks the fields every outbox event shares, common to
// both payload shapes below.
func requireUTCEnvelope(t *testing.T, env eventEnvelopeJSON, wantEventType, wantAggregateID, wantCorrelationID string) {
	t.Helper()
	if env.EventID == "" {
		t.Error("eventId is empty, want a generated id")
	}
	if env.EventType != wantEventType {
		t.Errorf("eventType = %q, want %q", env.EventType, wantEventType)
	}
	if env.AggregateID != wantAggregateID {
		t.Errorf("aggregateId = %q, want %q", env.AggregateID, wantAggregateID)
	}
	if env.CorrelationID != wantCorrelationID {
		t.Errorf("correlationId = %q, want %q", env.CorrelationID, wantCorrelationID)
	}
	if !strings.HasSuffix(env.OccurredAt, "Z") {
		t.Errorf("occurredAt = %q, want a UTC RFC3339 timestamp (suffix Z)", env.OccurredAt)
	}
	if env.Version != 1 {
		t.Errorf("version = %d, want 1", env.Version)
	}
}

type walletBalanceChangedPayload struct {
	eventEnvelopeJSON
	Data struct {
		WalletID      string    `json:"walletId"`
		TransactionID string    `json:"transactionId"`
		Direction     string    `json:"direction"`
		Money         moneyJSON `json:"money"`
		BalanceBefore moneyJSON `json:"balanceBefore"`
		BalanceAfter  moneyJSON `json:"balanceAfter"`
		WalletVersion int64     `json:"walletVersion"`
	} `json:"data"`
}

type wagerTransactionProcessedPayload struct {
	eventEnvelopeJSON
	Data struct {
		TransactionID string    `json:"transactionId"`
		WalletID      string    `json:"walletId"`
		PlayerID      string    `json:"playerId"`
		Kind          string    `json:"kind"`
		Origin        string    `json:"origin"`
		Money         moneyJSON `json:"money"`
		Status        string    `json:"status"`
	} `json:"data"`
}

// requireNoExternalMetadata fails unless every OPENING-only-irrelevant,
// external-provider field is absent from the raw payload - not just empty
// in the typed struct above, but genuinely missing, which is what the
// domain event's `omitempty` tags promise for an internally originated
// transaction (spec: "External fields are absent for OPENING").
func requireNoExternalMetadata(t *testing.T, raw []byte) {
	t.Helper()
	var decoded struct {
		Data map[string]any `json:"data"`
	}
	requireNoError(t, json.Unmarshal(raw, &decoded), "decode WagerTransactionProcessed payload as a map")
	for _, field := range []string{
		"externalTransactionId", "providerId", "idempotencyKey", "payloadHash",
		"roundId", "gameId", "referenceExternalTransactionId",
	} {
		if _, present := decoded.Data[field]; present {
			t.Errorf("WagerTransactionProcessed.data.%s is present, want it omitted for an internal OPENING transaction", field)
		}
	}
}

type errorHTTPResponse struct {
	Error struct {
		Code        string `json:"code"`
		Message     string `json:"message"`
		Correctable bool   `json:"correctable"`
	} `json:"error"`
}

func openWalletBody(playerID, amount, currency string) []byte {
	return testclient.OpenWalletBody(playerID, amount, currency)
}

func decodeWalletResponse(t *testing.T, body []byte) walletHTTPResponse {
	t.Helper()
	return testclient.DecodeWalletResponse(t, body)
}

func decodeErrorResponse(t *testing.T, body []byte) errorHTTPResponse {
	t.Helper()
	var resp errorHTTPResponse
	requireNoError(t, json.Unmarshal(body, &resp), "decode error response: "+string(body))
	return resp
}

func queryWalletRow(t *testing.T, ctx context.Context, h *appHarness, id string) (balance, version int64, found bool) {
	t.Helper()
	err := h.pool.QueryRow(ctx, `SELECT balance, version FROM wallets WHERE id = $1`, id).Scan(&balance, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false
	}
	requireNoError(t, err, "query wallet row")
	return balance, version, true
}

func countWalletsByPlayerAndCurrency(t *testing.T, ctx context.Context, h *appHarness, playerID, currency string) int {
	t.Helper()
	var count int
	err := h.pool.QueryRow(ctx, `SELECT count(*) FROM wallets WHERE player_id = $1 AND currency = $2`, playerID, currency).Scan(&count)
	requireNoError(t, err, "count wallets")
	return count
}

type openingRow struct {
	id               string
	status           string
	amount           int64
	resultingBalance int64
}

func queryOpeningTransaction(t *testing.T, ctx context.Context, h *appHarness, walletID string) (openingRow, bool) {
	t.Helper()
	var row openingRow
	var resultingBalance *int64
	err := h.pool.QueryRow(ctx, `
		SELECT id, status, amount, resulting_balance
		FROM wager_transactions
		WHERE wallet_id = $1 AND kind = 'OPENING'`, walletID).Scan(&row.id, &row.status, &row.amount, &resultingBalance)
	if errors.Is(err, pgx.ErrNoRows) {
		return openingRow{}, false
	}
	requireNoError(t, err, "query opening transaction")
	if resultingBalance != nil {
		row.resultingBalance = *resultingBalance
	}
	return row, true
}

func countLedgerEntries(t *testing.T, ctx context.Context, h *appHarness, walletID string) int {
	t.Helper()
	var count int
	err := h.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&count)
	requireNoError(t, err, "count ledger entries")
	return count
}

// netLedgerBalance sums credits minus debits for a wallet, from the ledger
// alone - the independent source of truth every financial scenario in this
// package checks the stored balance against (spec: "Todo cenário financeiro
// termina conferindo o saldo armazenado contra a soma de créditos menos
// débitos do ledger").
func netLedgerBalance(t *testing.T, ctx context.Context, h *appHarness, walletID string) int64 {
	t.Helper()
	var net int64
	err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE -amount END), 0)
		FROM wallet_ledger_entries
		WHERE wallet_id = $1`, walletID).Scan(&net)
	requireNoError(t, err, "sum ledger entries")
	return net
}

type outboxRow struct {
	eventType     string
	aggregateType string
	aggregateID   string
	payload       []byte
}

func queryOutboxEvents(t *testing.T, ctx context.Context, h *appHarness, aggregateID string) []outboxRow {
	t.Helper()
	rows, err := h.pool.Query(ctx, `
		SELECT event_type, aggregate_type, aggregate_id, payload
		FROM outbox_events
		WHERE aggregate_id = $1
		ORDER BY occurred_at`, aggregateID)
	requireNoError(t, err, "query outbox events")
	defer rows.Close()

	var events []outboxRow
	for rows.Next() {
		var e outboxRow
		requireNoError(t, rows.Scan(&e.eventType, &e.aggregateType, &e.aggregateID, &e.payload), "scan outbox event")
		events = append(events, e)
	}
	requireNoError(t, rows.Err(), "iterate outbox events")
	return events
}

func TestOpenWallet_PositiveBalance_RecordsOpeningLedgerAndOutboxInSameCommit(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)
	correlationID := uniqueID("corr")

	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"X-Correlation-Id": correlationID},
		openWalletBody(playerID, "100.00", "BRL"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want 201, body = %s", resp.StatusCode, body)
	}

	wallet := decodeWalletResponse(t, body)
	if wallet.PlayerID != playerID || wallet.Balance.Amount != "100.00" || wallet.Balance.Currency != "BRL" || wallet.Version != 1 {
		t.Fatalf("unexpected wallet response: %+v", wallet)
	}

	balance, version, found := queryWalletRow(t, ctx, h, wallet.ID)
	if !found || balance != 10000 || version != 1 {
		t.Fatalf("stored wallet = (balance %d, version %d, found %v), want (10000, 1, true)", balance, version, found)
	}

	opening, found := queryOpeningTransaction(t, ctx, h, wallet.ID)
	if !found {
		t.Fatal("want an OPENING transaction row, found none")
	}
	if opening.status != "PROCESSED" || opening.amount != 10000 || opening.resultingBalance != 10000 {
		t.Errorf("opening transaction = %+v, want status PROCESSED, amount 10000, resultingBalance 10000", opening)
	}

	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != 1 {
		t.Errorf("ledger entries = %d, want 1", got)
	}
	if net := netLedgerBalance(t, ctx, h, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}

	balanceEvents := queryOutboxEvents(t, ctx, h, wallet.ID)
	if len(balanceEvents) != 1 || balanceEvents[0].eventType != "WalletBalanceChanged" || balanceEvents[0].aggregateType != "Wallet" {
		t.Fatalf("WalletBalanceChanged outbox events for wallet %s = %+v, want exactly one", wallet.ID, balanceEvents)
	}
	var balanceChanged walletBalanceChangedPayload
	requireNoError(t, json.Unmarshal(balanceEvents[0].payload, &balanceChanged), "decode WalletBalanceChanged payload")
	requireUTCEnvelope(t, balanceChanged.eventEnvelopeJSON, "WalletBalanceChanged", wallet.ID, correlationID)
	wantBalanceData := struct {
		WalletID      string
		TransactionID string
		Direction     string
		Money         moneyJSON
		BalanceBefore moneyJSON
		BalanceAfter  moneyJSON
		WalletVersion int64
	}{
		WalletID: wallet.ID, TransactionID: opening.id, Direction: "CREDIT",
		Money:         moneyJSON{Amount: "100.00", Currency: "BRL"},
		BalanceBefore: moneyJSON{Amount: "0.00", Currency: "BRL"},
		BalanceAfter:  moneyJSON{Amount: "100.00", Currency: "BRL"},
		WalletVersion: 1,
	}
	if balanceChanged.Data.WalletID != wantBalanceData.WalletID || balanceChanged.Data.TransactionID != wantBalanceData.TransactionID ||
		balanceChanged.Data.Direction != wantBalanceData.Direction || balanceChanged.Data.Money != wantBalanceData.Money ||
		balanceChanged.Data.BalanceBefore != wantBalanceData.BalanceBefore || balanceChanged.Data.BalanceAfter != wantBalanceData.BalanceAfter ||
		balanceChanged.Data.WalletVersion != wantBalanceData.WalletVersion {
		t.Errorf("WalletBalanceChanged.data = %+v, want %+v", balanceChanged.Data, wantBalanceData)
	}

	processedEvents := queryOutboxEvents(t, ctx, h, opening.id)
	if len(processedEvents) != 1 || processedEvents[0].eventType != "WagerTransactionProcessed" || processedEvents[0].aggregateType != "WagerTransaction" {
		t.Fatalf("WagerTransactionProcessed outbox events for transaction %s = %+v, want exactly one", opening.id, processedEvents)
	}
	var processed wagerTransactionProcessedPayload
	requireNoError(t, json.Unmarshal(processedEvents[0].payload, &processed), "decode WagerTransactionProcessed payload")
	requireUTCEnvelope(t, processed.eventEnvelopeJSON, "WagerTransactionProcessed", opening.id, correlationID)
	wantProcessedData := struct {
		TransactionID string
		WalletID      string
		PlayerID      string
		Kind          string
		Origin        string
		Money         moneyJSON
		Status        string
	}{
		TransactionID: opening.id, WalletID: wallet.ID, PlayerID: playerID, Kind: "OPENING", Origin: "INTERNAL",
		Money: moneyJSON{Amount: "100.00", Currency: "BRL"}, Status: "PROCESSED",
	}
	if processed.Data.TransactionID != wantProcessedData.TransactionID || processed.Data.WalletID != wantProcessedData.WalletID ||
		processed.Data.PlayerID != wantProcessedData.PlayerID || processed.Data.Kind != wantProcessedData.Kind ||
		processed.Data.Origin != wantProcessedData.Origin || processed.Data.Money != wantProcessedData.Money ||
		processed.Data.Status != wantProcessedData.Status {
		t.Errorf("WagerTransactionProcessed.data = %+v, want %+v", processed.Data, wantProcessedData)
	}
	requireNoExternalMetadata(t, processedEvents[0].payload)
}

// TestOpenWallet_PositiveBalance_PublishesCommittedOutboxSnapshots exercises
// seam 3a end to end: the opening commit creates both durable snapshots, and
// the events-reader receives exactly those snapshots from the output FIFO.
func TestOpenWallet_PositiveBalance_PublishesCommittedOutboxSnapshots(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	resp, body := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "100.00", "BRL"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want 201, body = %s", resp.StatusCode, body)
	}
	wallet := decodeWalletResponse(t, body)
	opening, found := queryOpeningTransaction(t, ctx, h, wallet.ID)
	if !found {
		t.Fatal("want OPENING transaction before observing published events")
	}

	expected := map[string]string{
		"WalletBalanceChanged":      wallet.ID,
		"WagerTransactionProcessed": opening.id,
	}
	readerCredentials := loadTestCreds(t)
	reader := sqsClient(t, readerCredentials.eventsReaderKey, readerCredentials.eventsReaderSecret)
	queueURL := queueURL(outputQueueName())
	deadline := time.Now().Add(15 * time.Second)

	for len(expected) > 0 && time.Now().Before(deadline) {
		messages, err := reader.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
		})
		requireNoError(t, err, "receive published outbox events as events-reader")
		for _, message := range messages.Messages {
			if message.Body == nil {
				continue
			}
			var envelope eventEnvelopeJSON
			requireNoError(t, json.Unmarshal([]byte(*message.Body), &envelope), "decode published event envelope")
			wantAggregateID, wanted := expected[envelope.EventType]
			if !wanted || envelope.AggregateID != wantAggregateID {
				continue
			}

			var snapshot []byte
			requireNoError(t, h.pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE event_id = $1`, envelope.EventID).Scan(&snapshot), "read committed outbox snapshot")
			if got := *message.Body; got != string(snapshot) {
				t.Errorf("published event %s differs from committed snapshot\n got: %s\nwant: %s", envelope.EventID, got, snapshot)
			}
			if _, err := reader.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: message.ReceiptHandle}); err != nil {
				t.Fatalf("delete observed event %s: %v", envelope.EventID, err)
			}
			delete(expected, envelope.EventType)
		}
	}
	if len(expected) != 0 {
		t.Fatalf("events not received from wallet-events.fifo before deadline: %v", expected)
	}
}

func TestOpenWallet_ZeroBalance_CreatesOnlyTheWallet(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	resp, body := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "0.00", "BRL"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want 201, body = %s", resp.StatusCode, body)
	}

	wallet := decodeWalletResponse(t, body)
	if wallet.Balance.Amount != "0.00" || wallet.Version != 1 {
		t.Fatalf("unexpected wallet response: %+v", wallet)
	}

	balance, version, found := queryWalletRow(t, ctx, h, wallet.ID)
	if !found || balance != 0 || version != 1 {
		t.Fatalf("stored wallet = (balance %d, version %d, found %v), want (0, 1, true)", balance, version, found)
	}
	if _, found := queryOpeningTransaction(t, ctx, h, wallet.ID); found {
		t.Error("want no OPENING transaction for a zero-balance opening")
	}
	if got := countLedgerEntries(t, ctx, h, wallet.ID); got != 0 {
		t.Errorf("ledger entries = %d, want 0", got)
	}
	if events := queryOutboxEvents(t, ctx, h, wallet.ID); len(events) != 0 {
		t.Errorf("outbox events for wallet = %+v, want none", events)
	}
}

func TestOpenWallet_Duplicate_ReturnsConflict(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	first, firstBody := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "10.00", "BRL"))
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first POST /wallets status = %d, want 201, body = %s", first.StatusCode, firstBody)
	}

	second, secondBody := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "10.00", "BRL"))
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second POST /wallets status = %d, want 409, body = %s", second.StatusCode, secondBody)
	}
	errResp := decodeErrorResponse(t, secondBody)
	if errResp.Error.Code != "WALLET_ALREADY_EXISTS" {
		t.Errorf("error code = %q, want WALLET_ALREADY_EXISTS", errResp.Error.Code)
	}

	if got := countWalletsByPlayerAndCurrency(t, ctx, h, playerID, "BRL"); got != 1 {
		t.Errorf("wallets for player+currency = %d, want exactly 1", got)
	}
}

func TestOpenWallet_ConcurrentOpens_ProduceExactlyOneWalletAndOneInitialCredit(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	const attempts = 10
	statusCodes := make([]int, attempts)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			resp, _ := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "25.00", "BRL"))
			statusCodes[i] = resp.StatusCode
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	created, conflicts := 0, 0
	for _, code := range statusCodes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status code %d among concurrent opens", code)
		}
	}
	if created != 1 || conflicts != attempts-1 {
		t.Fatalf("created = %d, conflicts = %d, want exactly 1 created and %d conflicts", created, conflicts, attempts-1)
	}

	if got := countWalletsByPlayerAndCurrency(t, ctx, h, playerID, "BRL"); got != 1 {
		t.Fatalf("wallets for player+currency after concurrent opens = %d, want exactly 1", got)
	}

	var walletID string
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT id FROM wallets WHERE player_id = $1 AND currency = 'BRL'`, playerID).Scan(&walletID), "find the winning wallet")
	if got := countLedgerEntries(t, ctx, h, walletID); got != 1 {
		t.Errorf("ledger entries after concurrent opens = %d, want exactly 1", got)
	}
	balance, _, _ := queryWalletRow(t, ctx, h, walletID)
	if net := netLedgerBalance(t, ctx, h, walletID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d after concurrent opens", balance, net)
	}
}

func TestOpenWallet_InvalidInput_RejectsWithoutPersisting(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		body     []byte
		wantCode string
	}{
		{name: "malformed JSON", body: []byte(`{"playerId":`), wantCode: "INVALID_REQUEST"},
		{name: "unknown field", body: []byte(fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"10.00","currency":"BRL"},"extra":true}`, newUUID(t))), wantCode: "INVALID_REQUEST"},
		{name: "money out of format", body: openWalletBody(newUUID(t), "10.5", "BRL"), wantCode: "INVALID_MONEY"},
		{name: "negative amount", body: openWalletBody(newUUID(t), "-10.00", "BRL"), wantCode: "INVALID_MONEY"},
		{name: "unsupported currency", body: openWalletBody(newUUID(t), "10.00", "GBP"), wantCode: "UNSUPPORTED_CURRENCY"},
		{name: "missing initialBalance", body: []byte(fmt.Sprintf(`{"playerId":%q}`, newUUID(t))), wantCode: "INVALID_MONEY"},
		{name: "malformed playerId", body: openWalletBody("not-a-uuid", "10.00", "BRL"), wantCode: "INVALID_REQUEST"},
	}

	var before int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wallets`).Scan(&before), "count wallets before invalid requests")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := h.do(t, http.MethodPost, "/wallets", nil, tt.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
			}
			errResp := decodeErrorResponse(t, body)
			if errResp.Error.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", errResp.Error.Code, tt.wantCode)
			}
			if !errResp.Error.Correctable {
				t.Error("want correctable=true for every invalid-input rejection")
			}
		})
	}

	var after int
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM wallets`).Scan(&after), "count wallets after invalid requests")
	if after != before {
		t.Errorf("wallets count changed from %d to %d - invalid input must persist nothing", before, after)
	}
}

func TestGetWallet_Found(t *testing.T) {
	h := newAppHarness(t)
	playerID := newUUID(t)

	created, createdBody := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "42.50", "BRL"))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want 201, body = %s", created.StatusCode, createdBody)
	}
	wallet := decodeWalletResponse(t, createdBody)

	resp, body := h.do(t, http.MethodGet, "/wallets/"+wallet.ID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /wallets/{id} status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	got := decodeWalletResponse(t, body)
	if got.ID != wallet.ID || got.PlayerID != playerID || got.Balance.Amount != "42.50" || got.Version != 1 {
		t.Errorf("GET response = %+v, want it to match the wallet just opened", got)
	}
}

func TestGetWallet_NotFound(t *testing.T) {
	h := newAppHarness(t)

	resp, body := h.do(t, http.MethodGet, "/wallets/"+newUUID(t), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}

	respMalformed, bodyMalformed := h.do(t, http.MethodGet, "/wallets/not-a-uuid", nil, nil)
	if respMalformed.StatusCode != http.StatusNotFound {
		t.Fatalf("status for a malformed id = %d, want 404, body = %s", respMalformed.StatusCode, bodyMalformed)
	}
}

// TestOpenWallet_TransientDatabaseFailure_PersistsNothing forces a real
// lock_timeout by holding an ACCESS EXCLUSIVE lock on wallets from a
// separate connection while the app - configured with a short
// DATABASE_LOCK_TIMEOUT for this test only - tries to INSERT into it. The
// wait queues behind the lock and lock_timeout fires, giving the app a
// genuine Postgres error unrelated to any constraint this ticket already
// classifies as a conflict, which is exactly the "anything else is
// transient" case internal/walletapp.OpenWalletUseCase.Open falls back to.
func TestOpenWallet_TransientDatabaseFailure_PersistsNothing(t *testing.T) {
	t.Setenv("DATABASE_LOCK_TIMEOUT", "300ms")
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	lockConn := connectOwner(t, ctx)
	tx, err := lockConn.Begin(ctx)
	requireNoError(t, err, "begin locking transaction")
	_, err = tx.Exec(ctx, "LOCK TABLE wallets IN ACCESS EXCLUSIVE MODE")
	requireNoError(t, err, "lock wallets table")
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	resp, body := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("want a Retry-After header on 503")
	}
	errResp := decodeErrorResponse(t, body)
	if errResp.Error.Code != "TEMPORARILY_UNAVAILABLE" {
		t.Errorf("error code = %q, want TEMPORARILY_UNAVAILABLE", errResp.Error.Code)
	}

	requireNoError(t, tx.Rollback(ctx), "release the wallets lock")
	time.Sleep(50 * time.Millisecond)

	if got := countWalletsByPlayerAndCurrency(t, ctx, h, playerID, "BRL"); got != 0 {
		t.Errorf("wallets for player+currency after a transient failure = %d, want 0 - nothing should be persisted", got)
	}
}

// failingSecondOutboxUnitOfWork wraps a real walletapp.UnitOfWork - a real
// pgx transaction, opened by the real *pg.UnitOfWork - and substitutes only
// the OutboxRepository the callback sees, so the first outbox INSERT
// (WagerTransactionProcessed) really reaches Postgres inside that real
// transaction, and only the second one (WalletBalanceChanged) is forced to
// fail. No unit-of-work fake is involved: this is the simplest, most
// isolated way to reach "the second outbox INSERT fails after wallet,
// transaction and ledger are already written" without a fake transaction
// boundary or a temporary schema constraint.
type failingSecondOutboxUnitOfWork struct {
	real walletapp.UnitOfWork
}

func (f failingSecondOutboxUnitOfWork) WithinTx(ctx context.Context, fn func(context.Context, walletapp.Repositories) error) error {
	return f.real.WithinTx(ctx, func(ctx context.Context, repos walletapp.Repositories) error {
		calls := 0
		repos.Outbox = &failingSecondOutboxRepository{real: repos.Outbox, calls: &calls}
		return fn(ctx, repos)
	})
}

type failingSecondOutboxRepository struct {
	real  walletapp.OutboxRepository
	calls *int
}

func (f *failingSecondOutboxRepository) Insert(ctx context.Context, record walletapp.OutboxRecord) error {
	*f.calls++
	if *f.calls == 2 {
		return errors.New("forced outbox failure for the rollback test")
	}
	return f.real.Insert(ctx, record)
}

// TestOpenWallet_SecondOutboxInsertFails_RollsBackWalletTransactionLedgerAndOutbox
// proves the one scenario no other test in this package exercises: the
// second outbox INSERT (WalletBalanceChanged) failing after the wallet, the
// OPENING transaction, its ledger entry and the first outbox record
// (WagerTransactionProcessed) have already been written inside the same
// transaction. If any of those four survived a rollback, the row counts
// below would differ before and after this call.
func TestOpenWallet_SecondOutboxInsertFails_RollsBackWalletTransactionLedgerAndOutbox(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	countRows := func(table string) int {
		t.Helper()
		var count int
		requireNoError(t, h.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count), "count "+table)
		return count
	}
	walletsBefore, txBefore := countRows("wallets"), countRows("wager_transactions")
	ledgerBefore, outboxBefore := countRows("wallet_ledger_entries"), countRows("outbox_events")

	initialBalance, err := money.Parse("100.00", money.BRL)
	requireNoError(t, err, "parse initial balance")

	uow := failingSecondOutboxUnitOfWork{real: walletpg.NewUnitOfWork(pg.NewUnitOfWork(h.pool))}
	useCase := walletapp.NewOpenWalletUseCase(uow)

	_, openErr := useCase.Open(ctx, walletapp.OpenWalletInput{
		PlayerID: playerID, InitialBalance: initialBalance, CorrelationID: "corr-forced-outbox-failure",
	})
	if openErr == nil {
		t.Fatal("Open() error = nil, want the forced second outbox INSERT failure to propagate")
	}

	if got := countRows("wallets"); got != walletsBefore {
		t.Errorf("wallets count changed from %d to %d - the wallet insert must roll back with everything else", walletsBefore, got)
	}
	if got := countRows("wager_transactions"); got != txBefore {
		t.Errorf("wager_transactions count changed from %d to %d - the OPENING transaction must roll back", txBefore, got)
	}
	if got := countRows("wallet_ledger_entries"); got != ledgerBefore {
		t.Errorf("wallet_ledger_entries count changed from %d to %d - the ledger entry must roll back", ledgerBefore, got)
	}
	if got := countRows("outbox_events"); got != outboxBefore {
		t.Errorf("outbox_events count changed from %d to %d - even the first, successfully inserted outbox record must roll back with the rest of the transaction", outboxBefore, got)
	}
	if got := countWalletsByPlayerAndCurrency(t, ctx, h, playerID, "BRL"); got != 0 {
		t.Errorf("wallets for player+currency after rollback = %d, want 0", got)
	}
}
