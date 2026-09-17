package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/auth"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// fakeDetailTransactionRepository is this file's own minimal double for
// GetTransactionUseCase's port, just enough to drive the two read handlers
// without a real Postgres - the seam 3a suite in test/integration covers
// the real repository and its provider isolation end to end.
type fakeDetailTransactionRepository struct {
	fakeNoopTransactionRepository
	byID       *walletapp.TransactionDetail
	byExternal *walletapp.TransactionDetail
}

func (f fakeDetailTransactionRepository) FindDetailByID(ctx context.Context, id string) (*walletapp.TransactionDetail, error) {
	if f.byID == nil {
		return nil, walletapp.ErrNotFound
	}
	return f.byID, nil
}

func (f fakeDetailTransactionRepository) FindDetailByProviderExternalID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.TransactionDetail, error) {
	if f.byExternal == nil {
		return nil, walletapp.ErrNotFound
	}
	return f.byExternal, nil
}

func newGetTransactionUseCase(byID, byExternal *walletapp.TransactionDetail) *walletapp.GetTransactionUseCase {
	return walletapp.NewGetTransactionUseCase(fakeDetailTransactionRepository{byID: byID, byExternal: byExternal})
}

func requestWithIdentityAndPath(method, path string, identity auth.Identity, pathValues map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), identity))
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	return req
}

func sampleDetail(t *testing.T, providerID string) *walletapp.TransactionDetail {
	t.Helper()
	amount, err := money.New(1000, money.BRL)
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	return &walletapp.TransactionDetail{
		TransactionID: "3fa85f64-5717-4562-b3fc-2c963f66afa6", ExternalTransactionID: "ext-1", ProviderID: providerID,
		PlayerID: "player-1", WalletID: "wallet-1", RoundID: "round-1", GameID: "game-1",
		Kind: domainwallet.Bet, Origin: domainwallet.External, Money: amount,
		Status: domainwallet.Processed, ResultingBalance: &amount, CreatedAt: now, UpdatedAt: now,
	}
}

func TestWageringTransactionByIDHandler_NoIdentity_Returns401(t *testing.T) {
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(nil, nil), discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6", nil)
	req.SetPathValue("transactionId", "3fa85f64-5717-4562-b3fc-2c963f66afa6")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body)
	}
}

func TestWageringTransactionByIDHandler_MalformedID_Returns400(t *testing.T) {
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(nil, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/not-a-uuid",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"transactionId": "not-a-uuid"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got.Error.Code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", got.Error.Code)
	}
}

// TestWageringTransactionByIDHandler_NonCanonicalUUID_Returns400 checks the
// path id is a canonical UUID - lowercase hex, hyphenated 8-4-4-4-12 - before
// the use case ever runs. uuid.Parse alone is not enough: it also accepts
// uppercase, unhyphenated and braced spellings.
func TestWageringTransactionByIDHandler_NonCanonicalUUID_Returns400(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"uppercase", "3FA85F64-5717-4562-B3FC-2C963F66AFA6"},
		{"no hyphens", "3fa85f6457174562b3fc2c963f66afa6"},
		{"with spaces", "3fa85f64-5717-4562-b3fc-2c963f66afa6 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := wageringTransactionByIDHandler(newGetTransactionUseCase(nil, nil), discardLogger())

			req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/x",
				auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
				map[string]string{"transactionId": tc.id})
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
			}
			got := decodeErrorBody(t, rec.Body.Bytes())
			if got.Error.Code != "INVALID_REQUEST" {
				t.Errorf("code = %q, want INVALID_REQUEST", got.Error.Code)
			}
		})
	}
}

func TestWageringTransactionByIDHandler_EmptyID_Returns400(t *testing.T) {
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(nil, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"transactionId": ""})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

