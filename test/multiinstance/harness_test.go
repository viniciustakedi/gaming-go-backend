//go:build multiinstance

// Package multiinstance drives the same financial scenarios test/integration
// proves in one process, but against three independent `wallet-service`
// processes - separate memory, separate connections, disputing the same
// wallets over the same Postgres, MiniStack and Keycloak. It needs that real
// infrastructure up first - run scripts/wait-for-integration.sh, then:
//
//	go test -race -tags multiinstance -timeout 10m -count=1 ./test/multiinstance/...
//
// This is intentionally its own build tag, not `integration`: compiling the
// binary with -race -tags faultinject and running three real OS processes
// per test is far slower than test/integration's in-process fxtest suite, so
// the two are run as separate commands.
package multiinstance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	// buildTimeout bounds the one-time `go build` every test in this
	// package shares via buildBinary's sync.Once.
	buildTimeout = 5 * time.Minute
	// instanceContextTimeout bounds a single wallet-service process's
	// lifetime - generous enough for any scenario in this package, and a
	// contingency on top of the explicit stop/kill every instance already
	// gets (see startInstance and instance.stop).
	instanceContextTimeout = 5 * time.Minute
	// readinessTimeout bounds how long startInstance waits for
	// /health/ready before failing the test.
	readinessTimeout = 30 * time.Second
)

var (
	binaryOnce     sync.Once
	binaryPathVal  string
	binaryBuildErr error
	binaryTempDir  string
)

// buildBinary compiles cmd/wallet-service exactly once per `go test`
// invocation - with -race and -tags faultinject - and every test in this
// package that needs a running instance reuses the same binary rather than
// rebuilding it. The build itself runs under a bounded context derived from
// the first caller's t - sync.Once.Do blocks every other caller until this
// returns, so the context only needs to outlive this one synchronous call.
func buildBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wallet-service-multiinstance-")
		if err != nil {
			binaryBuildErr = fmt.Errorf("build binary: create temp dir: %w", err)
			return
		}
		binaryTempDir = dir

		ctx, cancel := context.WithTimeout(t.Context(), buildTimeout)
		defer cancel()

		out := filepath.Join(dir, "wallet-service")
		cmd := exec.CommandContext(ctx, "go", "build", "-race", "-tags", "faultinject", "-o", out, "./cmd/wallet-service")
		cmd.Dir = repoPath()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			binaryBuildErr = fmt.Errorf("build binary: %w: %s", err, stderr.String())
			return
		}
		binaryPathVal = out
	})
	if binaryBuildErr != nil {
		t.Fatalf("%v", binaryBuildErr)
	}
	return binaryPathVal
}

// TestMain removes the compiled binary's temp directory once every test in
// the package has finished, since buildBinary's sync.Once means no
// individual test owns that cleanup.
func TestMain(m *testing.M) {
	code := m.Run()
	if binaryTempDir != "" {
		_ = os.RemoveAll(binaryTempDir)
	}
	os.Exit(code)
}

// instance is one running wallet-service process, with its own memory,
// connections and log file - never shared with any other instance.
type instance struct {
	name    string
	baseURL string
	logPath string

	cmd     *exec.Cmd
	logFile *os.File

	mu      sync.Mutex
	exited  bool
	waitErr error

	// cleanupOnce makes stop idempotent: a scenario that stops or kills an
	// instance itself still has the same instance registered for automatic
	// cleanup at the end of the test.
	cleanupOnce sync.Once
}

func (inst *instance) reap() {
	err := inst.cmd.Wait()
	inst.mu.Lock()
	inst.exited = true
	inst.waitErr = err
	inst.mu.Unlock()
	_ = inst.logFile.Close()
}

func (inst *instance) hasExited() bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.exited
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startInstance launches one wallet-service process bound to httpAddr, with
// env layered over the shared base configuration, and blocks until it
// answers /health/ready - so every caller gets back an instance that is
// already safe to send traffic to.
//
// Its cleanup - stopping the process and checking its log for a race report
// - is registered via t.Cleanup immediately after the process starts, not
// after every instance in a trio comes up: if a sibling fails readiness
// afterward and fails the test, this instance still gets torn down when the
// test ends instead of leaking its process and port. The process itself runs
// under a context derived from t, bounded by instanceContextTimeout, as a
// contingency on top of that explicit stop - if the test's own goroutine
// never reaches its cleanup for some reason, the context's cancellation
// still kills the process (t.Cleanup(cancel) is registered before the stop
// cleanup, so the ordinary path always stops the process gracefully first;
// LIFO means the last registration runs first).
func startInstance(t *testing.T, binary string, env map[string]string, name, logDir, httpAddr string) *instance {
	t.Helper()

	full := make(map[string]string, len(env)+1)
	for k, v := range env {
		full[k] = v
	}
	full["HTTP_ADDR"] = httpAddr

	envSlice := make([]string, 0, len(full))
	for k, v := range full {
		envSlice = append(envSlice, k+"="+v)
	}

	logPath := filepath.Join(logDir, name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log file for instance %s: %v", name, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), instanceContextTimeout)
	cmd := exec.CommandContext(ctx, binary, "serve")
	cmd.Env = envSlice
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	inst := &instance{
		name:    name,
		baseURL: "http://" + httpAddr,
		logPath: logPath,
		cmd:     cmd,
		logFile: logFile,
	}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = logFile.Close()
		t.Fatalf("start instance %s: %v", name, err)
	}
	go inst.reap()

	t.Cleanup(cancel)
	t.Cleanup(func() { stopInstance(t, inst) })

	waitReady(ctx, t, inst)
	return inst
}

