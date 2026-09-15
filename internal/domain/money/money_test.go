package money_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
)

func TestParseAcceptsCanonicalAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		amount   string
		currency money.Currency
		want     int64
	}{
		{name: "zero", amount: "0.00", currency: money.BRL, want: 0},
		{name: "whole and cents", amount: "25.00", currency: money.BRL, want: 2500},
		{name: "largest representable amount", amount: "92233720368547758.07", currency: money.EUR, want: 9223372036854775807},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := money.Parse(tt.amount, tt.currency)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			units, err := value.MinorUnits()
			if err != nil {
				t.Fatalf("MinorUnits() error = %v", err)
			}
			if units != tt.want {
				t.Errorf("MinorUnits() = %d, want %d", units, tt.want)
			}
		})
	}
}

func TestParseRejectsNonCanonicalAmounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		amount string
		want   error
	}{
		{name: "empty", amount: "", want: money.ErrInvalidAmount},
		{name: "plus sign", amount: "+1.00", want: money.ErrInvalidAmount},
		{name: "minus sign", amount: "-1.00", want: money.ErrInvalidAmount},
		{name: "NaN", amount: "NaN", want: money.ErrInvalidAmount},
		{name: "Infinity", amount: "Infinity", want: money.ErrInvalidAmount},
		{name: "scientific notation", amount: "1e3", want: money.ErrInvalidAmount},
		{name: "one decimal", amount: "1.5", want: money.ErrInvalidAmount},
		{name: "three decimals", amount: "1.555", want: money.ErrInvalidAmount},
		{name: "leading zero", amount: "01.00", want: money.ErrInvalidAmount},
		{name: "leading space", amount: " 1.00", want: money.ErrInvalidAmount},
		{name: "trailing space", amount: "1.00 ", want: money.ErrInvalidAmount},
		{name: "above limit", amount: "92233720368547758.08", want: money.ErrOverflow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.Parse(tt.amount, money.BRL)
			if !errors.Is(err, tt.want) {
				t.Errorf("Parse(%q) error = %v, want errors.Is(_, %v)", tt.amount, err, tt.want)
			}

			var domainErr *money.Error
			if !errors.As(err, &domainErr) {
				t.Errorf("Parse(%q) error = %v, want a *money.Error", tt.amount, err)
			}
		})
	}
}

func TestNewRejectsUnsupportedCurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		apply func() error
	}{
		{name: "internal construction", apply: func() error { _, err := money.New(100, "GBP"); return err }},
		{name: "external parsing", apply: func() error { _, err := money.Parse("1.00", "GBP"); return err }},
		{name: "zero", apply: func() error { _, err := money.Zero("GBP"); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.apply(); !errors.Is(err, money.ErrUnsupportedCurrency) {
				t.Errorf("operation error = %v, want errors.Is(_, ErrUnsupportedCurrency)", err)
			}
		})
	}
}

func TestMoneyRejectsSmallestInt64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		apply func() error
	}{
		{
			name: "internal construction",
			apply: func() error {
				_, err := money.New(-9223372036854775808, money.BRL)
				return err
			},
		},
		{
			name: "addition result",
			apply: func() error {
				_, err := mustNew(t, -9223372036854775807, money.BRL).Add(mustNew(t, -1, money.BRL))
				return err
			},
		},
		{
			name: "subtraction result",
			apply: func() error {
				_, err := mustNew(t, -1, money.BRL).Subtract(mustNew(t, 9223372036854775807, money.BRL))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.apply(); !errors.Is(err, money.ErrOverflow) {
				t.Errorf("operation error = %v, want errors.Is(_, ErrOverflow)", err)
			}
		})
	}
}

func TestZeroBuildsAValueForEachSupportedCurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		currency money.Currency
		want     string
	}{
		{currency: money.BRL, want: `{"amount":"0.00","currency":"BRL"}`},
		{currency: money.USD, want: `{"amount":"0.00","currency":"USD"}`},
		{currency: money.EUR, want: `{"amount":"0.00","currency":"EUR"}`},
	}

	for _, tt := range tests {
		value, err := money.Zero(tt.currency)
		if err != nil {
			t.Fatalf("Zero(%q) error = %v", tt.currency, err)
		}

		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		if string(encoded) != tt.want {
			t.Errorf("json.Marshal() = %s, want %s", encoded, tt.want)
		}
	}
}