func TestWageringTransactionByIDHandler_Missing_Returns404(t *testing.T) {
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(nil, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got.Error.Code != "TRANSACTION_NOT_FOUND" {
		t.Errorf("code = %q, want TRANSACTION_NOT_FOUND", got.Error.Code)
	}
}

// TestWageringTransactionByIDHandler_OtherProviderRow_Returns404NotOwn
// proves the isolation rule end to end through the handler: provider-a's
// own row exists, but a caller with a different providerId is told it does
// not, not that it belongs to someone else (spec: "sem revelar existência").
func TestWageringTransactionByIDHandler_OtherProviderRow_Returns404NotOwn(t *testing.T) {
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(sampleDetail(t, "provider-a"), nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-b"},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

func TestWageringTransactionByIDHandler_OwnerProvider_Returns200(t *testing.T) {
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(sampleDetail(t, "provider-a"), nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
}

func TestWageringTransactionByIDHandler_Admin_SeesOtherProviderRow(t *testing.T) {
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(sampleDetail(t, "provider-a"), nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleWalletAdmin}},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
}

// specTransactionDetailKeys is every key the wire body must carry, set or not,
// so an omitempty silently dropping one never slips back in undetected.
var specTransactionDetailKeys = []string{
	"transactionId", "externalTransactionId", "providerId", "playerId", "walletId",
	"roundId", "gameId", "kind", "origin", "money",
	"referenceExternalTransactionId", "referenceTransactionId", "status", "failureCode",
	"resultingBalance", "attempts", "nextAttemptAt", "pendingExpiresAt", "createdAt", "updatedAt",
}

func decodeTransactionDetailMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	for _, key := range specTransactionDetailKeys {
		if _, ok := decoded[key]; !ok {
			t.Errorf("response missing key %q, want it present (null when not applicable)", key)
		}
	}
	return decoded
}

func assertNullKeys(t *testing.T, decoded map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if decoded[key] != nil {
			t.Errorf("key %q = %v, want null", key, decoded[key])
		}
	}
}

// TestWageringTransactionByIDHandler_OpeningRow_NullFieldsPresent pins that an
// INTERNAL OPENING row - carrying none of externalTransactionId, providerId,
// roundId or gameId - still sends every key as an explicit null rather than
// omitting it.
func TestWageringTransactionByIDHandler_OpeningRow_NullFieldsPresent(t *testing.T) {
	amount, err := money.New(5000, money.BRL)
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	opening := &walletapp.TransactionDetail{
		TransactionID: "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		PlayerID:      "player-1", WalletID: "wallet-1",
		Kind: domainwallet.Opening, Origin: domainwallet.Internal, Money: amount,
		Status: domainwallet.Processed, ResultingBalance: &amount,
		CreatedAt: now, UpdatedAt: now,
	}
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(opening, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleWalletAdmin}},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	decoded := decodeTransactionDetailMap(t, rec.Body.Bytes())
	assertNullKeys(t, decoded, "externalTransactionId", "providerId", "roundId", "gameId",
		"referenceExternalTransactionId", "referenceTransactionId", "failureCode", "nextAttemptAt", "pendingExpiresAt")
	if decoded["resultingBalance"] == nil {
		t.Error(`key "resultingBalance" = null, want the settled balance for a PROCESSED row`)
	}
}

