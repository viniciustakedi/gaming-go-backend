//go:build !faultinject

// Package faultinject lets a named point in the code abruptly end the
// process, so multi-instance tests can prove the service survives a crash
// at that exact spot. This file is the production build: Trigger is a pure
// no-op - see faultinject_enabled.go for the build that actually kills the
// process.
package faultinject

// Trigger does nothing in a binary built without the faultinject tag.
// FAULT_INJECT_POINT is never read, so a production deployment carries no
// crash hook at all - only a call it always no-ops.
func Trigger(point string) {}
