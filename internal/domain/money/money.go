// Package money contains the monetary value object used by the domain.
package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
)

const (
	minorUnitsPerMajor = 100
	// MaximumAmount is the largest positive amount representable by Money.
	MaximumAmount = "92233720368547758.07"
)

var (
	ErrInvalidMoney        = errors.New("invalid money")
	ErrInvalidAmount       = errors.New("invalid money amount")
	ErrUnsupportedCurrency = errors.New("unsupported currency")
	ErrCurrencyMismatch    = errors.New("money currency mismatch")
	ErrOverflow            = errors.New("money overflow")
)

var canonicalAmount = regexp.MustCompile(`^(0|[1-9]\d*)\.\d{2}$`)

// Currency is an ISO 4217 currency code accepted by the wallet domain.
type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

// Error identifies the domain category for an invalid monetary operation.
type Error struct {
	Kind error
}

func (e *Error) Error() string {
	return e.Kind.Error()
}

func (e *Error) Unwrap() error {
	return e.Kind
}

// Money is an immutable amount in minor currency units.
type Money struct {
	minorUnits int64
	currency   Currency
}

// New constructs an internal monetary value from minor units.
func New(minorUnits int64, currency Currency) (Money, error) {
	if !isSupported(currency) {
		return Money{}, domainError(ErrUnsupportedCurrency)
	}
	if minorUnits == math.MinInt64 {
		return Money{}, domainError(ErrOverflow)
	}

	return Money{minorUnits: minorUnits, currency: currency}, nil
}

// Parse accepts only the canonical non-negative external decimal representation.
func Parse(amount string, currency Currency) (Money, error) {
	if !canonicalAmount.MatchString(amount) {
		return Money{}, domainError(ErrInvalidAmount)
	}

	minorUnits, ok := parseMinorUnits(amount)
	if !ok {
		return Money{}, domainError(ErrOverflow)
	}

	return New(minorUnits, currency)
}

// Zero returns the zero amount for currency.
func Zero(currency Currency) (Money, error) {
	return New(0, currency)
}

// MinorUnits returns the stored amount after validating the value object.
func (m Money) MinorUnits() (int64, error) {
	if err := m.validate(); err != nil {
		return 0, err
	}

	return m.minorUnits, nil
}

// Currency returns the currency after validating the value object.
func (m Money) Currency() (Currency, error) {
	if err := m.validate(); err != nil {
		return "", err
	}

	return m.currency, nil
}

// Add returns the sum of two values in the same currency.
func (m Money) Add(other Money) (Money, error) {
	if err := m.requireComparable(other); err != nil {
		return Money{}, err
	}
	if (other.minorUnits > 0 && m.minorUnits > math.MaxInt64-other.minorUnits) ||
		(other.minorUnits < 0 && m.minorUnits <= math.MinInt64-other.minorUnits) {
		return Money{}, domainError(ErrOverflow)
	}

	return Money{minorUnits: m.minorUnits + other.minorUnits, currency: m.currency}, nil
}

// Subtract returns the difference between two values in the same currency.
func (m Money) Subtract(other Money) (Money, error) {
	if err := m.requireComparable(other); err != nil {
		return Money{}, err
	}
	if (other.minorUnits > 0 && m.minorUnits <= math.MinInt64+other.minorUnits) ||
		(other.minorUnits < 0 && m.minorUnits > math.MaxInt64+other.minorUnits) {
		return Money{}, domainError(ErrOverflow)
	}

	return Money{minorUnits: m.minorUnits - other.minorUnits, currency: m.currency}, nil
}

// Negate returns the additive inverse of a value.
func (m Money) Negate() (Money, error) {
	if err := m.validate(); err != nil {
		return Money{}, err
	}
	if m.minorUnits == math.MinInt64 {
		return Money{}, domainError(ErrOverflow)
	}

	return Money{minorUnits: -m.minorUnits, currency: m.currency}, nil
}

// Compare reports whether m is less than, equal to, or greater than other.
func (m Money) Compare(other Money) (int, error) {
	if err := m.requireComparable(other); err != nil {
		return 0, err
	}

	switch {
	case m.minorUnits < other.minorUnits:
		return -1, nil
	case m.minorUnits > other.minorUnits:
		return 1, nil
	default:
		return 0, nil
	}
}

// MarshalJSON encodes Money using the external decimal contract.
func (m Money) MarshalJSON() ([]byte, error) {
	amount, err := m.decimal()
	if err != nil {
		return nil, err
	}

	return json.Marshal(struct {
		Amount   string   `json:"amount"`
		Currency Currency `json:"currency"`
	}{
		Amount:   amount,
		Currency: m.currency,
	})
}

// UnmarshalJSON decodes Money only from the external decimal contract.
func (m *Money) UnmarshalJSON(data []byte) error {
	var input struct {
		Amount   json.RawMessage `json:"amount"`
		Currency Currency        `json:"currency"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return domainError(ErrInvalidMoney)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return domainError(ErrInvalidMoney)
	}

	var amount string
	if len(input.Amount) == 0 || json.Unmarshal(input.Amount, &amount) != nil {
		return domainError(ErrInvalidAmount)
	}

	parsed, err := Parse(amount, input.Currency)
	if err != nil {
		return err
	}

	*m = parsed
	return nil
}

func (m Money) decimal() (string, error) {
	if err := m.validate(); err != nil {
		return "", err
	}

	magnitude := uint64(m.minorUnits)
	sign := ""
	if m.minorUnits < 0 {
		magnitude = uint64(-m.minorUnits)
		sign = "-"
	}

	return sign + strconv.FormatUint(magnitude/minorUnitsPerMajor, 10) + "." + fmt.Sprintf("%02d", magnitude%minorUnitsPerMajor), nil
}

func (m Money) requireComparable(other Money) error {
	if err := m.validate(); err != nil {
		return err
	}
	if err := other.validate(); err != nil {
		return err
	}
	if m.currency != other.currency {
		return domainError(ErrCurrencyMismatch)
	}

	return nil
}

func (m Money) validate() error {
	if !isSupported(m.currency) {
		return domainError(ErrInvalidMoney)
	}

	return nil
}

func isSupported(currency Currency) bool {
	return currency == BRL || currency == USD || currency == EUR
}

func parseMinorUnits(amount string) (int64, bool) {
	minorUnits := int64(0)
	for i := 0; i < len(amount); i++ {
		if amount[i] == '.' {
			continue
		}

		digit := int64(amount[i] - '0')
		if minorUnits > (math.MaxInt64-digit)/10 {
			return 0, false
		}
		minorUnits = minorUnits*10 + digit
	}

	return minorUnits, true
}

func domainError(kind error) error {
	return &Error{Kind: kind}
}
