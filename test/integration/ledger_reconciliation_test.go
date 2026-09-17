//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// wallet-admin ledger paging and reconciliation through the fully composed Fx
// application, with Postgres as the source of truth.
type ledgerHTTPResponse struct {
	Entries []struct {
		SequenceNumber int64     `json:"sequenceNumber"`
		TransactionID  string    `json:"transactionId"`
		Direction      string    `json:"direction"`
		Money          moneyJSON `json:"money"`
	} `json:"entries"`
	NextCursor *string `json:"nextCursor"`
}

type reconciliationHTTPResponse struct {
	WalletID          string    `json:"walletId"`
	StoredBalance     moneyJSON `json:"storedBalance"`
	CalculatedBalance moneyJSON `json:"calculatedBalance"`
	Difference        moneyJSON `json:"difference"`
	Consistent        bool      `json:"consistent"`
	CheckedEntries    int64     `json:"checkedEntries"`
}

func decodeLedgerHTTPResponse(t *testing.T, body []byte) ledgerHTTPResponse {
	t.Helper()
	var response ledgerHTTPResponse
	requireNoError(t, json.Unmarshal(body, &response), "decode ledger response: "+string(body))
	return response
}

func decodeReconciliationHTTPResponse(t *testing.T, body []byte) reconciliationHTTPResponse {
	t.Helper()
	var response reconciliationHTTPResponse
	requireNoError(t, json.Unmarshal(body, &response), "decode reconciliation response: "+string(body))
	return response
}

func reconciliationDivergencesMetric(t *testing.T, h *appHarness) float64 {
	t.Helper()
	resp, body := h.do(t, http.MethodGet, "/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "wallet_reconciliation_divergences_total ") {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "wallet_reconciliation_divergences_total ")), 64)
		requireNoError(t, err, "parse wallet_reconciliation_divergences_total")
		return value
	}
	return 0
}

func TestReconciliation_OpeningAndBetMatchesStoredBalance(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "1000.00")
	resp, body := doWagering(t, h, providerAToken(t), wageringBodyInput{
		ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID,
		RoundID: uniqueID("round"), GameID: "ledger-game", Kind: "BET", Amount: "25.00", Currency: testCurrency,
	}, "idem-"+uniqueID("key"), uniqueID("corr"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup BET status = %d, want 200, body = %s", resp.StatusCode, body)
	}

	resp, body = h.do(t, http.MethodPost, "/wallets/"+wallet.ID+"/reconciliation", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reconciliation status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	got := decodeReconciliationHTTPResponse(t, body)
	if got.WalletID != wallet.ID || got.StoredBalance != (moneyJSON{Amount: "975.00", Currency: "BRL"}) || got.CalculatedBalance != (moneyJSON{Amount: "975.00", Currency: "BRL"}) || got.Difference != (moneyJSON{Amount: "0.00", Currency: "BRL"}) || !got.Consistent || got.CheckedEntries != 2 {
		t.Errorf("reconciliation = %+v, want wallet %s, balances 975.00/975.00/0.00, consistent true, checkedEntries 2", got, wallet.ID)
	}
}

func TestReconciliation_DivergenceReportsMetricAndDoesNotRepairBalance(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	wallet := openWalletHTTP(t, h, "100.00")
	owner := connectOwner(t, ctx)
	_, err := owner.Exec(ctx, `UPDATE wallets SET balance = 9500 WHERE id = $1`, wallet.ID)
	requireNoError(t, err, "force stored balance divergence as owner")
	beforeMetric := reconciliationDivergencesMetric(t, h)

	resp, body := h.do(t, http.MethodPost, "/wallets/"+wallet.ID+"/reconciliation", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reconciliation status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	got := decodeReconciliationHTTPResponse(t, body)
	if got.StoredBalance != (moneyJSON{Amount: "95.00", Currency: "BRL"}) || got.CalculatedBalance != (moneyJSON{Amount: "100.00", Currency: "BRL"}) || got.Difference != (moneyJSON{Amount: "-5.00", Currency: "BRL"}) || got.Consistent || got.CheckedEntries != 1 {
		t.Errorf("reconciliation = %+v, want 95.00, 100.00, -5.00, false, 1", got)
	}
	if afterMetric := reconciliationDivergencesMetric(t, h); afterMetric != beforeMetric+1 {
		t.Errorf("divergence metric = %v, want %v", afterMetric, beforeMetric+1)
	}
	var balance int64
	requireNoError(t, h.pool.QueryRow(ctx, `SELECT balance FROM wallets WHERE id = $1`, wallet.ID).Scan(&balance), "read stored balance after reconciliation")
	if balance != 9500 {
		t.Errorf("stored balance after reconciliation = %d, want unchanged forced value 9500", balance)
	}
}

func TestLedgerPagination_KeysetCursorKeepsExistingEntriesExactlyOnceDuringConcurrentWrite(t *testing.T) {
	h := newAppHarness(t)
	wallet := openWalletHTTP(t, h, "100.00")
	token := providerAToken(t)
	for i := 0; i < 3; i++ {
		resp, body := doWagering(t, h, token, wageringBodyInput{ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "ledger-game", Kind: "BET", Amount: "1.00", Currency: testCurrency}, "idem-"+uniqueID("key"), uniqueID("corr"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("setup BET %d status = %d, want 200, body = %s", i, resp.StatusCode, body)
		}
	}
	resp, body := h.do(t, http.MethodGet, "/wallets/"+wallet.ID+"/ledger?limit=2", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first ledger page status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	first := decodeLedgerHTTPResponse(t, body)
	if len(first.Entries) != 2 || first.NextCursor == nil {
		t.Fatalf("first page = %+v, want two entries and next cursor", first)
	}

	resp, body = doWagering(t, h, token, wageringBodyInput{ProviderID: "provider-a", ExternalID: uniqueID("ext"), PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: uniqueID("round"), GameID: "ledger-game", Kind: "BET", Amount: "1.00", Currency: testCurrency}, "idem-"+uniqueID("key"), uniqueID("corr"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("concurrent BET status = %d, want 200, body = %s", resp.StatusCode, body)
	}

	seen := map[int64]bool{}
	for _, entry := range first.Entries {
		seen[entry.SequenceNumber] = true
	}
	cursor := *first.NextCursor
	for cursor != "" {
		resp, body = h.do(t, http.MethodGet, "/wallets/"+wallet.ID+"/ledger?limit=2&cursor="+cursor, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("next ledger page status = %d, want 200, body = %s", resp.StatusCode, body)
		}
		page := decodeLedgerHTTPResponse(t, body)
		for _, entry := range page.Entries {
			if seen[entry.SequenceNumber] {
				t.Fatalf("sequence %d repeated across pages", entry.SequenceNumber)
			}
			seen[entry.SequenceNumber] = true
		}
		if page.NextCursor == nil {
			cursor = ""
		} else {
			cursor = *page.NextCursor
		}
	}
	if len(seen) != 5 {
		t.Errorf("unique ledger entries seen = %d, want opening + four bets = 5", len(seen))
	}
	resp, body = h.do(t, http.MethodGet, "/wallets/"+wallet.ID+"/ledger?cursor=not-a-cursor", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid cursor status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}