// TestWageringTransactionByIDHandler_NoReference_NullReferenceFields proves
// the same fix for an external operation with no reference at all (a plain
// BET): referenceExternalTransactionId and referenceTransactionId are null,
// not omitted.
func TestWageringTransactionByIDHandler_NoReference_NullReferenceFields(t *testing.T) {
	detail := sampleDetail(t, "provider-a")
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(detail, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	decoded := decodeTransactionDetailMap(t, rec.Body.Bytes())
	assertNullKeys(t, decoded, "referenceExternalTransactionId", "referenceTransactionId", "failureCode", "nextAttemptAt", "pendingExpiresAt")
}

// TestWageringTransactionByIDHandler_Timestamps_SerializedAsUTC pins that
// timestamps returned by the repository in a non-UTC zone still serialize as
// RFC 3339 in UTC, terminated in "Z", never with a local offset baked in.
func TestWageringTransactionByIDHandler_Timestamps_SerializedAsUTC(t *testing.T) {
	amount, err := money.New(1000, money.BRL)
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	brt := time.FixedZone("BRT", -3*3600)
	createdAt := time.Date(2026, 9, 15, 9, 0, 0, 0, brt)
	updatedAt := time.Date(2026, 9, 15, 10, 30, 0, 0, brt)
	detail := &walletapp.TransactionDetail{
		TransactionID: "3fa85f64-5717-4562-b3fc-2c963f66afa6", ExternalTransactionID: "ext-1", ProviderID: "provider-a",
		PlayerID: "player-1", WalletID: "wallet-1",
		Kind: domainwallet.Bet, Origin: domainwallet.External, Money: amount,
		Status: domainwallet.Processed, ResultingBalance: &amount,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(detail, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleWalletAdmin}},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	decoded := decodeTransactionDetailMap(t, rec.Body.Bytes())
	if decoded["createdAt"] != "2026-09-15T12:00:00Z" {
		t.Errorf("createdAt = %v, want %q", decoded["createdAt"], "2026-09-15T12:00:00Z")
	}
	if decoded["updatedAt"] != "2026-09-15T13:30:00Z" {
		t.Errorf("updatedAt = %v, want %q", decoded["updatedAt"], "2026-09-15T13:30:00Z")
	}
}

// TestWageringTransactionByIDHandler_PendingReferenceRow_SerializesAllFields
// checks the PENDING_REFERENCE shape by hand - attempts, nextAttemptAt,
// pendingExpiresAt and referenceExternalTransactionId set, with
// referenceTransactionId, failureCode and resultingBalance still null - so a
// regression on any single key, type or value is caught.
func TestWageringTransactionByIDHandler_PendingReferenceRow_SerializesAllFields(t *testing.T) {
	amount, err := money.New(2500, money.BRL)
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	createdAt := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 9, 15, 12, 5, 0, 0, time.UTC)
	nextAttemptAt := time.Date(2026, 9, 15, 12, 10, 0, 0, time.UTC)
	pendingExpiresAt := time.Date(2026, 9, 15, 13, 0, 0, 0, time.UTC)
	detail := &walletapp.TransactionDetail{
		TransactionID: "3fa85f64-5717-4562-b3fc-2c963f66afa6", ExternalTransactionID: "ext-1", ProviderID: "provider-a",
		PlayerID: "player-1", WalletID: "wallet-1",
		Kind: domainwallet.Win, Origin: domainwallet.External, Money: amount,
		ReferenceExternalTransactionID: "ext-ref-1",
		Status:                         domainwallet.PendingReference,
		Attempts:                       3,
		NextAttemptAt:                  &nextAttemptAt,
		PendingExpiresAt:               &pendingExpiresAt,
		CreatedAt:                      createdAt, UpdatedAt: updatedAt,
	}
	handler := wageringTransactionByIDHandler(newGetTransactionUseCase(detail, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
		auth.Identity{Roles: []string{auth.RoleWalletAdmin}},
		map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response body: %v", err)
	}

	// want is the entire expected wire body, hand-written key by key -
	// money as {amount, currency} strings and every timestamp as an exact
	// RFC 3339 UTC string - so the comparison below catches a missing key,
	// an extra key, a wrong type (e.g. attempts serialized as a string) or
	// a wrong value on ANY field, not just attempts's type.
	want := map[string]any{
		"transactionId":                  "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		"externalTransactionId":          "ext-1",
		"providerId":                     "provider-a",
		"playerId":                       "player-1",
		"walletId":                       "wallet-1",
		"roundId":                        nil,
		"gameId":                         nil,
		"kind":                           "WIN",
		"origin":                         "EXTERNAL",
		"money":                          map[string]any{"amount": "25.00", "currency": "BRL"},
		"referenceExternalTransactionId": "ext-ref-1",
		"referenceTransactionId":         nil,
		"status":                         "PENDING_REFERENCE",
		"failureCode":                    nil,
		"resultingBalance":               nil,
		"attempts":                       float64(3),
		"nextAttemptAt":                  "2026-09-15T12:10:00Z",
		"pendingExpiresAt":               "2026-09-15T13:00:00Z",
		"createdAt":                      "2026-09-15T12:00:00Z",
		"updatedAt":                      "2026-09-15T12:05:00Z",
	}

	if !reflect.DeepEqual(got, want) {
		for key, wantValue := range want {
			gotValue, ok := got[key]
			if !ok {
				t.Errorf("response missing key %q, want %#v", key, wantValue)
				continue
			}
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Errorf("key %q = %#v (%T), want %#v (%T)", key, gotValue, gotValue, wantValue, wantValue)
			}
		}
		for key, gotValue := range got {
			if _, ok := want[key]; !ok {
				t.Errorf("response has unexpected extra key %q = %#v", key, gotValue)
			}
		}
		t.Fatalf("response body mismatch\n got:  %#v\nwant: %#v", got, want)
	}
}

