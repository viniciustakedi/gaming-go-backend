package operation

import (
	"errors"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

const maxOpaqueIdentifierBytes = 255

// Action describes what the use case should do after evaluating an operation.
type Action string

const (
	// Process applies the operation's financial movement, if any.
	Process Action = "PROCESS"
	// WaitForReference persists a reversal until its reference can be resolved.
	WaitForReference Action = "WAIT_FOR_REFERENCE"
	// Reject persists a terminal business rejection.
	Reject Action = "REJECT"
)

// Decision is the pure outcome of evaluating an operation.
type Decision struct {
	Action    Action
	Direction wallet.Direction
	Error     *Error
}

// Request is the external operation content that determines its financial effect and idempotency hash.
type Request struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           wallet.WagerKind
	Money                          money.Money
	ReferenceExternalTransactionID *string
}

type referencePolicy uint8

const (
	referenceForbidden referencePolicy = iota
	referenceOptional
	referenceRequired
)

type amountPolicy uint8

const (
	positiveAmount amountPolicy = iota
	zeroAmount
)

type kindRule struct {
	direction      wallet.Direction
	reference      referencePolicy
	amount         amountPolicy
	reversal       bool
	referenceKinds map[wallet.WagerKind]struct{}
}

var kindRules = map[wallet.WagerKind]kindRule{
	wallet.Bet:      {direction: wallet.Debit, reference: referenceForbidden, amount: positiveAmount},
	wallet.Win:      {direction: wallet.Credit, reference: referenceOptional, amount: positiveAmount, referenceKinds: kinds(wallet.Bet)},
	wallet.Loss:     {reference: referenceForbidden, amount: zeroAmount},
	wallet.Refund:   {direction: wallet.Credit, reference: referenceRequired, amount: positiveAmount, reversal: true, referenceKinds: kinds(wallet.Bet)},
	wallet.Rollback: {reference: referenceRequired, amount: positiveAmount, reversal: true, referenceKinds: kinds(wallet.Bet, wallet.Win, wallet.Refund)},
}