// waitReady polls inst's readiness endpoint, bounded by readinessTimeout and
// by ctx, until it answers 200 or the instance exits - never a blind loop
// detached from the test's own context.
func waitReady(ctx context.Context, t *testing.T, inst *instance) {
	t.Helper()
	readyCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if inst.hasExited() {
			t.Fatalf("instance %s exited before becoming ready - see %s", inst.name, inst.logPath)
		}
		if req, err := http.NewRequestWithContext(readyCtx, http.MethodGet, inst.baseURL+"/health/ready", nil); err == nil {
			if resp, err := httpClient.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
		select {
		case <-readyCtx.Done():
			t.Fatalf("instance %s did not become ready within %s - see %s", inst.name, readinessTimeout, inst.logPath)
		case <-ticker.C:
		}
	}
}

// waitExit blocks until inst.reap observes the process has exited, or force
// -kills it and fails the test if it hangs past timeout - used after both a
// graceful SIGTERM (stopInstance) and an abrupt SIGKILL (killInstance), so
// neither ever leaks a process past the end of a test.
func waitExit(t *testing.T, inst *instance, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if inst.hasExited() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = inst.cmd.Process.Kill()
	t.Errorf("instance %s did not exit within %s - see %s", inst.name, timeout, inst.logPath)
}

// stop signals inst with sig, waits up to timeout for it to exit and checks
// its log for a race report - exactly once, no matter how many times it is
// called (stopInstance and killInstance below both funnel through this),
// since a scenario that stops or kills an instance itself still has that
// same instance registered for cleanup at the end of the test.
func (inst *instance) stop(t *testing.T, sig syscall.Signal, timeout time.Duration) {
	inst.cleanupOnce.Do(func() {
		if !inst.hasExited() {
			if err := inst.cmd.Process.Signal(sig); err != nil {
				t.Errorf("signal %v instance %s: %v", sig, inst.name, err)
			} else {
				waitExit(t, inst, timeout)
			}
		}
		requireNoRaceOutput(t, inst)
	})
}

// stopInstance shuts an instance down the same way an operator would
// (SIGTERM, the signal main.go's own signal.NotifyContext listens for),
// giving internal/httpapi's drain sequence a chance to run.
func stopInstance(t *testing.T, inst *instance) {
	t.Helper()
	inst.stop(t, syscall.SIGTERM, 15*time.Second)
}

// killInstance ends an instance the way a crash would - SIGKILL, uncatchable,
// no drain, no clean shutdown - the harness-level equivalent of what
// faultinject.Trigger does from inside the process, for scenarios that crash
// every instance at once rather than at one named point.
func killInstance(t *testing.T, inst *instance) {
	t.Helper()
	inst.stop(t, syscall.SIGKILL, 10*time.Second)
}

// requireNoRaceOutput fails the test if this instance's combined stdout/
// stderr log - where `go build -race` writes every data race report -
// contains one, quoting the report itself so the failure is diagnosable
// even after t.TempDir() removes the log file.
func requireNoRaceOutput(t *testing.T, inst *instance) {
	t.Helper()
	data, err := os.ReadFile(inst.logPath)
	if err != nil {
		t.Errorf("read log for instance %s: %v", inst.name, err)
		return
	}
	if idx := bytes.Index(data, []byte("WARNING: DATA RACE")); idx != -1 {
		end := idx + 4000
		if end > len(data) {
			end = len(data)
		}
		t.Errorf("instance %s: race detector reported a data race:\n%s", inst.name, data[idx:end])
	}
}

