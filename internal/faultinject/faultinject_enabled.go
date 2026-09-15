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
//
// syscall.Kill only enqueues the signal - it does not itself block until the
// kernel has actually torn the process down, and SIGKILL delivery to a
// multi-threaded process is not guaranteed to preempt the calling goroutine
// before its very next instruction (observed on this project's Darwin dev
// host: a network write started right after the call could still reach
// Postgres and complete a commit the trigger was meant to prevent). Once the
// signal is sent, the outcome is certain - this process is going to die -
// so blocking forever here costs nothing and closes that window: no
// caller-side code, in particular the commit or send this point guards,
// ever runs again.
func Trigger(point string) {
	if point == "" || os.Getenv("FAULT_INJECT_POINT") != point {
		return
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}
