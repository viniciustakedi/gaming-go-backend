//go:build faultinject

package faultinject

import (
	"os"
	"syscall"
)

// Trigger ends the process the instant point matches FAULT_INJECT_POINT -
// nothing else about the call site is inspected, so a caller cannot make
// this conditional on business state and accidentally leak a crash into
// production logic. SIGKILL, not os.Exit, is the actual crash primitive: it
// is uncatchable, so it can never run a deferred rollback, close a
// database transaction cleanly, or reach main.go's SIGTERM handler -
// exactly the "as if the process were kill -9'd mid-flight" scenario the
// multi-instance harness needs to prove idempotent recovery against (spec,
// "Injeção de falhas e ambiente"). This file only ever links into a binary
// built with `-tags faultinject`; see faultinject.go for the no-op that
// every other build carries instead.
func Trigger(point string) {
	if point == "" || os.Getenv("FAULT_INJECT_POINT") != point {
		return
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}
