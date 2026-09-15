package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
)

// fakeUnitOfWork and its repositories are this file's own minimal doubles -
// walletapp's own fakes live in its _test package and are not exported -
// just enough to drive OpenWalletUseCase.Open through openWalletHandler
// without a real Postgres, so this file can assert on the wire-level error
// envelope alone (spec: "error: { code, message, correctable, details }").
type fakeUnitOfWork struct {
	insertErr error
}

func (f fakeUnitOfWork) WithinTx(ctx context.Context, fn func(context.Context, walletapp.Repositories) error) error {
	return fn(ctx, walletapp.Repositories{
		Wallets:      fakeWalletRepository{insertErr: f.insertErr},
		Transactions: fakeNoopTransactionRepository{},
		Ledger:       fakeNoopLedgerRepository{},
		Outbox:       fakeNoopOutboxRepository{},
	})
}

type fakeWalletRepository struct{ insertErr error }

func (f fakeWalletRepository) Insert(ctx context.Context, w *domainwallet.Wallet) error {
	return f.insertErr
}

func (f fakeWalletRepository) FindByID(ctx context.Context, id string) (*domainwallet.Wallet, error) {
	return nil, walletapp.ErrNotFound
}

func (f fakeWalletRepository) FindForUpdate(ctx context.Context, id string) (*domainwallet.Wallet, error) {
	return nil, walletapp.ErrNotFound
}

func (f fakeWalletRepository) UpdateBalance(ctx context.Context, w *domainwallet.Wallet, previousVersion int64) error {
	return nil
}

type fakeNoopTransactionRepository struct{}

func (fakeNoopTransactionRepository) Insert(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	return nil
}

func (fakeNoopTransactionRepository) InsertNew(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) (bool, error) {
	return true, nil
}

func (fakeNoopTransactionRepository) InsertPending(ctx context.Context, t *domainwallet.WagerTransaction, nextAttemptAt time.Time, ttl time.Duration) (time.Time, bool, error) {
	return nextAttemptAt.Add(ttl), true, nil
}

func (fakeNoopTransactionRepository) FindByIdempotencyKey(ctx context.Context, providerID, idempotencyKey string) (*walletapp.ExistingTransaction, error) {
	return nil, walletapp.ErrNotFound
}

func (fakeNoopTransactionRepository) FindByExternalTransactionID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.ExistingTransaction, error) {
	return nil, walletapp.ErrNotFound
}

func (fakeNoopTransactionRepository) FindReference(ctx context.Context, providerID, referenceExternalTransactionID string) (*domainwallet.WagerTransaction, error) {
	return nil, walletapp.ErrNotFound
}

func (fakeNoopTransactionRepository) ExistsSuccessfulReversal(ctx context.Context, referenceTransactionID string) (bool, error) {
	return false, nil
}

func (fakeNoopTransactionRepository) FindPendingForUpdate(ctx context.Context, transactionID string) (*walletapp.PendingReferenceTransaction, error) {
	return nil, walletapp.ErrNotFound
}

func (fakeNoopTransactionRepository) ReschedulePending(ctx context.Context, transactionID string, attempts int, retryDelay time.Duration) error {
	return nil
}

func (fakeNoopTransactionRepository) CompletePending(ctx context.Context, t *domainwallet.WagerTransaction, resultingBalance *int64) error {
	return nil
}

func (fakeNoopTransactionRepository) FindDetailByID(ctx context.Context, id string) (*walletapp.TransactionDetail, error) {
	return nil, walletapp.ErrNotFound
}

func (fakeNoopTransactionRepository) FindDetailByProviderExternalID(ctx context.Context, providerID, externalTransactionID string) (*walletapp.TransactionDetail, error) {
	return nil, walletapp.ErrNotFound
}

type fakeNoopLedgerRepository struct{}

func (fakeNoopLedgerRepository) Insert(ctx context.Context, entry *domainwallet.WalletLedgerEntry) error {
	return nil
}

type fakeNoopOutboxRepository struct{}

func (fakeNoopOutboxRepository) Insert(ctx context.Context, record walletapp.OutboxRecord) error {
	return nil
}

