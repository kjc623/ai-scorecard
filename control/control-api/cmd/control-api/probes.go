package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// The two paths the container platform probes (azure/modules/container-app.bicep probes
// livenessPath /healthz and readinessPath /readyz on targetPort).
//
// Readiness is deliberately not liveness: /readyz asks a real dependency -- main.go passes a closure
// that opens a store transaction and reads ops.tenant, so a broken database connection answers 503
// while /healthz stays a liveness answer the platform may restart on. An unknown tenant is a healthy
// database, so the closure treats ErrUnknownTenant as ready.

const (
	probeReadyBody    = `{"status":"ready"}`
	probeNotReadyBody = `{"status":"not-ready"}`
	probeTimeout      = 2 * time.Second
)

// withProbes adds /readyz in front of the service handler. Every other path is passed through
// untouched, so this wrapper cannot change what the device-facing surface does.
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
