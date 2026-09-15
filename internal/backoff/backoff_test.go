package backoff

import (
	"math"
	"testing"
	"time"
)

func TestExponential(t *testing.T) {
	cases := []struct {
		name              string
		attempt           int
		base, max, expect time.Duration
	}{
		{name: "first attempt", attempt: 0, base: time.Second, max: time.Minute, expect: time.Second},
		{name: "second attempt", attempt: 1, base: time.Second, max: time.Minute, expect: 2 * time.Second},
		{name: "caps at maximum", attempt: 6, base: time.Second, max: time.Minute, expect: time.Minute},
		{name: "does not overflow", attempt: 63, base: time.Second, max: time.Duration(math.MaxInt64), expect: time.Duration(math.MaxInt64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Exponential(tc.attempt, tc.base, tc.max); got != tc.expect {
				t.Errorf("Exponential(%d, %s, %s) = %s, want %s", tc.attempt, tc.base, tc.max, got, tc.expect)
			}
		})
	}
}