func newOpenWalletHandler(insertErr error) http.HandlerFunc {
	useCase := walletapp.NewOpenWalletUseCase(fakeUnitOfWork{insertErr: insertErr})
	return openWalletHandler(useCase, discardLogger())
}

func decodeErrorBody(t *testing.T, body []byte) errorBody {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode error body: %v, body = %s", err, body)
	}
	return got
}

func TestOpenWalletHandler_MalformedJSON_DetailsNameBody(t *testing.T) {
	handler := newOpenWalletHandler(nil)

	req := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(`{"playerId":`))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got.Error.Code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", got.Error.Code)
	}
	if len(got.Error.Details) != 1 || got.Error.Details[0].Field != "body" {
		t.Errorf("details = %+v, want exactly one item naming field \"body\"", got.Error.Details)
	}
}

func TestOpenWalletHandler_UnknownField_DetailsNameBody(t *testing.T) {
	handler := newOpenWalletHandler(nil)

	body := `{"playerId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","initialBalance":{"amount":"10.00","currency":"BRL"},"extra":true}`
	req := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if len(got.Error.Details) != 1 || got.Error.Details[0].Field != "body" {
		t.Errorf("details = %+v, want exactly one item naming field \"body\"", got.Error.Details)
	}
}

func TestOpenWalletHandler_MoneyOutOfFormat_DetailsNameInitialBalance(t *testing.T) {
	handler := newOpenWalletHandler(nil)

	body := `{"playerId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","initialBalance":{"amount":"10.5","currency":"BRL"}}`
	req := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got.Error.Code != "INVALID_MONEY" {
		t.Errorf("code = %q, want INVALID_MONEY", got.Error.Code)
	}
	if len(got.Error.Details) != 1 || got.Error.Details[0].Field != "initialBalance" {
		t.Errorf("details = %+v, want exactly one item naming field \"initialBalance\"", got.Error.Details)
	}
}

func TestOpenWalletHandler_MissingInitialBalance_DetailsNameInitialBalance(t *testing.T) {
	handler := newOpenWalletHandler(nil)

	body := `{"playerId":"3fa85f64-5717-4562-b3fc-2c963f66afa6"}`
	req := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got.Error.Code != "INVALID_MONEY" {
		t.Errorf("code = %q, want INVALID_MONEY", got.Error.Code)
	}
	if len(got.Error.Details) != 1 || got.Error.Details[0].Field != "initialBalance" {
		t.Errorf("details = %+v, want exactly one item naming field \"initialBalance\" - this is the use case's own validation, not decodeJSONBody's", got.Error.Details)
	}
}

func TestOpenWalletHandler_InvalidPlayerID_DetailsNamePlayerId(t *testing.T) {
	handler := newOpenWalletHandler(nil)

	body := `{"playerId":"not-a-uuid","initialBalance":{"amount":"10.00","currency":"BRL"}}`
	req := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if len(got.Error.Details) != 1 || got.Error.Details[0].Field != "playerId" {
		t.Errorf("details = %+v, want exactly one item naming field \"playerId\"", got.Error.Details)
	}
}

func TestOpenWalletHandler_Conflict_DetailsIsEmptyNotOmitted(t *testing.T) {
	handler := newOpenWalletHandler(walletapp.ErrAlreadyExists)

	body := `{"playerId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","initialBalance":{"amount":"10.00","currency":"BRL"}}`
	req := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"details":[]`) {
		t.Errorf("body = %s, want the literal \"details\":[] for an error with no field-level detail", rec.Body.String())
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got.Error.Details == nil || len(got.Error.Details) != 0 {
		t.Errorf("details = %#v, want a non-nil empty slice", got.Error.Details)
	}
}

func TestGetWalletHandler_NotFound_DetailsIsEmptyNotOmitted(t *testing.T) {
	useCase := walletapp.NewGetWalletUseCase(fakeWalletRepository{})
	handler := getWalletHandler(useCase, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/wallets/3fa85f64-5717-4562-b3fc-2c963f66afa6", nil)
	req.SetPathValue("walletId", "3fa85f64-5717-4562-b3fc-2c963f66afa6")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"details":[]`) {
		t.Errorf("body = %s, want the literal \"details\":[]", rec.Body.String())
	}
}
