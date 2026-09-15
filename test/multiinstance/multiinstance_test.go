//go:build multiinstance

package multiinstance

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// Every scenario in this file is seam 3b (spec, "Seams acordados"): the
// same financial guarantees seam 3a already proves in one fxtest process,
// now proved across three independent wallet-service processes - separate
// memory, separate connections - disputing the same wallets over the same
// Postgres, MiniStack and Keycloak. The database is read only for
// assertions, never to drive the scenario itself (spec, seam 3: "O banco é
// lido só para asserções").

func adminToken(t *testing.T) string     { t.Helper(); return fetchToken(t, walletServiceClient()) }
func providerAToken(t *testing.T) string { t.Helper(); return fetchToken(t, providerAClient()) }

// TestMultiInstance_TwoBetsExceedingBalance_OneOnEachInstance is the
// ticket's own acceptance scenario: 80.00 + 80.00 over a 100.00 wallet,
// each BET submitted to a different instance, must settle to exactly one
// PROCESSED, one INSUFFICIENT_FUNDS, balance "20.00" and a single debit -
// the FOR UPDATE row lock on the wallet has to serialize the two instances
// exactly as it would two goroutines in one process.
func TestMultiInstance_TwoBetsExceedingBalance_OneOnEachInstance(t *testing.T) {
	instances := startTrio(t)
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)

	wallet := openWalletAt(t, instances[0], admin, newUUID(t), "100.00")
	ledgerBaseline := countLedgerEntries(t, ctx, conn, wallet.ID) // the OPENING credit

	inputs := [2]wageringBodyInput{
		{ProviderID: "provider-a", ExternalID: uniqueID("ext-1"), PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "80.00", Currency: testCurrency},
		{ProviderID: "provider-a", ExternalID: uniqueID("ext-2"), PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "80.00", Currency: testCurrency},
	}
	keys := [2]string{"idem-" + uniqueID("k0"), "idem-" + uniqueID("k1")}
	targets := [2]*instance{instances[0], instances[1]}

	statusCodes := make([]int, 2)
	results := make([]wageringHTTPResponse, 2)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			resp, body := doWageringAt(t, targets[i], provider, inputs[i], keys[i])
			statusCodes[i] = resp.StatusCode
			results[i] = decodeWageringResponse(t, body)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	processed, rejected := 0, 0
	for i, code := range statusCodes {
		switch code {
		case http.StatusOK:
			processed++
			if results[i].Status != "PROCESSED" {
				t.Errorf("attempt %d (instance %s): status 200 but body status = %q", i, targets[i].name, results[i].Status)
			}
		case http.StatusUnprocessableEntity:
			rejected++
			if results[i].FailureCode != "INSUFFICIENT_FUNDS" {
				t.Errorf("attempt %d (instance %s): 422 but failureCode = %q, want INSUFFICIENT_FUNDS", i, targets[i].name, results[i].FailureCode)
			}
		default:
			t.Errorf("attempt %d (instance %s): unexpected status code %d", i, targets[i].name, code)
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed = %d, rejected = %d, want exactly 1 and 1", processed, rejected)
	}

	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 2000 {
		t.Fatalf("stored wallet balance = %d, want 2000 (\"20.00\")", balance)
	}
	if got := countLedgerEntries(t, ctx, conn, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries = %d, want %d (a single debit)", got, ledgerBaseline+1)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_SameBetFiftyTimesAcrossInstances_OneDebit sends the same
// BET, same Idempotency-Key, fifty times, round-robined across all three
// instances at once - the ticket's own scenario. Only one of the fifty may
// be a genuine PROCESSED; the rest must observe the first one's persisted
// result as a replay, regardless of which instance they landed on.
func TestMultiInstance_SameBetFiftyTimesAcrossInstances_OneDebit(t *testing.T) {
	instances := startTrio(t)
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)

	wallet := openWalletAt(t, instances[0], admin, newUUID(t), "1000.00")
	ledgerBaseline := countLedgerEntries(t, ctx, conn, wallet.ID)
	duplicatesBaseline := make([]float64, len(instances))
	for i, inst := range instances {
		duplicatesBaseline[i] = duplicateAttemptsMetric(t, inst, "HTTP")
	}

	key := "idem-" + uniqueID("k")
	in := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "30.00", Currency: testCurrency,
	}

	const attempts = 50
	statusCodes := make([]int, attempts)
	results := make([]wageringHTTPResponse, attempts)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		target := instances[i%len(instances)]
		go func(i int, target *instance) {
			defer wg.Done()
			ready.Done()
			<-start
			resp, body := doWageringAt(t, target, provider, in, key)
			statusCodes[i] = resp.StatusCode
			if resp.StatusCode == http.StatusOK {
				results[i] = decodeWageringResponse(t, body)
			}
		}(i, target)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	newAttempts, replays := 0, 0
	for i, code := range statusCodes {
		if code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200", i, code)
		}
		if results[i].IdempotentReplay {
			replays++
		} else {
			newAttempts++
		}
	}
	if newAttempts != 1 || replays != attempts-1 {
		t.Fatalf("newAttempts = %d, replays = %d, want exactly 1 and %d", newAttempts, replays, attempts-1)
	}

	// The spec requires proving the repeated attempts actually reached the
	// application via the duplicate-attempts metric, not just inferring it
	// from the replays already counted above - and, since the 50 attempts
	// are spread round-robin across all three instances, that proof has to
	// sum each instance's own increment (spec, Testing Decisions: "Os testes
	// de duplicidade provam que as entradas repetidas chegaram de fato à
	// aplicação, pela métrica de duplicatas").
	var duplicatesIncrease float64
	for i, inst := range instances {
		duplicatesIncrease += duplicateAttemptsMetric(t, inst, "HTTP") - duplicatesBaseline[i]
	}
	if duplicatesIncrease != 49 {
		t.Errorf("wagering_duplicate_attempts_total{channel=\"HTTP\"} increased by %v across the trio, want 49", duplicatesIncrease)
	}

	balance := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balance != 97000 {
		t.Fatalf("stored wallet balance = %d, want 97000 (a single 30.00 debit from 1000.00)", balance)
	}
	if got := countLedgerEntries(t, ctx, conn, wallet.ID); got != ledgerBaseline+1 {
		t.Errorf("ledger entries = %d, want %d despite 50 submissions across 3 instances", got, ledgerBaseline+1)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balance {
		t.Errorf("stored balance %d does not match ledger net %d", balance, net)
	}
}

