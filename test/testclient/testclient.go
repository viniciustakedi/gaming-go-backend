//go:build integration || multiinstance

// Package testclient is the test client the spec's seam 3 asks for: "o mesmo
// cliente de teste roda em dois harnesses". It holds the wagering/wallet
// DTOs, the HTTP request plumbing and Keycloak token retrieval shared by
// test/integration's in-process harness (seam 3a, fxtest in one process) and
// test/multiinstance's real-process harness (seam 3b, three OS processes) -
// nothing here is a test itself, so both packages import it directly. The
// build tag keeps it out of the production binary and out of `go build ./...`
// without a tag.
package testclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// NewHTTPClient returns an *http.Client bounded by timeout - every request
// this package or its callers issue against a running wallet-service or
// Keycloak carries both this timeout and a context, so a hung server fails
// the affected test instead of hanging the suite.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// MoneyJSON mirrors internal/domain/money.Money's own external contract
// (amount as a canonical decimal string, currency as its ISO code) - the
// independent source of truth every assertion compares against is a
// hand-written expected value, never one recomputed the way the production
// code itself computes it.
type MoneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// WalletHTTPResponse mirrors internal/httpapi's response body for
// POST /wallets and GET /wallets/{walletId}.
type WalletHTTPResponse struct {
	ID       string    `json:"id"`
	PlayerID string    `json:"playerId"`
	Balance  MoneyJSON `json:"balance"`
	Version  int64     `json:"version"`
}

// WageringHTTPResponse mirrors internal/httpapi's response body for
// POST /wagering/transactions.
type WageringHTTPResponse struct {
	TransactionID    string     `json:"transactionId"`
	Status           string     `json:"status"`
	FailureCode      string     `json:"failureCode"`
	Balance          MoneyJSON  `json:"balance"`
	PendingExpiresAt *time.Time `json:"pendingExpiresAt"`
	IdempotentReplay bool       `json:"idempotentReplay"`
}

// WageringBodyInput is the input to WageringBody - every field the wagering
// HTTP contract accepts, ReferenceID included for REFUND/ROLLBACK (seam 3a,
// and, since ticket 16's fault-injection scenarios, seam 3b too).
type WageringBodyInput struct {
	ProviderID  string
	ExternalID  string
	PlayerID    string
	WalletID    string
	RoundID     string
	GameID      string
	Kind        string
	Amount      string
	Currency    string
	ReferenceID *string
}

// WageringBody builds the JSON body for POST /wagering/transactions from in.
func WageringBody(in WageringBodyInput) []byte {
	payload := map[string]any{
		"providerId":            in.ProviderID,
		"externalTransactionId": in.ExternalID,
		"playerId":              in.PlayerID,
		"walletId":              in.WalletID,
		"roundId":               in.RoundID,
		"gameId":                in.GameID,
		"kind":                  in.Kind,
		"money":                 map[string]string{"amount": in.Amount, "currency": in.Currency},
	}
	if in.ReferenceID != nil {
		payload["referenceExternalTransactionId"] = *in.ReferenceID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return body
}

// OpenWalletBody builds the JSON body for POST /wallets.
func OpenWalletBody(playerID, amount, currency string) []byte {
	body, err := json.Marshal(map[string]any{
		"playerId":       playerID,
		"initialBalance": map[string]string{"amount": amount, "currency": currency},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// DecodeWalletResponse decodes body as a WalletHTTPResponse, failing t if it
// does not parse.
func DecodeWalletResponse(t testing.TB, body []byte) WalletHTTPResponse {
	t.Helper()
	var resp WalletHTTPResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode wallet response: %v: %s", err, body)
	}
	return resp
}

// DecodeWageringResponse decodes body as a WageringHTTPResponse, failing t
// if it does not parse.
func DecodeWageringResponse(t testing.TB, body []byte) WageringHTTPResponse {
	t.Helper()
	var resp WageringHTTPResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode wagering response: %v: %s", err, body)
	}
	return resp
}

// UniqueID returns a short, random, collision-free identifier for the opaque
// external TEXT columns of this schema (provider ids, external transaction
// ids, idempotency keys, round/game ids). These only need uniqueness across
// runs, not any particular shape, so a *testing.T is not required to report
// a crypto/rand failure - practically never happens - the way NewUUID does.
func UniqueID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("generate id: %v", err))
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(buf[:]))
}

// NewUUID returns a random version-4 UUID string for the internal identifier
// columns this schema types as UUID. The domain generates real ids as UUID
// v7 (see alignment.md); a hand-rolled v4 generator needs only crypto/rand
// and avoids pulling in a UUID dependency the alignment decisions never
// called for - these tests only need a syntactically valid, unique UUID, not
// any particular version's bit layout.
func NewUUID(t testing.TB) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// EnvOrDefault returns the environment variable key, or fallback if it is
// unset or empty.
func EnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Do issues one HTTP request with ctx and reads the response body fully, so
// callers never repeat the drain-and-close dance themselves. headers
// overlays the Content-Type: application/json default this sets; an empty
// value for a key removes that header instead of setting it (used to omit
// Authorization for a missing-token scenario).
func Do(ctx context.Context, t testing.TB, client *http.Client, method, url string, headers map[string]string, body []byte) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body for %s %s: %v", method, url, err)
	}
	return resp, respBody
}

// DuplicateAttemptsMetric parses wagering_duplicate_attempts_total{channel="<channel>"}
// out of a GET /metrics response body already read, returning 0 if the
// series is absent (not yet incremented) - the independent source of truth a
// duplicate-submission scenario checks against, so it proves the repeated
// attempts actually reached the application (spec, Testing Decisions), not
// only that the final result looks right.
func DuplicateAttemptsMetric(t testing.TB, body []byte, channel string) float64 {
	t.Helper()
	prefix := fmt.Sprintf(`wagering_duplicate_attempts_total{channel="%s"} `, channel)
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
		if err != nil {
			t.Fatalf("parse wagering_duplicate_attempts_total value: %v", err)
		}
		return value
	}
	return 0
}
