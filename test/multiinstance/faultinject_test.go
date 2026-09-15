//go:build multiinstance

package multiinstance

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

// This file is seam 3b's own fault-injection suite (spec, "Injeção de
// falhas e ambiente" and ticket 16): the binary under test here is always
// compiled with -race -tags faultinject (buildBinary), so every instance
// below can be handed FAULT_INJECT_POINT and made to SIGKILL itself at one
// of the five named points instrumented for this ticket - internal/pg
// (before-commit), internal/consumer (before-commit,
// after-commit-before-delete, after-pending-reference-commit),
// internal/walletapp (after-pending-reference-commit) and internal/outbox
// (after-outbox-claim-before-send, after-outbox-send-before-confirm). Every
// scenario names a "victim" instance carrying the fault point and a
// "rescuer" instance, started only after waitExit confirms the victim
// actually died, that completes the interrupted work - never a mocked
// crash, always a real process killed mid-flight against the same Postgres,
// MiniStack and Keycloak the healthy instances use.

// TestMultiInstance_ConsumerDiesAfterCommitBeforeDelete_RedeliveredAndAcknowledgedOnce
// is the ticket's own first scenario: the consumer commits the inbox row and
// the wallet debit, then is killed before it can delete the SQS message.
// Once its short visibility timeout expires, another instance receives the
// redelivery, recognises it by the inbox's messageId and deletes it without
// a second debit - the sqs_consumer_duplicate_messages_total counter is the
// independent proof the redelivery actually reached the application.
func TestMultiInstance_ConsumerDiesAfterCommitBeforeDelete_RedeliveredAndAcknowledgedOnce(t *testing.T) {
	binary := buildBinary(t)
	logDir := t.TempDir()
	ctx := context.Background()
	admin := adminToken(t)
	conn := connectApp(t, ctx)
	creds := loadSQSTestCreds(t)
	gateway := newSQSClient(t, creds.gatewayKey, creds.gatewaySecret)

	// Drains any message an earlier run of these tests left visible on the
	// shared input queue first - otherwise the victim's own consumer could
	// receive and commit that stray message before it ever reaches the one
	// this scenario sends, and die on it instead (observed running this
	// suite with `-count=3`).
	drainBacklog(t, binary, logDir, true)
	victim := startWithEnv(t, binary, logDir, "victim", map[string]string{
		"SQS_CONSUMER_ENABLED":            "true",
		"SQS_CONSUMER_POLL_WAIT":          "1s",
		"SQS_CONSUMER_CONCURRENCY":        "1",
		"SQS_CONSUMER_VISIBILITY_TIMEOUT": "3s",
		"SQS_CONSUMER_PROCESSING_TIMEOUT": "1s",
		"FAULT_INJECT_POINT":              "after-commit-before-delete",
	})

	wallet := openWalletAt(t, victim, admin, newUUID(t), "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, conn, wallet.ID)

	externalID := uniqueID("ext")
	in := wageringBodyInput{ProviderID: "provider-a", ExternalID: externalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}
	sendWagerMessage(t, gateway, wallet.ID, "delivery-1-"+uniqueID("dedup"), sqsWagerEnvelope(t, uniqueID("fault-after-commit"), in, "idem-"+uniqueID("key")))

	waitExit(t, victim, 15*time.Second)

	waitForWalletBalance(t, ctx, conn, wallet.ID, 7000, 10*time.Second)
	if _, status, found := findTransactionByExternalID(t, ctx, conn, externalID); !found || status != "PROCESSED" {
		t.Fatalf("transaction after crash = %q (found %v), want PROCESSED - the commit already happened before the kill", status, found)
	}

	rescuer := startWithEnv(t, binary, logDir, "rescuer", map[string]string{"SQS_CONSUMER_ENABLED": "true", "SQS_CONSUMER_POLL_WAIT": "1s"})
	waitForSQSConsumerDuplicateMessagesMetric(t, rescuer, 1, 15*time.Second)

	if got := countLedgerEntries(t, ctx, conn, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries after redelivery = %d, want %d (a single debit)", got, ledgerBaseline+1)
	}
	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 7000 {
		t.Errorf("stored balance after redelivery = %d, want 7000", balance)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_ConsumerDiesBeforeCommit_RedeliveryProcessesOnce is the
// ticket's second scenario: the consumer is killed before its transaction
// ever commits, so Postgres rolls back the dead connection's work and
// nothing - not even the inbox row - survives. The redelivery therefore
// looks like a first delivery to whichever instance receives it, and must
// still produce exactly one debit.
func TestMultiInstance_ConsumerDiesBeforeCommit_RedeliveryProcessesOnce(t *testing.T) {
	binary := buildBinary(t)
	logDir := t.TempDir()
	ctx := context.Background()
	admin := adminToken(t)
	conn := connectApp(t, ctx)
	creds := loadSQSTestCreds(t)
	gateway := newSQSClient(t, creds.gatewayKey, creds.gatewaySecret)

	// "before-commit" is the same point pg.UnitOfWork.Execute fires for
	// every committing transaction, including opening a wallet - the wallet
	// is therefore opened through a separate, un-injected instance, so the
	// victim's very first commit attempt is the SQS BET this scenario is
	// actually about. drainBacklog runs its own SQS consumer first and is
	// stopped before that instance starts, so the input queue is empty and
	// handed over to the victim exclusively.
	drainBacklog(t, binary, logDir, true)
	setup := startWithEnv(t, binary, logDir, "setup", nil)
	wallet := openWalletAt(t, setup, admin, newUUID(t), "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, conn, wallet.ID)
	stopInstance(t, setup)

	victim := startWithEnv(t, binary, logDir, "victim", map[string]string{
		"SQS_CONSUMER_ENABLED":            "true",
		"SQS_CONSUMER_POLL_WAIT":          "1s",
		"SQS_CONSUMER_CONCURRENCY":        "1",
		"SQS_CONSUMER_VISIBILITY_TIMEOUT": "3s",
		"SQS_CONSUMER_PROCESSING_TIMEOUT": "1s",
		"FAULT_INJECT_POINT":              "before-commit",
		// "before-commit" also fires for the pending-reference worker's own
		// resume commits (internal/referenceworker, via the same
		// pg.UnitOfWork.Execute): disabled here so a leftover pending row
		// from an unrelated test can never race this scenario's own SQS
		// commit for the kill.
		"REFERENCE_WORKER_ENABLED": "false",
	})

	externalID := uniqueID("ext")
	in := wageringBodyInput{ProviderID: "provider-a", ExternalID: externalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}
	messageID := uniqueID("fault-before-commit")
	sendWagerMessage(t, gateway, wallet.ID, "delivery-1-"+uniqueID("dedup"), sqsWagerEnvelope(t, messageID, in, "idem-"+uniqueID("key")))

	waitExit(t, victim, 15*time.Second)

	// The kill happened before the transaction that would have inserted
	// both the inbox row and the wager transaction ever committed - an
	// uncommitted row is never visible to another connection regardless of
	// how quickly Postgres notices the dead backend, so this is a direct
	// assertion, not a race against cleanup.
	if _, _, found := findTransactionByExternalID(t, ctx, conn, externalID); found {
		t.Fatalf("wager transaction %s exists after a pre-commit kill, want nothing persisted", externalID)
	}
	var inboxRows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE consumer_name = 'wager-transactions' AND message_id = $1`, messageID).Scan(&inboxRows); err != nil {
		t.Fatalf("count inbox rows: %v", err)
	}
	if inboxRows != 0 {
		t.Fatalf("inbox rows for message %s = %d after a pre-commit kill, want 0", messageID, inboxRows)
	}

	startWithEnv(t, binary, logDir, "rescuer", map[string]string{"SQS_CONSUMER_ENABLED": "true", "SQS_CONSUMER_POLL_WAIT": "1s"})
	waitForWalletBalance(t, ctx, conn, wallet.ID, 7000, 15*time.Second)

	if _, status, found := findTransactionByExternalID(t, ctx, conn, externalID); !found || status != "PROCESSED" {
		t.Fatalf("transaction after redelivery = %q (found %v), want PROCESSED", status, found)
	}
	if got := countLedgerEntries(t, ctx, conn, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries after redelivery = %d, want %d (a single debit)", got, ledgerBaseline+1)
	}
	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 7000 {
		t.Errorf("stored balance after redelivery = %d, want 7000", balance)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_OutboxPublisherDiesAfterSendBeforeConfirm_AnotherInstanceRepublishesSameEventID
// is the ticket's third scenario: opening a wallet commits two outbox
// records; the victim's own publisher claims them and is killed right after
// the first Send succeeds, before it can mark that record published. A
// rescuer started afterward claims the expired lease on both records and
// republishes - the already-sent one under the very same eventId, the other
// for the first time - and no confirmed event is ever lost.
func TestMultiInstance_OutboxPublisherDiesAfterSendBeforeConfirm_AnotherInstanceRepublishesSameEventID(t *testing.T) {
	binary := buildBinary(t)
	logDir := t.TempDir()
	ctx := context.Background()
	admin := adminToken(t)
	conn := connectApp(t, ctx)

	drainBacklog(t, binary, logDir, false)
	victim := startWithEnv(t, binary, logDir, "victim", map[string]string{
		"OUTBOX_LEASE":       "2s",
		"FAULT_INJECT_POINT": "after-outbox-send-before-confirm",
	})

	wallet := openWalletAt(t, victim, admin, newUUID(t), "100.00")
	waitExit(t, victim, 15*time.Second)

	want := openingOutboxEventIDs(t, ctx, conn, wallet.ID)
	if len(want) == 0 {
		t.Fatal("no outbox events found for the opened wallet")
	}

	// Between the victim's confirmed death and the rescuer's own start, the
	// output queue must show exactly the one message the victim itself
	// managed to send before it crashed - proof the Send genuinely reached
	// the wire, not merely that published_at was left unset (review,
	// correctness: a published_at-only check would pass identically if the
	// crash point were moved to before Send, which never sends anything).
	reader := newOutputReader(t)
	if n := reader.drain(t, time.Now().Add(10*time.Second), want); n != 1 {
		t.Fatalf("output queue delivered %d message(s) among the opened wallet's events before the rescuer started, want exactly 1 (the victim's own send)", n)
	}
	var firstEventID string
	for eventID, count := range reader.rawDeliveries {
		if count > 0 {
			firstEventID = eventID
		}
	}
	if !want[firstEventID] {
		t.Fatalf("the message sent before the crash carried eventId %q, want one of %v", firstEventID, want)
	}

	// rescueStart is Postgres's own clock, not the test host's, so the
	// confirmation-timing proof below never depends on the two being in
	// sync.
	rescueStart := pgNow(t, ctx, conn)
	startWithEnv(t, binary, logDir, "rescuer", nil)

	for eventID := range want {
		waitForOutboxPublished(t, ctx, conn, eventID, 15*time.Second)
	}
	// The victim crashes between Send and confirm on the very first record
	// it ever touches - faultinject.Trigger fires unconditionally there, so
	// it can never reach MarkPublished for anything. Every confirmation
	// observed here therefore happened strictly after rescueStart, which
	// only the surviving rescuer could have produced (review, correctness:
	// DB-side proof of republication, independent of the queue).
	for eventID := range want {
		if !outboxPublishedAfter(t, ctx, conn, eventID, rescueStart) {
			t.Errorf("event %s was confirmed published before the rescuer started, want confirmation strictly after - only the rescuer, not the dead victim, could have completed it", eventID)
		}
	}

	reader.drain(t, time.Now().Add(20*time.Second), want)
	for eventID := range want {
		if !reader.applied[eventID] {
			t.Errorf("event %s never reached the output queue after the publisher crash", eventID)
		}
	}
	// The reader deduplicates by eventId (outputReader.applied), so every
	// eventId is treated as processed exactly once above regardless of how
	// many raw deliveries follow. What differs by outcome is only how many
	// raw deliveries the *queue* itself carried for the republished event.
	switch n := reader.rawDeliveries[firstEventID]; n {
	case 1:
		t.Logf("event %s (sent once by the victim) was never redelivered on the wire - MiniStack's FIFO five-minute deduplication window (spec, \"Outbox e eventos\") suppressed the rescuer's republish; the republication proof for this run is the DB-side outboxPublishedAfter check above, not the queue", firstEventID)
	case 2:
		if !bytes.Equal(reader.payloads[firstEventID][0], reader.payloads[firstEventID][1]) {
			t.Errorf("event %s was redelivered with a different payload across its two deliveries, want the identical committed payload both times", firstEventID)
		}
	default:
		t.Errorf("event %s was delivered %d times, want 1 (FIFO dedup suppressed the republish) or 2 (both deliveries reached the wire)", firstEventID, n)
	}
	for eventID := range want {
		if eventID == firstEventID {
			continue
		}
		if got := reader.rawDeliveries[eventID]; got != 1 {
			t.Errorf("event %s (never sent before the crash) was delivered %d times, want exactly 1", eventID, got)
		}
	}

	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_OutboxPublisherDiesAfterClaimBeforeSend_LeaseExpiresAndAnotherInstancePublishes
// is the ticket's fourth scenario: the victim claims the outbox batch -
// pushing its lease - and is killed before ever calling Send. No message
// ever reaches SQS from the victim; once the short lease it claimed with
// expires, a rescuer claims the same records and is the one that actually
// publishes them.
func TestMultiInstance_OutboxPublisherDiesAfterClaimBeforeSend_LeaseExpiresAndAnotherInstancePublishes(t *testing.T) {
	binary := buildBinary(t)
	logDir := t.TempDir()
	ctx := context.Background()
	admin := adminToken(t)
	conn := connectApp(t, ctx)

	drainBacklog(t, binary, logDir, false)
	victim := startWithEnv(t, binary, logDir, "victim", map[string]string{
		"OUTBOX_LEASE":       "2s",
		"FAULT_INJECT_POINT": "after-outbox-claim-before-send",
	})

	wallet := openWalletAt(t, victim, admin, newUUID(t), "100.00")
	waitExit(t, victim, 15*time.Second)

	want := openingOutboxEventIDs(t, ctx, conn, wallet.ID)
	if len(want) == 0 {
		t.Fatal("no outbox events found for the opened wallet")
	}
	for eventID := range want {
		if outboxPublished(t, ctx, conn, eventID) {
			t.Fatalf("event %s already published before the victim ever sent anything", eventID)
		}
	}
	// published_at alone only proves nothing was *confirmed* - that would
	// look identical if the trigger fired after a real Send instead of
	// before it (review, correctness). Peeking the output queue between the
	// victim's confirmed death and the rescuer's start proves nothing was
	// even attempted.
	requireNoOutputEvents(t, 5*time.Second, want)

	startWithEnv(t, binary, logDir, "rescuer", nil)

	for eventID := range want {
		waitForOutboxPublished(t, ctx, conn, eventID, 15*time.Second)
	}
	seen := drainOutputEvents(t, time.Now().Add(20*time.Second), want)
	for eventID := range want {
		if seen[eventID] < 1 {
			t.Errorf("event %s never reached the output queue after the lease expired", eventID)
		}
	}

	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_DiesAfterPendingReferenceCommit_AnotherInstanceWorkerCompletes
// is the ticket's fifth scenario: a REFUND referencing a BET that has not
// arrived yet is durably committed as PENDING_REFERENCE, and the victim is
// killed right after that commit, before it can answer the request. Another
// instance later receives the referenced BET, and that instance's own
// pending-reference worker resolves the credit.
func TestMultiInstance_DiesAfterPendingReferenceCommit_AnotherInstanceWorkerCompletes(t *testing.T) {
	binary := buildBinary(t)
	logDir := t.TempDir()
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)

	victim := startWithEnv(t, binary, logDir, "victim", map[string]string{"FAULT_INJECT_POINT": "after-pending-reference-commit"})

	wallet := openWalletAt(t, victim, admin, newUUID(t), "100.00")
	round := uniqueID("round")
	betExternalID := uniqueID("bet")
	refundExternalID := uniqueID("refund")
	ref := betExternalID
	refund := wageringBodyInput{ProviderID: "provider-a", ExternalID: refundExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "REFUND", Amount: "30.00", Currency: testCurrency, ReferenceID: &ref}
	doWageringExpectingCrash(t, victim, provider, refund, "idem-"+uniqueID("key"))

	waitExit(t, victim, 15*time.Second)

	pendingID, status, found := findTransactionByExternalID(t, ctx, conn, refundExternalID)
	if !found || status != "PENDING_REFERENCE" {
		t.Fatalf("REFUND after crash = %q (found %v), want PENDING_REFERENCE - the commit already happened before the kill", status, found)
	}

	rescuer := startWithEnv(t, binary, logDir, "rescuer", map[string]string{"REFERENCE_WORKER_POLL_INTERVAL": "100ms"})
	bet := wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}
	betResp, betBody := doWageringAt(t, rescuer, provider, bet, "idem-"+uniqueID("key"))
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("BET status = %d, want 200, body = %s", betResp.StatusCode, betBody)
	}

	resolvedID := waitForTransactionStatus(t, ctx, conn, refundExternalID, "PROCESSED", 40*time.Second)
	if resolvedID != pendingID {
		t.Errorf("resolved transaction id = %s, want %s (the same row the victim committed)", resolvedID, pendingID)
	}

	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 10000 {
		t.Errorf("stored balance = %d, want 10000 (BET debited then REFUND credited back)", balance)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_RefundBeforeBet_SurvivesAllInstancesRestarted is the
// ticket's sixth scenario: a REFUND arrives before its BET and is accepted
// as PENDING_REFERENCE, every instance is then killed and a fresh trio
// started in its place (the harness's own restartTrio - a plain,
// undifferentiated crash of all three at once, not a single named fault
// point), and only then does the BET arrive. The pendency has to survive
// losing every instance's in-memory state and still resolve once its
// reference exists.
func TestMultiInstance_RefundBeforeBet_SurvivesAllInstancesRestarted(t *testing.T) {
	instances := startTrio(t)
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)

	wallet := openWalletAt(t, instances[0], admin, newUUID(t), "100.00")
	round := uniqueID("round")
	betExternalID := uniqueID("bet")
	refundExternalID := uniqueID("refund")
	ref := betExternalID
	refund := wageringBodyInput{ProviderID: "provider-a", ExternalID: refundExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "REFUND", Amount: "30.00", Currency: testCurrency, ReferenceID: &ref}
	refundResp, refundBody := doWageringAt(t, instances[1], provider, refund, "idem-"+uniqueID("key"))
	if refundResp.StatusCode != http.StatusAccepted {
		t.Fatalf("REFUND before BET status = %d, want 202, body = %s", refundResp.StatusCode, refundBody)
	}
	pendingID, status, found := findTransactionByExternalID(t, ctx, conn, refundExternalID)
	if !found || status != "PENDING_REFERENCE" {
		t.Fatalf("REFUND before restart = %q (found %v), want PENDING_REFERENCE", status, found)
	}

	restarted := restartTrio(t, instances)

	// The pendency is read straight from Postgres, independent of any
	// instance's memory, so it must still be exactly what was committed
	// before the restart.
	if _, status, found := findTransactionByExternalID(t, ctx, conn, refundExternalID); !found || status != "PENDING_REFERENCE" {
		t.Fatalf("REFUND after restart = %q (found %v), want PENDING_REFERENCE to survive", status, found)
	}

	bet := wageringBodyInput{ProviderID: "provider-a", ExternalID: betExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}
	betResp, betBody := doWageringAt(t, restarted[2], provider, bet, "idem-"+uniqueID("key"))
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("BET after restart status = %d, want 200, body = %s", betResp.StatusCode, betBody)
	}

	resolvedID := waitForTransactionStatus(t, ctx, conn, refundExternalID, "PROCESSED", 40*time.Second)
	if resolvedID != pendingID {
		t.Errorf("resolved transaction id = %s, want %s (the same pendency created before the restart)", resolvedID, pendingID)
	}
	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 10000 {
		t.Errorf("stored balance = %d, want 10000 (BET debited then REFUND credited back)", balance)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_CrossingHTTPAndSQS_ConsumerDiesMidway_SameOperationSettlesOnce
// is the ticket's seventh scenario: the exact same BET is first submitted
// over SQS to a victim killed after commit but before it can delete the
// message, then replayed over plain HTTP against a different, healthy
// instance while the original message is still invisible, and finally
// redelivered over SQS once more to a third instance after the visibility
// timeout expires. Every one of those three paths - the crashed SQS
// delivery, the HTTP replay and the SQS redelivery - has to observe the
// exact same settled operation, with only one debit ever applied.
func TestMultiInstance_CrossingHTTPAndSQS_ConsumerDiesMidway_SameOperationSettlesOnce(t *testing.T) {
	binary := buildBinary(t)
	logDir := t.TempDir()
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)
	creds := loadSQSTestCreds(t)
	gateway := newSQSClient(t, creds.gatewayKey, creds.gatewaySecret)

	// Drains any message an earlier run of these tests left visible on the
	// shared input queue first - see the first scenario's own comment above.
	drainBacklog(t, binary, logDir, true)
	victim := startWithEnv(t, binary, logDir, "victim", map[string]string{
		"SQS_CONSUMER_ENABLED":            "true",
		"SQS_CONSUMER_POLL_WAIT":          "1s",
		"SQS_CONSUMER_CONCURRENCY":        "1",
		"SQS_CONSUMER_VISIBILITY_TIMEOUT": "3s",
		"SQS_CONSUMER_PROCESSING_TIMEOUT": "1s",
		"FAULT_INJECT_POINT":              "after-commit-before-delete",
	})

	wallet := openWalletAt(t, victim, admin, newUUID(t), "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, conn, wallet.ID)

	externalID := uniqueID("ext")
	key := "idem-" + uniqueID("key")
	in := wageringBodyInput{ProviderID: "provider-a", ExternalID: externalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency}
	sendWagerMessage(t, gateway, wallet.ID, "delivery-1-"+uniqueID("dedup"), sqsWagerEnvelope(t, uniqueID("fault-cross"), in, key))

	waitExit(t, victim, 15*time.Second)
	waitForWalletBalance(t, ctx, conn, wallet.ID, 7000, 10*time.Second)
	original, _, found := findTransactionByExternalID(t, ctx, conn, externalID)
	if !found {
		t.Fatal("transaction not found after the SQS commit, before the HTTP replay")
	}

	httpReplayInstance := startWithEnv(t, binary, logDir, "http-replay", nil)
	replayResp, replayBody := doWageringAt(t, httpReplayInstance, provider, in, key)
	if replayResp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP replay of the crashed SQS operation status = %d, want 200, body = %s", replayResp.StatusCode, replayBody)
	}
	replay := decodeWageringResponse(t, replayBody)
	if !replay.IdempotentReplay || replay.TransactionID != original {
		t.Errorf("HTTP replay = %+v, want idempotentReplay=true and transactionId=%s", replay, original)
	}

	sqsRescuer := startWithEnv(t, binary, logDir, "sqs-rescuer", map[string]string{"SQS_CONSUMER_ENABLED": "true", "SQS_CONSUMER_POLL_WAIT": "1s"})
	waitForSQSConsumerDuplicateMessagesMetric(t, sqsRescuer, 1, 15*time.Second)

	if got := countLedgerEntries(t, ctx, conn, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries = %d, want %d (a single debit across SQS crash, HTTP replay and SQS redelivery)", got, ledgerBaseline+1)
	}
	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 7000 {
		t.Errorf("stored balance = %d, want 7000", balance)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_TwoPublishersDisputeSameOutboxConcurrently_BothInstancesPublishPartOfTheBatch
// is the re-review's own correction to the earlier version of this scenario
// (correctness, spec): giving the peers a 5s poll interval against the
// victim's default 250ms one let the victim's own next tick win the fresh
// batch deterministically before either peer's next tick could even fire,
// so the peers only ever raced each other over whatever the victim's lease
// released afterward - proof of authorship, not of overlap. A publisher
// serialized by timing would still have passed every assertion below it.
//
// Here every instance - peer-b, peer-c and the victim alike - polls every
// 100ms, and the batch (20 wallets, 40 outbox rows) is large enough that no
// single poll tick can drain it: reaching zero pending needs several rounds
// of FOR UPDATE SKIP LOCKED claims from whichever instance's tick fires
// next, so peer-b and peer-c end up genuinely racing each other for slices
// of the same still-open batch, not for disjoint ones handed out in turn.
// The proof of that overlap never uses this test process's own wall clock:
// instancePublishedSpan attributes each confirmed eventId to the instance
// whose own log recorded publishing it, then reads that eventId's
// published_at - set by MarkPublished's `now()`, Postgres's own clock -
// to build each instance's [first confirmation, last confirmation] span.
// Two serialized publishers would produce two disjoint spans; this test
// requires peer-b's and peer-c's spans to overlap.
//
// The victim's fault point still kills it right after its own first claim,
// on the first record, abandoning that slice under a short lease for
// whichever peer claims it next - the victim itself never confirms
// anything, so it is deliberately absent from the overlap proof.
func TestMultiInstance_TwoPublishersDisputeSameOutboxConcurrently_BothInstancesPublishPartOfTheBatch(t *testing.T) {
	binary := buildBinary(t)
	logDir := t.TempDir()
	ctx := context.Background()
	admin := adminToken(t)
	conn := connectApp(t, ctx)

	drainBacklog(t, binary, logDir, false)

	overrides := map[string]string{"OUTBOX_POLL_INTERVAL": "100ms", "OUTBOX_BATCH_SIZE": "3"}
	peerB := startWithEnv(t, binary, logDir, "peer-b", overrides)
	peerC := startWithEnv(t, binary, logDir, "peer-c", overrides)
	// The victim polls exactly as fast as the peers - a real contender for
	// the same batch, not a head start - and carries its own short lease so
	// whatever it claims and abandons on crash comes back up for grabs
	// quickly.
	victim := startWithEnv(t, binary, logDir, "victim", map[string]string{
		"OUTBOX_POLL_INTERVAL": "100ms",
		"OUTBOX_BATCH_SIZE":    "3",
		"OUTBOX_LEASE":         "800ms",
		"FAULT_INJECT_POINT":   "after-outbox-claim-before-send",
	})

	const walletCount = 20
	want := map[string]bool{}
	var walletIDs []string
	for i := 0; i < walletCount; i++ {
		wallet := openWalletAt(t, peerB, admin, newUUID(t), "100.00")
		walletIDs = append(walletIDs, wallet.ID)
		for eventID := range openingOutboxEventIDs(t, ctx, conn, wallet.ID) {
			want[eventID] = true
		}
	}
	if len(want) != walletCount*2 {
		t.Fatalf("outbox events for %d wallets = %d, want %d", walletCount, len(want), walletCount*2)
	}

	waitExit(t, victim, 15*time.Second)

	for eventID := range want {
		waitForOutboxPublished(t, ctx, conn, eventID, 30*time.Second)
	}

	// Every eventId in the batch is confirmed and reaches the reader exactly
	// applied once - none confirmed is ever lost, and none is double-applied
	// even though the two peers claimed slices independently.
	seen := drainOutputEvents(t, time.Now().Add(20*time.Second), want)
	for eventID := range want {
		if seen[eventID] < 1 {
			t.Errorf("event %s never reached the output queue", eventID)
		}
	}

	// The victim can never contribute a confirmed publish - faultinject
	// kills it between claim and send, before it ever calls MarkPublished on
	// anything - so "both instances published" is evidence that the two
	// *peers* each independently confirmed part of the batch, not that the
	// victim did. A per-process Prometheus counter is exactly that: each
	// instance's own outbox_published_total reflects only what its own
	// publisher confirmed.
	waitForMetricAtLeast(t, peerB, "outbox_published_total", 1, 5*time.Second)
	waitForMetricAtLeast(t, peerC, "outbox_published_total", 1, 5*time.Second)
	publishedByB := bareMetricValue(t, peerB, "outbox_published_total")
	publishedByC := bareMetricValue(t, peerC, "outbox_published_total")
	if publishedByB < 1 || publishedByC < 1 {
		t.Errorf("outbox_published_total peer-b=%.0f peer-c=%.0f, want both >= 1 (a genuine dispute, not one instance publishing everything)", publishedByB, publishedByC)
	}
	if total := publishedByB + publishedByC; total != float64(len(want)) {
		t.Errorf("outbox_published_total peer-b=%.0f + peer-c=%.0f = %.0f, want %d (every event published exactly once between the two survivors)", publishedByB, publishedByC, total, len(want))
	}

	// The re-review's own correction: proof of temporal overlap, not just of
	// authorship after the fact. A publisher serialized by timing produces
	// two disjoint spans and fails this assertion.
	bSpan := instancePublishedSpan(t, ctx, conn, peerB, want)
	cSpan := instancePublishedSpan(t, ctx, conn, peerC, want)
	if !bSpan.overlaps(cSpan) {
		t.Errorf("peer-b published %s..%s and peer-c published %s..%s, want overlapping spans (a genuine concurrent dispute, not two serialized publishers)",
			bSpan.start, bSpan.end, cSpan.start, cSpan.end)
	}

	for _, walletID := range walletIDs {
		balance := queryWalletBalance(t, ctx, conn, walletID)
		if net := netLedgerBalance(t, ctx, conn, walletID); net != balance {
			t.Errorf("wallet %s: stored balance %d does not match ledger net %d", walletID, balance, net)
		}
	}
}

// TestMultiInstance_PendingReferenceExpiresAfterAllInstancesRestart_RejectedWithReferenceNotFound
// is the review's own addition to the sixth scenario (spec): the existing
// restart scenario only ever proves resolution (the referenced BET
// eventually arrives). This one proves the other half the ticket names -
// expiration "conforme a configuração" - survives the same full restart:
// a REFUND is accepted as PENDING_REFERENCE, every instance is killed and a
// fresh trio started in its place, and its BET never arrives at all. A
// short REFERENCE_WORKER_MAX_ATTEMPTS (durable in wager_transactions.attempts,
// so the restart cannot reset it) is what makes the pendency expire quickly
// on either side of the restart, exactly the way the ticket's own attempt
// -exhaustion alternative (as opposed to TTL) already works at seam 3a.
func TestMultiInstance_PendingReferenceExpiresAfterAllInstancesRestart_RejectedWithReferenceNotFound(t *testing.T) {
	overrides := map[string]string{"REFERENCE_WORKER_MAX_ATTEMPTS": "2"}
	instances := startTrioWithEnv(t, overrides)
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)

	wallet := openWalletAt(t, instances[0], admin, newUUID(t), "100.00")
	round := uniqueID("round")
	betExternalID := uniqueID("bet")
	refundExternalID := uniqueID("refund")
	ref := betExternalID
	refund := wageringBodyInput{ProviderID: "provider-a", ExternalID: refundExternalID, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: round, GameID: "game-1", Kind: "REFUND", Amount: "30.00", Currency: testCurrency, ReferenceID: &ref}
	refundResp, refundBody := doWageringAt(t, instances[1], provider, refund, "idem-"+uniqueID("key"))
	if refundResp.StatusCode != http.StatusAccepted {
		t.Fatalf("REFUND before BET status = %d, want 202, body = %s", refundResp.StatusCode, refundBody)
	}
	pendingID, status, found := findTransactionByExternalID(t, ctx, conn, refundExternalID)
	if !found || status != "PENDING_REFERENCE" {
		t.Fatalf("REFUND before restart = %q (found %v), want PENDING_REFERENCE", status, found)
	}

	restartTrioWithEnv(t, instances, overrides)

	// The BET referenced by betExternalID is deliberately never sent: this
	// scenario is about expiration, not resolution (the sixth scenario
	// already covers resolution surviving the same kind of restart).
	resolvedID := waitForTransactionStatus(t, ctx, conn, refundExternalID, "REJECTED", 20*time.Second)
	if resolvedID != pendingID {
		t.Errorf("resolved transaction id = %s, want %s (the same pendency created before the restart)", resolvedID, pendingID)
	}
	if code := transactionFailureCode(t, ctx, conn, pendingID); code == nil || *code != "REFERENCE_NOT_FOUND" {
		t.Errorf("failure code = %v, want REFERENCE_NOT_FOUND", code)
	}
	types := outboxEventTypesFor(t, ctx, conn, pendingID)
	if len(types) == 0 || types[len(types)-1] != "WagerTransactionRejected" {
		t.Errorf("outbox event types for the expired pendency = %v, want the last one to be WagerTransactionRejected", types)
	}

	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 10000 {
		t.Errorf("stored balance = %d, want 10000 (the REFUND never applied)", balance)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}
