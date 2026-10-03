package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/keys"
)

// The two paths the container platform probes.
//
// infra/modules/container-app.bicep probes livenessPath (/healthz) and readinessPath (/readyz) on
// targetPort. The vault already served /healthz inside internal/httpapi; /readyz did not exist, so a
// container built from this binary would have stayed unready behind a 404 — the same class of defect
// as binding loopback, and invisible for the same reason: the deployment and the process disagree
// about a contract that lives in neither of them.
//
// Readiness here answers a question the vault can actually fail: **is the key backend usable?** A
// process started with an acknowledged-but-unimplemented KMS backend is alive and must not be sent
// traffic, because every operation it exists for would fail. That is exactly the distinction the
// module's readiness probe is for. When the SQL store mode lands, its connection check belongs here
// too, which is why the probe takes the store mode rather than assuming.
func withProbes(inner http.Handler, kw keys.KeyWrapper, storeMode string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		_ = ctx
		if reason, ready := readiness(kw, storeMode); !ready {
			logger.Warn("readiness probe failed", "reason", reason)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"not-ready","reason":"`+reason+`"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ready","key_backend":"`+string(kw.Kind())+`","store":"`+storeMode+`"}`)
	})
	mux.Handle("/", inner)
	return mux
}

// readiness decides whether this process can do the work it exists for.
//
// The one thing it must never do is report ready for a backend this build does not have: a
// deployment would then route content operations to a process whose every unwrap fails, and the
// failure would appear as an application error rather than as an unhealthy replica.
func readiness(kw keys.KeyWrapper, storeMode string) (reason string, ready bool) {
	if kw == nil {
		return "no key backend was constructed", false
	}
	switch kw.Kind() {
	case keys.KindLocalSoftware:
		// Implemented, and its operations are in-process.
		return "", true
	case keys.KindAzureKeyVault, keys.KindCustomerHSM:
		return "the configured key backend is not implemented in this build (ADR 0016); it started only because --allow-unimplemented-kms was passed", false
	}
	return "unknown key backend " + string(kw.Kind()), false
}