// TestMultiInstance_DistinctWalletsAcrossInstances_ProcessInParallel proves
// distinct wallets never serialize against each other just because they
// share instances: one BET per wallet, each wallet's op landing on a
// different instance, all fired at once.
func TestMultiInstance_DistinctWalletsAcrossInstances_ProcessInParallel(t *testing.T) {
	instances := startTrio(t)
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)

	const wallets = 6
	walletResponses := make([]walletHTTPResponse, wallets)
	for i := range walletResponses {
		walletResponses[i] = openWalletAt(t, instances[i%len(instances)], admin, newUUID(t), "100.00")
	}

	statusCodes := make([]int, wallets)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(wallets)
	for i := 0; i < wallets; i++ {
		wg.Add(1)
		target := instances[i%len(instances)]
		go func(i int, target *instance) {
			defer wg.Done()
			ready.Done()
			<-start
			in := wageringBodyInput{
				ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: walletResponses[i].PlayerID, WalletID: walletResponses[i].ID,
				RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "10.00", Currency: testCurrency,
			}
			resp, _ := doWageringAt(t, target, provider, in, "idem-"+uniqueID("k"))
			statusCodes[i] = resp.StatusCode
		}(i, target)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	for i, code := range statusCodes {
		if code != http.StatusOK {
			t.Errorf("wallet %d status = %d, want 200", i, code)
		}
	}
	for i, w := range walletResponses {
		balance := queryWalletBalance(t, ctx, conn, w.ID)
		if balance != 9000 {
			t.Errorf("wallet %d stored balance = %d, want 9000 (\"90.00\")", i, balance)
		}
		if net := netLedgerBalance(t, ctx, conn, w.ID); net != balance {
			t.Errorf("wallet %d stored balance %d does not match ledger net %d", i, balance, net)
		}
	}
}

// TestMultiInstance_RestartAllInstances_ReplayIsIdempotent processes a BET
// on one instance, kills all three abruptly and restarts them, then resends
// the exact same request to a different instance than the one that
// originally processed it. The replay must return the original transaction
// id, status and balance - never move money again - proving idempotency
// survives every instance losing its in-memory state at once (ticket:
// "todas as instâncias reiniciadas, com replay idempotente devolvendo o
// resultado original e saldo conferido contra o ledger").
func TestMultiInstance_RestartAllInstances_ReplayIsIdempotent(t *testing.T) {
	instances := startTrio(t)
	ctx := context.Background()
	admin := adminToken(t)
	provider := providerAToken(t)
	conn := connectApp(t, ctx)

	wallet := openWalletAt(t, instances[0], admin, newUUID(t), "100.00")
	key := "idem-" + uniqueID("k")
	in := wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "game-1", Kind: "BET", Amount: "40.00", Currency: testCurrency,
	}

	firstResp, firstBody := doWageringAt(t, instances[0], provider, in, key)
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("initial BET status = %d, want 200, body = %s", firstResp.StatusCode, firstBody)
	}
	original := decodeWageringResponse(t, firstBody)
	if original.IdempotentReplay {
		t.Fatalf("initial BET reported idempotentReplay = true, want a fresh PROCESSED")
	}
	balanceBeforeRestart := queryWalletBalance(t, ctx, conn, wallet.ID)

	restarted := restartTrio(t, instances)

	replayResp, replayBody := doWageringAt(t, restarted[1], provider, in, key)
	if replayResp.StatusCode != http.StatusOK {
		t.Fatalf("replay after restart status = %d, want 200, body = %s", replayResp.StatusCode, replayBody)
	}
	replay := decodeWageringResponse(t, replayBody)
	if !replay.IdempotentReplay {
		t.Errorf("replay after restart: idempotentReplay = false, want true")
	}
	if replay.TransactionID != original.TransactionID || replay.Status != original.Status || replay.Balance != original.Balance {
		t.Errorf("replay after restart = %+v, want a replay of the original result %+v", replay, original)
	}

	balanceAfterReplay := queryWalletBalance(t, ctx, conn, wallet.ID)
	if balanceAfterReplay != balanceBeforeRestart {
		t.Errorf("balance after restart+replay = %d, want unchanged %d", balanceAfterReplay, balanceBeforeRestart)
	}
	if net := netLedgerBalance(t, ctx, conn, wallet.ID); net != balanceAfterReplay {
		t.Errorf("stored balance %d does not match ledger net %d", balanceAfterReplay, net)
	}
}
