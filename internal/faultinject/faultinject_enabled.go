//go:build faultinject

package faultinject

import (
	"os"
	"syscall"
)

// Trigger ends the process the instant point matches FAULT_INJECT_POINT.
// SIGKILL, not os.Exit, is the crash primitive: it is uncatchable, so it can
// never run a deferred rollback, close a database transaction cleanly, or
// reach main.go's SIGTERM handler - exactly the "as if the process were
// kill -9'd mid-flight" scenario the multi-instance harness needs to prove
// idempotent recovery against.
//
// syscall.Kill only enqueues the signal, and SIGKILL delivery to a
// multi-threaded process is not guaranteed to preempt the calling goroutine
// before its very next instruction (observed on Darwin: a network write
// started right after the call still reached Postgres and completed a commit
// the trigger was meant to prevent). The process is going to die either way,
// so blocking forever here closes that window - no caller-side code, in
// particular the commit or send this point guards, ever runs again.
func Trigger(point string) {
	if point == "" || os.Getenv("FAULT_INJECT_POINT") != point {
		return
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}
