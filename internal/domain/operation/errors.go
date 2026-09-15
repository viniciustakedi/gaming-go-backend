// Package operation evaluates external wagering operations without I/O.
package operation

// Code is a stable failure identifier returned to an external provider.
type Code string

const (
	CodeInvalidRequest                Code = "INVALID_REQUEST"
	CodeInvalidMoney                  Code = "INVALID_MONEY"
	CodeUnsupportedCurrency           Code = "UNSUPPORTED_CURRENCY"
	CodeInvalidAmountForKind          Code = "INVALID_AMOUNT_FOR_KIND"
	CodeKindNotAllowed                Code = "KIND_NOT_ALLOWED"
	CodeMissingIdempotencyKey         Code = "MISSING_IDEMPOTENCY_KEY"
	CodeReferenceRequired             Code = "REFERENCE_REQUIRED"
	CodeReferenceNotAllowed           Code = "REFERENCE_NOT_ALLOWED"
	CodeWalletNotFound                Code = "WALLET_NOT_FOUND"
	CodeWalletPlayerMismatch          Code = "WALLET_PLAYER_MISMATCH"
	CodeWalletCurrencyMismatch        Code = "WALLET_CURRENCY_MISMATCH"
	CodeInsufficientFunds             Code = "INSUFFICIENT_FUNDS"
	CodeReversalInsufficientFunds     Code = "REVERSAL_INSUFFICIENT_FUNDS"
	CodeReferenceNotFound             Code = "REFERENCE_NOT_FOUND"
	CodeReferenceNotProcessed         Code = "REFERENCE_NOT_PROCESSED"
	CodeReferenceAlreadyReversed      Code = "REFERENCE_ALREADY_REVERSED"
	CodeReferenceKindNotReversible    Code = "REFERENCE_KIND_NOT_REVERSIBLE"
	CodeReferenceMismatch             Code = "REFERENCE_MISMATCH"
	CodeReferenceAmountMismatch       Code = "REFERENCE_AMOUNT_MISMATCH"
	CodePermanentProcessingFailure    Code = "PERMANENT_PROCESSING_FAILURE"
	CodeIdempotencyKeyReused          Code = "IDEMPOTENCY_KEY_REUSED"
	CodeExternalTransactionIDConflict Code = "EXTERNAL_TRANSACTION_ID_CONFLICT"
	CodeWalletAlreadyExists           Code = "WALLET_ALREADY_EXISTS"
	CodeTemporarilyUnavailable        Code = "TEMPORARILY_UNAVAILABLE"
)

// Classification tells callers whether a code is correctable, terminal, conflicting, or transient.
type Classification string

const (
	Correctable      Classification = "CORRECTABLE"
	Definitive       Classification = "DEFINITIVE_REJECTION"
	PermanentFailure Classification = "PERMANENT_FAILURE"
	Conflict         Classification = "CONFLICT"
	Unavailable      Classification = "UNAVAILABLE"
)

// Error is a classified domain error that can be inspected with errors.Is and errors.As.
type Error struct {
	code           Code
	classification Classification
}

func (e *Error) Error() string { return string(e.code) }

// Is compares errors by stable failure code.
func (e *Error) Is(target error) bool {
	targetError, ok := target.(*Error)
	return ok && e != nil && targetError != nil && e.code == targetError.code
}

// Code returns the stable failure identifier.
func (e *Error) Code() Code {
	if e == nil {
		return ""
	}
	return e.code
}

// Classification returns the externally observable group of this failure.
func (e *Error) Classification() Classification {
	if e == nil {
		return ""
	}
	return e.classification
}

var (
	ErrInvalidRequest                = newError(CodeInvalidRequest)
	ErrInvalidMoney                  = newError(CodeInvalidMoney)
	ErrUnsupportedCurrency           = newError(CodeUnsupportedCurrency)
	ErrInvalidAmountForKind          = newError(CodeInvalidAmountForKind)
	ErrKindNotAllowed                = newError(CodeKindNotAllowed)
	ErrMissingIdempotencyKey         = newError(CodeMissingIdempotencyKey)
	ErrReferenceRequired             = newError(CodeReferenceRequired)
	ErrReferenceNotAllowed           = newError(CodeReferenceNotAllowed)
	ErrWalletNotFound                = newError(CodeWalletNotFound)
	ErrWalletPlayerMismatch          = newError(CodeWalletPlayerMismatch)
	ErrWalletCurrencyMismatch        = newError(CodeWalletCurrencyMismatch)
	ErrInsufficientFunds             = newError(CodeInsufficientFunds)
	ErrReversalInsufficientFunds     = newError(CodeReversalInsufficientFunds)
	ErrReferenceNotFound             = newError(CodeReferenceNotFound)
	ErrReferenceNotProcessed         = newError(CodeReferenceNotProcessed)
	ErrReferenceAlreadyReversed      = newError(CodeReferenceAlreadyReversed)
	ErrReferenceKindNotReversible    = newError(CodeReferenceKindNotReversible)
	ErrReferenceMismatch             = newError(CodeReferenceMismatch)
	ErrReferenceAmountMismatch       = newError(CodeReferenceAmountMismatch)
	ErrPermanentProcessingFailure    = newError(CodePermanentProcessingFailure)
	ErrIdempotencyKeyReused          = newError(CodeIdempotencyKeyReused)
	ErrExternalTransactionIDConflict = newError(CodeExternalTransactionIDConflict)
	ErrWalletAlreadyExists           = newError(CodeWalletAlreadyExists)
	ErrTemporarilyUnavailable        = newError(CodeTemporarilyUnavailable)
)

func newError(code Code) *Error {
	classification, ok := ClassificationFor(code)
	if !ok {
		panic("operation: unknown error code")
	}
	return &Error{code: code, classification: classification}
}

// ClassificationFor returns the documented classification for a stable code.
func ClassificationFor(code Code) (Classification, bool) {
	switch code {
	case CodeInvalidRequest, CodeInvalidMoney, CodeUnsupportedCurrency, CodeInvalidAmountForKind, CodeKindNotAllowed,
		CodeMissingIdempotencyKey, CodeReferenceRequired, CodeReferenceNotAllowed, CodeWalletNotFound,
		CodeWalletPlayerMismatch, CodeWalletCurrencyMismatch:
		return Correctable, true
	case CodeInsufficientFunds, CodeReversalInsufficientFunds, CodeReferenceNotFound, CodeReferenceNotProcessed,
		CodeReferenceAlreadyReversed, CodeReferenceKindNotReversible, CodeReferenceMismatch, CodeReferenceAmountMismatch:
		return Definitive, true
	case CodePermanentProcessingFailure:
		return PermanentFailure, true
	case CodeIdempotencyKeyReused, CodeExternalTransactionIDConflict, CodeWalletAlreadyExists:
		return Conflict, true
	case CodeTemporarilyUnavailable:
		return Unavailable, true
	default:
		return "", false
	}
}