// startTrio brings up three independent instances - a, b and c - sharing
// env, each on its own port and log file, and waits for all three to be
// ready before returning. Each instance registers its own cleanup as soon
// as it starts (see startInstance), so a test that restarts or kills
// instances mid-scenario, or that fails partway through startup, still gets
// every already-started instance torn down and log-checked at the end of
// the test.
func startTrio(t *testing.T) []*instance {
	t.Helper()
	binary := buildBinary(t)
	env := baseEnv(t)
	logDir := t.TempDir()

	names := [3]string{"a", "b", "c"}
	instances := make([]*instance, len(names))
	for i, name := range names {
		instances[i] = startInstance(t, binary, env, name, logDir, "127.0.0.1:"+strconv.Itoa(freePort(t)))
	}
	return instances
}

// restartTrio kills every instance abruptly (SIGKILL, no drain) and starts a
// fresh trio of processes against the same shared configuration. Each new
// instance registers its own cleanup exactly as startTrio's did.
func restartTrio(t *testing.T, instances []*instance) []*instance {
	t.Helper()
	for _, inst := range instances {
		killInstance(t, inst)
	}

	binary := buildBinary(t)
	env := baseEnv(t)
	logDir := t.TempDir()

	names := [3]string{"a-restarted", "b-restarted", "c-restarted"}
	restarted := make([]*instance, len(names))
	for i, name := range names {
		restarted[i] = startInstance(t, binary, env, name, logDir, "127.0.0.1:"+strconv.Itoa(freePort(t)))
	}
	return restarted
}

// leakTestHelperEnv, set to "1", tells
// TestStartTrio_LeavesNoInstanceRunningWhenASiblingFailsToStart that it is
// running as its own re-exec'd helper subprocess (see that test's doc
// comment) rather than as the orchestrating outer test.
const leakTestHelperEnv = "MULTIINSTANCE_LEAK_TEST_HELPER"

// leakTestMarker prefixes the line the helper subprocess logs with the
// first instance's pid and address, before the second one fails it - a
// failing test's t.Logf output is always dumped to stdout, so the
// orchestrating process can recover this line from the subprocess's
// captured output even though the helper test itself fails.
const leakTestMarker = "LEAK_TEST_INSTANCE "

// If a later instance in a trio fails to start - here forced by a port
// already in use - an already-ready sibling must not leak its process or
// port.
//
// A subtest that is expected to fail still marks its parent, and the whole
// package run, failed, so the doomed trio runs in a re-exec of this same
// test binary instead: the standard idiom for isolating an expected test
// failure. The helper subprocess logs the first instance's pid/address
// before the second one fails it, so this process can check them once the
// subprocess - and every t.Cleanup it registered - has fully exited.
func TestStartTrio_LeavesNoInstanceRunningWhenASiblingFailsToStart(t *testing.T) {
	if os.Getenv(leakTestHelperEnv) == "1" {
		runDoomedTrioHelper(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), leakTestHelperEnv+"=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err == nil {
		t.Fatalf("helper subprocess was expected to fail (forced port conflict on the second instance), it passed:\n%s", output.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run helper subprocess: %v\n%s", err, output.String())
	}

	var addr string
	var pid int
	for _, line := range strings.Split(output.String(), "\n") {
		// -test.v prefixes every logged line with "file.go:NN: ", so the
		// marker is found by substring, not by a line prefix.
		idx := strings.Index(line, leakTestMarker)
		if idx == -1 {
			continue
		}
		rest := line[idx+len(leakTestMarker):]
		if _, err := fmt.Sscanf(rest, "pid=%d addr=%s", &pid, &addr); err != nil {
			t.Fatalf("parse helper marker line %q: %v", line, err)
		}
	}
	if pid == 0 || addr == "" {
		t.Fatalf("helper subprocess never logged the first instance's pid/addr:\n%s", output.String())
	}

	// The subprocess has fully exited by now, so every t.Cleanup it
	// registered - including startInstance's for "leak-a" - has already run.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("instance a's process (pid %d) is still alive after a sibling's startup failed", pid)
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Errorf("instance a's port (%s) is still bound after a sibling's startup failed: %v", addr, err)
		return
	}
	l.Close()
}

// runDoomedTrioHelper is the re-exec'd subprocess body: start one good
// instance, log its pid and address, then force a second one to fail by
// binding it to a port already in use - exactly like a real port conflict
// would.
func runDoomedTrioHelper(t *testing.T) {
	binary := buildBinary(t)
	env := baseEnv(t)
	logDir := t.TempDir()

	busyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port to force a startup failure: %v", err)
	}
	defer busyListener.Close()

	first := startInstance(t, binary, env, "leak-a", logDir, "127.0.0.1:"+strconv.Itoa(freePort(t)))
	t.Logf("%spid=%d addr=%s", leakTestMarker, first.cmd.Process.Pid, strings.TrimPrefix(first.baseURL, "http://"))

	startInstance(t, binary, env, "leak-b", logDir, busyListener.Addr().String())
}
