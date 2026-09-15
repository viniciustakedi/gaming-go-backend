package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/health"
)

// Readiness tracks whether the process is still accepting traffic and runs
// the readiness dependency checks. shuttingDown flips to true as the very
// first step of shutdown - see RegisterLifecycle in server.go - so
// /health/ready starts failing before the HTTP server stops accepting
// connections, giving the orchestrator a chance to stop routing new
// requests here before Shutdown starts draining the old ones.
type Readiness struct {
	shuttingDown atomic.Bool
	checks       []health.Named
	timeout      time.Duration
}

func NewReadiness(checks []health.Named, timeout time.Duration) *Readiness {
	return &Readiness{checks: checks, timeout: timeout}
}

// MarkNotReady is called exactly once, at the start of shutdown.
func (r *Readiness) MarkNotReady() {
	r.shuttingDown.Store(true)
}

type checkResult struct {
	name string
	err  error
}

// evaluate runs every dependency check concurrently, each bounded by its own
// slice of the shared timeout, and returns the failures.
func (r *Readiness) evaluate(ctx context.Context) []checkResult {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	results := make(chan checkResult, len(r.checks))
	var wg sync.WaitGroup
	for _, c := range r.checks {
		wg.Add(1)
		go func(c health.Named) {
			defer wg.Done()
			results <- checkResult{name: c.Name, err: c.Check(ctx)}
		}(c)
	}
	wg.Wait()
	close(results)

	var failures []checkResult
	for res := range results {
		if res.err != nil {
			failures = append(failures, res)
		}
	}
	return failures
}

func liveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "live"})
	}
}

func readyHandler(readiness *Readiness) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if readiness.shuttingDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "shutting-down"})
			return
		}

		failures := readiness.evaluate(r.Context())
		if len(failures) > 0 {
			reasons := make(map[string]string, len(failures))
			for _, f := range failures {
				reasons[f.name] = f.err.Error()
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "not-ready", "failures": reasons})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	}
}
