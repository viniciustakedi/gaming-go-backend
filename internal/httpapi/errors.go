package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
)

// errorBody is the JSON envelope every rejected request gets (spec,
// "Contratos HTTP": "error: { code, message, correctable, details }").
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	Correctable bool              `json:"correctable"`
	Details     []errorDetailItem `json:"details"`
}

// errorDetailItem names the one request field a validation failure traces
// back to, and why. Every errorDetail carries a Details slice - never
// omitted, `[]` rather than `null` when a failure has no field-level detail
// to report (spec: "error: { code, message, correctable, details }").
type errorDetailItem struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// writeErrorEnvelope is the one path every rejected request's JSON body
// goes through - operation errors (below) and auth rejections
// (auth_middleware.go's writeAuthError) alike - so the envelope's shape
// (errorBody) is serialized in exactly one place (ticket 07 review:
// "writeAuthError reescreve a serialização que writeOperationError já
// faz"). It also sets Retry-After for a transient failure so the caller
// knows it is safe, and worth it, to retry (spec: "Falha transitória do
// banco devolve 503 com Retry-After").
func writeErrorEnvelope(w http.ResponseWriter, status int, body errorBody) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeOperationError maps a classified domain error onto the HTTP status
// and body the spec's error catalog documents for it. details is always
// encoded, defaulting to an empty (not nil) slice so the envelope's
// "details" key is never omitted or null.
func writeOperationError(w http.ResponseWriter, err *operation.Error, details []errorDetailItem) {
	if details == nil {
		details = []errorDetailItem{}
	}
	writeErrorEnvelope(w, statusForOperationError(err), errorBody{Error: errorDetail{
		Code:        string(err.Code()),
		Message:     messageForCode(err.Code()),
		Correctable: err.Classification() == operation.Correctable,
		Details:     details,
	}})
}

// statusForOperationError picks the status for the /wallets endpoints this
// ticket adds. WALLET_NOT_FOUND is the one code the wallets contract
// answers with 404 rather than the generic correctable status; every other
// correctable code on POST /wallets is explicitly 400 per this ticket
// ("Entrada inválida ... devolve 400"), not the 422 the general wagering
// contract table uses for its own correctable codes.
func statusForOperationError(err *operation.Error) int {
	if err.Code() == operation.CodeWalletNotFound {
		return http.StatusNotFound
	}
	switch err.Classification() {
	case operation.Correctable:
		return http.StatusBadRequest
	case operation.Conflict:
		return http.StatusConflict
	case operation.Unavailable:
		return http.StatusServiceUnavailable
	case operation.Definitive:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// detailsForOperationError names the request field responsible for a
// correctable error raised inside a use case - reachable, on POST /wallets,
// only for a money-shaped rejection: an invalid playerId or a malformed
// body are both caught earlier, at decode time, by classifyDecodeError and
// the handler's own uuid.Parse check. Every other error kind (conflict,
// unavailable, not found) carries no single field to blame, so it gets no
// details.
func detailsForOperationError(err *operation.Error) []errorDetailItem {
	switch err.Code() {
	case operation.CodeInvalidMoney, operation.CodeUnsupportedCurrency:
		return []errorDetailItem{{Field: "initialBalance", Reason: messageForCode(err.Code())}}
	default:
		return nil
	}
}

func messageForCode(code operation.Code) string {
	switch code {
	case operation.CodeInvalidRequest:
		return "the request body is malformed or carries fields this endpoint does not accept"
	case operation.CodeInvalidMoney:
		return "amount must match the canonical decimal format with exactly two decimal places"
	case operation.CodeUnsupportedCurrency:
		return "currency is not in the supported allowlist"
	case operation.CodeWalletNotFound:
		return "no wallet exists with this id"
	case operation.CodeWalletAlreadyExists:
		return "a wallet already exists for this player and currency"
	case operation.CodeTemporarilyUnavailable:
		return "the service is temporarily unavailable, retry with the same request"
	default:
		return "request rejected"
	}
}
