package drain

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

func diagnosticsDrainer(t *testing.T, fn http.HandlerFunc) *Drainer {
	t.Helper()
	e := newFakeEdge(t)
	e.mux.HandleFunc(protocol.DiagnosticsUploadPath, fn)
	return newTestDrainer(t, e, nil, e.issued(24*time.Hour))
}

func TestUploadDiagnosticsSendsTheBundleForTheRequest(t *testing.T) {
	var gotCert, gotRequest, gotType string
	var gotBody []byte
	d := diagnosticsDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			gotCert = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		gotRequest, gotType = r.Header.Get(protocol.HeaderDiagnosticsRequest), r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := d.UploadDiagnostics(context.Background(), "r1", []byte("PK zip")); err != nil {
		t.Fatal(err)
	}
	if gotCert != testDevice || gotRequest != "r1" || gotType != protocol.DiagnosticsContentType || string(gotBody) != "PK zip" {
		t.Fatalf("upload: certificate %q request %q type %q body %q", gotCert, gotRequest, gotType, gotBody)
	}
}

func TestUploadDiagnosticsNamesARequestNoLongerWaiting(t *testing.T) {
	d := diagnosticsDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusConflict, []byte(`{"error":{"code":"diagnostics_not_pending"}}`))
	})
	if err := d.UploadDiagnostics(context.Background(), "r1", []byte("PK")); !errors.Is(err, ErrDiagnosticsNotPending) {
		t.Fatalf("a refusal naming the request: %v, want ErrDiagnosticsNotPending", err)
	}
	d = diagnosticsDrainer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if err := d.UploadDiagnostics(context.Background(), "r1", []byte("PK")); err == nil || errors.Is(err, ErrDiagnosticsNotPending) {
		t.Fatalf("an outage: %v, want another error", err)
	}
}

func TestUploadDiagnosticsRefusesAnOversizeBundle(t *testing.T) {
	called := false
	d := diagnosticsDrainer(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if err := d.UploadDiagnostics(context.Background(), "r1", make([]byte, protocol.MaxDiagnosticsBundleBytes+1)); err == nil || called {
		t.Fatalf("an oversize bundle: %v, sent %v", err, called)
	}
}
