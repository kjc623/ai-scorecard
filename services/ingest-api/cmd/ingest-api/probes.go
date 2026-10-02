package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// The two paths the container platform probes.
//
// infra/modules/container-app.bicep probes livenessPath (/healthz) and readinessPath (/readyz) on
// targetPort. The service already served /healthz; it did not serve /readyz, so a container built
// from this binary would have stayed unready forever behind a 404 — a second, quieter instance of
// the same defect as binding loopback: the deployment and the process disagreeing about a contract
// neither declares in one place.
//
// Readiness is deliberately not liveness, which is what the module's own comment asks for: "a
// service holding a broken database connection is ready to be taken out of rotation, not killed."
// So /readyz asks a real dependency — the store reads ref.route_fidelity, which is a query against
// PostgreSQL in the SQL mode and a proof that the process built its state in memory mode — and
// answers 503 when it fails, while /healthz stays a liveness answer that the platform may restart on.

const (
	probeReadyBody    = `{"status":"ready"}`
	probeNotReadyBody = `{"status":"not-ready"}`
	probeTimeout      = 2 * time.Second
)

// withProbes adds /readyz in front of the service handler. Every other path, including /healthz and
// /v1/events, is passed through untouched, so this wrapper cannot change what the device-facing
// surface does.
func withProbes(inner http.Handler, ready func(context.Context) error, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if ready == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, probeReadyBody)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			// The reason is logged rather than returned: the probe's reader is the platform, and a
			// dependency error can carry a hostname or a DSN fragment.
			logger.Warn("readiness probe failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, probeNotReadyBody)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, probeReadyBody)
	})
	mux.Handle("/", inner)
	return mux
}
