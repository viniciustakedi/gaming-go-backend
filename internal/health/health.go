// Package health defines the shared contract readiness dependencies publish
// into the Fx graph. It has no dependents of its own - internal/pg and
// internal/queue each contribute a Check, and internal/httpapi fans out over
// all of them - so none of those packages needs to import each other.
package health

import "context"

// Check reports whether a single dependency is currently reachable. It must
// return quickly: callers run it with a short, caller-owned timeout.
type Check func(ctx context.Context) error

// Named pairs a Check with the dependency name it reports as, for logging
// and for the JSON body of /health/ready.
type Named struct {
	Name  string
	Check Check
}
