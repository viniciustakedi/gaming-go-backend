package faultinject

import "testing"

// TestTrigger_NoOpWithoutFaultinjectTag proves the production build (this
// package built with no build tag, exactly how `go test ./...` and
// `go vet ./...` compile it per the ticket's own verification commands)
// never acts on Trigger, even when FAULT_INJECT_POINT names the exact point
// being triggered. Only a binary built with `-tags faultinject` links
// faultinject_enabled.go instead, which the multi-instance harness (not
// this suite) compiles and runs as a separate process - this test must
// never run against that build, since Trigger would then really end the
// process.
func TestTrigger_NoOpWithoutFaultinjectTag(t *testing.T) {
	t.Setenv("FAULT_INJECT_POINT", "before-commit")

	Trigger("before-commit")

	// Reaching this line is the assertion: the no-op build returned
	// instead of ending the process.
}
