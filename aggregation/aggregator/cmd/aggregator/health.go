package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// status carries what the health endpoints report: whether a run has ever completed and how long
// ago. It is deliberately small — the probes are for an orchestrator, not an operator console.
type status struct {
	mu           sync.Mutex
	lastSuccess  time.Time
	lastError    string
	lastTenants  int
	lastBucketCt int64
}

func (s *status) recordSuccess(tenants int, buckets int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSuccess = at
	s.lastTenants = tenants
	s.lastBucketCt = buckets
	s.lastError = ""
}

func (s *status) recordFailure(err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err
}

func (s *status) snapshot() (time.Time, string, int, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSuccess, s.lastError, s.lastTenants, s.lastBucketCt
}

// healthHandler serves /healthz (the process is alive) and /readyz (a run has completed recently
// enough that the aggregates can be trusted to advance). `staleAfter` is three intervals, the same
// three-missed-runs bound the read path uses to call a watermark stale.
func healthHandler(s *status, staleAfter time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		last, lastErr, tenants, buckets := s.snapshot()
		if !last.IsZero() && time.Since(last) <= staleAfter {
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "ready", "last_success": last.UTC().Format(time.RFC3339),
				"tenants": tenants, "buckets": buckets,
			})
			return
		}
		reason := "no successful run yet"
		if lastErr != "" {
			reason = lastErr
		} else if !last.IsZero() {
			reason = "last successful run is older than the staleness bound"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not-ready", "reason": reason,
			"last_success": last.UTC().Format(time.RFC3339),
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