func TestProviderWageringTransactionHandler_MismatchedProvider_Returns403(t *testing.T) {
	handler := providerWageringTransactionHandler(newGetTransactionUseCase(nil, sampleDetail(t, "provider-a")), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/providers/provider-a/wagering/transactions/ext-1",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-b"},
		map[string]string{"providerId": "provider-a", "externalTransactionId": "ext-1"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body)
	}
	assertErrorCode(t, rec.Body.Bytes(), codeForbidden)
}

func TestProviderWageringTransactionHandler_OwnerProvider_Returns200(t *testing.T) {
	handler := providerWageringTransactionHandler(newGetTransactionUseCase(nil, sampleDetail(t, "provider-a")), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/providers/provider-a/wagering/transactions/ext-1",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"providerId": "provider-a", "externalTransactionId": "ext-1"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
}

func TestProviderWageringTransactionHandler_Admin_AnyProvider_Returns200(t *testing.T) {
	handler := providerWageringTransactionHandler(newGetTransactionUseCase(nil, sampleDetail(t, "provider-a")), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/providers/provider-a/wagering/transactions/ext-1",
		auth.Identity{Roles: []string{auth.RoleWalletAdmin}},
		map[string]string{"providerId": "provider-a", "externalTransactionId": "ext-1"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
}

func TestProviderWageringTransactionHandler_EmptyExternalID_Returns400(t *testing.T) {
	handler := providerWageringTransactionHandler(newGetTransactionUseCase(nil, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/providers/provider-a/wagering/transactions/",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"providerId": "provider-a", "externalTransactionId": ""})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if len(got.Error.Details) != 1 || got.Error.Details[0].Field != "externalTransactionId" {
		t.Errorf("details = %+v, want exactly one item naming field \"externalTransactionId\"", got.Error.Details)
	}
}

func TestProviderWageringTransactionHandler_Missing_Returns404(t *testing.T) {
	handler := providerWageringTransactionHandler(newGetTransactionUseCase(nil, nil), discardLogger())

	req := requestWithIdentityAndPath(http.MethodGet, "/providers/provider-a/wagering/transactions/ext-1",
		auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"},
		map[string]string{"providerId": "provider-a", "externalTransactionId": "ext-1"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

// TestWageringTransactionByIDHandler_RolePrecedenceMatrix wires the handler
// behind requireAnyRole exactly as server.go does for
// GET /wagering/transactions/:transactionId, and drives all four token shapes.
// wallet-admin wins whenever present, and provider_id is required only from a
// caller acting solely as a provider - otherwise wallet-admin+provider would
// resolve differently depending on whether provider_id happened to be set.
func TestWageringTransactionByIDHandler_RolePrecedenceMatrix(t *testing.T) {
	providerOrAdmin := []string{auth.RoleProvider, auth.RoleWalletAdmin}
	handler := requireAnyRole(providerOrAdmin, wageringTransactionByIDHandler(newGetTransactionUseCase(sampleDetail(t, "provider-a"), nil), discardLogger()))

	cases := []struct {
		name       string
		identity   auth.Identity
		wantStatus int
	}{
		{"provider+wallet-admin without provider_id sees everything",
			auth.Identity{Roles: []string{auth.RoleProvider, auth.RoleWalletAdmin}}, http.StatusOK},
		{"provider+wallet-admin with provider_id sees everything",
			auth.Identity{Roles: []string{auth.RoleProvider, auth.RoleWalletAdmin}, ProviderID: "provider-b"}, http.StatusOK},
		{"provider only without provider_id is forbidden",
			auth.Identity{Roles: []string{auth.RoleProvider}}, http.StatusForbidden},
		{"provider only with own provider_id sees its own transaction",
			auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
				tc.identity, map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.wantStatus, rec.Body)
			}
		})
	}

	t.Run("provider only with someone else's provider_id sees nothing", func(t *testing.T) {
		req := requestWithIdentityAndPath(http.MethodGet, "/wagering/transactions/3fa85f64-5717-4562-b3fc-2c963f66afa6",
			auth.Identity{Roles: []string{auth.RoleProvider}, ProviderID: "provider-b"},
			map[string]string{"transactionId": "3fa85f64-5717-4562-b3fc-2c963f66afa6"})
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body)
		}
	})
}