func kinds(values ...wallet.WagerKind) map[wallet.WagerKind]struct{} {
	result := make(map[wallet.WagerKind]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

// Evaluate decides the movement for an external operation and its resolved reference.
func Evaluate(request Request, reference *wallet.WagerTransaction, referenceAlreadyReversed bool) (Decision, error) {
	rule, err := validateRequest(request)
	if err != nil {
		return Decision{}, err
	}
	if request.ReferenceExternalTransactionID == nil {
		if rule.reference == referenceRequired {
			return Decision{}, ErrReferenceRequired
		}
		return movementDecision(request.Kind, nil), nil
	}
	if rule.reference == referenceForbidden {
		return Decision{}, ErrReferenceNotAllowed
	}
	if reference == nil || reference.Status() == wallet.PendingReference {
		return Decision{Action: WaitForReference}, nil
	}
	if reference.Status() == wallet.Pending || reference.Status() == wallet.Rejected || reference.Status() == wallet.Failed {
		return rejection(ErrReferenceNotProcessed), nil
	}
	if err := validateReference(request, rule, reference); err != nil {
		return rejection(err), nil
	}
	if rule.reversal && referenceAlreadyReversed {
		return rejection(ErrReferenceAlreadyReversed), nil
	}
	return movementDecision(request.Kind, reference), nil
}

// ExpirePendingReference returns the terminal result at the retry or TTL limit.
// A reference that became processed is evaluated normally so the pending reversal can complete.
func ExpirePendingReference(request Request, reference *wallet.WagerTransaction, referenceAlreadyReversed bool) (Decision, error) {
	if reference == nil {
		return rejection(ErrReferenceNotFound), nil
	}
	if reference.Status() != wallet.Processed {
		return rejection(ErrReferenceNotProcessed), nil
	}
	return Evaluate(request, reference, referenceAlreadyReversed)
}

// InsufficientFundsFor translates a wallet debit failure into the operation's stable provider code.
func InsufficientFundsFor(kind wallet.WagerKind) *Error {
	if rule, ok := kindRules[kind]; ok && rule.reversal {
		return ErrReversalInsufficientFunds
	}
	return ErrInsufficientFunds
}

func validateRequest(request Request) (kindRule, *Error) {
	if err := validateInput(request); err != nil {
		return kindRule{}, err
	}
	rule, ok := kindRules[request.Kind]
	if !ok {
		if request.Kind == wallet.Opening {
			return kindRule{}, ErrKindNotAllowed
		}
		return kindRule{}, ErrInvalidRequest
	}
	currency, err := request.Money.Currency()
	if err != nil {
		if errors.Is(err, money.ErrUnsupportedCurrency) {
			return kindRule{}, ErrUnsupportedCurrency
		}
		return kindRule{}, ErrInvalidMoney
	}
	zero, err := money.Zero(currency)
	if err != nil {
		return kindRule{}, ErrUnsupportedCurrency
	}
	comparison, err := request.Money.Compare(zero)
	if err != nil {
		return kindRule{}, ErrInvalidMoney
	}
	if (rule.amount == zeroAmount && comparison != 0) || (rule.amount == positiveAmount && comparison <= 0) {
		return kindRule{}, ErrInvalidAmountForKind
	}
	return rule, nil
}

func validateInput(request Request) *Error {
	if !IsCanonicalUUID(request.PlayerID) || !IsCanonicalUUID(request.WalletID) ||
		!isOpaqueIdentifier(request.ProviderID) || !isOpaqueIdentifier(request.ExternalTransactionID) ||
		!isOpaqueIdentifier(request.RoundID) || !isOpaqueIdentifier(request.GameID) ||
		(request.ReferenceExternalTransactionID != nil && !isOpaqueIdentifier(*request.ReferenceExternalTransactionID)) {
		return ErrInvalidRequest
	}
	return nil
}

func isOpaqueIdentifier(value string) bool {
	return len(value) > 0 && len(value) <= maxOpaqueIdentifierBytes
}

// IsCanonicalUUID reports whether value is a canonical UUID: lowercase
// hexadecimal digits and hyphens in the 8-4-4-4-12 layout. It rejects
// uppercase, unhyphenated, braced, or otherwise non-canonical spellings,
// matching the spec's requirement that path UUIDs be canonical.
func IsCanonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if value[index] != '-' {
				return false
			}
			continue
		}
		if !(value[index] >= '0' && value[index] <= '9') && !(value[index] >= 'a' && value[index] <= 'f') {
			return false
		}
	}
	return true
}

func movementDecision(kind wallet.WagerKind, reference *wallet.WagerTransaction) Decision {
	rule := kindRules[kind]
	direction := rule.direction
	if kind == wallet.Rollback {
		if reference != nil && reference.Kind() == wallet.Bet {
			direction = wallet.Credit
		} else {
			direction = wallet.Debit
		}
	}
	return Decision{Action: Process, Direction: direction}
}

func rejection(err *Error) Decision { return Decision{Action: Reject, Error: err} }

func validateReference(request Request, rule kindRule, reference *wallet.WagerTransaction) *Error {
	if request.ProviderID != reference.ProviderID() || request.PlayerID != reference.PlayerID() ||
		request.WalletID != reference.WalletID() || request.RoundID != reference.RoundID() {
		return ErrReferenceMismatch
	}
	requestCurrency, requestCurrencyErr := request.Money.Currency()
	referenceCurrency, referenceCurrencyErr := reference.Money().Currency()
	if requestCurrencyErr != nil || referenceCurrencyErr != nil || requestCurrency != referenceCurrency {
		return ErrReferenceMismatch
	}
	if _, ok := rule.referenceKinds[reference.Kind()]; !ok {
		return ErrReferenceKindNotReversible
	}
	if rule.reversal {
		comparison, err := request.Money.Compare(reference.Money())
		if err != nil || comparison != 0 {
			return ErrReferenceAmountMismatch
		}
	}
	return nil
}
