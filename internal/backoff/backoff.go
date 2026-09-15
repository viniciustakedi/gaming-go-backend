// Package backoff holds the bounded retry schedule shared by queue workers.
package backoff

import "time"

// Exponential returns base doubled once per prior attempt, capped at max.
// The cap is checked before every doubling so large attempt counts cannot
// overflow time.Duration and turn a retry delay negative.
func Exponential(attempt int, base, max time.Duration) time.Duration {
	if base >= max {
		return max
	}
	if attempt <= 0 {
		return base
	}
	delay := base
	for range attempt {
		if delay > max/2 {
			return max
		}
		delay *= 2
	}
	return delay
}