func TestMoneyArithmetic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  money.Money
		right money.Money
		apply func(money.Money, money.Money) (money.Money, error)
		want  string
	}{
		{
			name:  "addition",
			left:  mustNew(t, 500, money.BRL),
			right: mustNew(t, 250, money.BRL),
			apply: func(left, right money.Money) (money.Money, error) { return left.Add(right) },
			want:  `{"amount":"7.50","currency":"BRL"}`,
		},
		{
			name:  "subtraction produces an internal negative amount",
			left:  mustNew(t, 250, money.BRL),
			right: mustNew(t, 500, money.BRL),
			apply: func(left, right money.Money) (money.Money, error) { return left.Subtract(right) },
			want:  `{"amount":"-2.50","currency":"BRL"}`,
		},
		{
			name:  "negation",
			left:  mustNew(t, 500, money.BRL),
			apply: func(left, _ money.Money) (money.Money, error) { return left.Negate() },
			want:  `{"amount":"-5.00","currency":"BRL"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := tt.apply(tt.left, tt.right)
			if err != nil {
				t.Fatalf("operation error = %v", err)
			}

			encoded, err := json.Marshal(actual)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if string(encoded) != tt.want {
				t.Errorf("json.Marshal() = %s, want %s", encoded, tt.want)
			}
		})
	}
}

func TestMoneyOperationsRejectOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		apply func() error
	}{
		{
			name: "addition",
			apply: func() error {
				_, err := mustNew(t, 9223372036854775807, money.BRL).Add(mustNew(t, 1, money.BRL))
				return err
			},
		},
		{
			name: "subtraction",
			apply: func() error {
				_, err := mustNew(t, 9223372036854775807, money.BRL).Subtract(mustNew(t, -1, money.BRL))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.apply(); !errors.Is(err, money.ErrOverflow) {
				t.Errorf("operation error = %v, want errors.Is(_, ErrOverflow)", err)
			}
		})
	}
}

func TestMoneyOperationsRejectIncompatibleCurrencies(t *testing.T) {
	t.Parallel()

	brl := mustNew(t, 100, money.BRL)
	usd := mustNew(t, 100, money.USD)
	tests := []struct {
		name  string
		apply func() error
	}{
		{name: "addition", apply: func() error { _, err := brl.Add(usd); return err }},
		{name: "subtraction", apply: func() error { _, err := brl.Subtract(usd); return err }},
		{name: "comparison", apply: func() error { _, err := brl.Compare(usd); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.apply(); !errors.Is(err, money.ErrCurrencyMismatch) {
				t.Errorf("operation error = %v, want errors.Is(_, ErrCurrencyMismatch)", err)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  int64
		right int64
		want  int
	}{
		{name: "less than", left: -1, right: 0, want: -1},
		{name: "equal", left: 250, right: 250, want: 0},
		{name: "greater than", left: 251, right: 250, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := mustNew(t, tt.left, money.EUR).Compare(mustNew(t, tt.right, money.EUR))
			if err != nil {
				t.Fatalf("Compare() error = %v", err)
			}
			if actual != tt.want {
				t.Errorf("Compare() = %d, want %d", actual, tt.want)
			}
		})
	}
}

func TestZeroValueIsRejectedByOperations(t *testing.T) {
	t.Parallel()

	var zero money.Money
	valid := mustNew(t, 100, money.BRL)
	tests := []struct {
		name  string
		apply func() error
	}{
		{name: "minor units", apply: func() error { _, err := zero.MinorUnits(); return err }},
		{name: "currency", apply: func() error { _, err := zero.Currency(); return err }},
		{name: "addition", apply: func() error { _, err := zero.Add(valid); return err }},
		{name: "subtraction", apply: func() error { _, err := zero.Subtract(valid); return err }},
		{name: "negation", apply: func() error { _, err := zero.Negate(); return err }},
		{name: "comparison", apply: func() error { _, err := zero.Compare(valid); return err }},
		{name: "JSON marshal", apply: func() error { _, err := json.Marshal(zero); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.apply(); !errors.Is(err, money.ErrInvalidMoney) {
				t.Errorf("operation error = %v, want errors.Is(_, ErrInvalidMoney)", err)
			}
		})
	}
}

func TestJSONContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "number amount", input: `{"amount":25.00,"currency":"BRL"}`, want: money.ErrInvalidAmount},
		{name: "missing amount", input: `{"currency":"BRL"}`, want: money.ErrInvalidAmount},
		{name: "null amount", input: `{"amount":null,"currency":"BRL"}`, want: money.ErrInvalidAmount},
		{name: "invalid amount", input: `{"amount":"01.00","currency":"BRL"}`, want: money.ErrInvalidAmount},
		{name: "missing currency", input: `{"amount":"25.00"}`, want: money.ErrUnsupportedCurrency},
		{name: "null currency", input: `{"amount":"25.00","currency":null}`, want: money.ErrUnsupportedCurrency},
		{name: "unsupported currency", input: `{"amount":"25.00","currency":"GBP"}`, want: money.ErrUnsupportedCurrency},
		{name: "unknown field", input: `{"amount":"25.00","currency":"BRL","extra":"x"}`, want: money.ErrInvalidMoney},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var value money.Money
			err := json.Unmarshal([]byte(tt.input), &value)
			if !errors.Is(err, tt.want) {
				t.Errorf("json.Unmarshal() error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}

	var value money.Money
	if err := json.Unmarshal([]byte(`{"amount":"25.00","currency":"BRL"}`), &value); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(encoded) != `{"amount":"25.00","currency":"BRL"}` {
		t.Errorf("json.Marshal() = %s, want canonical JSON", encoded)
	}
}

func mustNew(t *testing.T, minorUnits int64, currency money.Currency) money.Money {
	t.Helper()

	value, err := money.New(minorUnits, currency)
	if err != nil {
		t.Fatalf("New(%d, %q) error = %v", minorUnits, currency, err)
	}
	return value
}
