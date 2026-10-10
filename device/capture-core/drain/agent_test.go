package drain

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// agentDrainer is an enrolled drainer whose edge serves the agent routes with fn.
func agentDrainer(t *testing.T, fn http.HandlerFunc) *Drainer {
	t.Helper()
	e := newFakeEdge(t)
	e.mux.HandleFunc(protocol.AgentReleasePath, fn)
	e.mux.HandleFunc(protocol.AgentPackagePath, fn)
	return newTestDrainer(t, e, nil, e.issued(24*time.Hour))
}

func TestFetchAgentReleaseReturnsTheStatementWithTheDeviceCertificate(t *testing.T) {
	const statement = `{"algorithm":"ed25519","payload":{"type":"agent_release"},"signature":"c2ln"}`
	var gotCert string
	d := agentDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			gotCert = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		writeJSON(t, w, http.StatusOK, []byte(statement))
	})
	got, err := d.FetchAgentRelease(context.Background())
	if err != nil || string(got) != statement || gotCert != testDevice {
		t.Fatalf("FetchAgentRelease = %s, %v (certificate %q)", got, err, gotCert)
	}
}

func TestFetchAgentReleaseNoReleaseNeedsTheErrorEnvelope(t *testing.T) {
	d := agentDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, []byte(`{"error":{"code":"release_unavailable"}}`))
	})
	if _, err := d.FetchAgentRelease(context.Background()); !errors.Is(err, ErrNoAgentRelease) {
		t.Fatalf("404 with the error envelope: %v, want ErrNoAgentRelease", err)
	}
	d = agentDrainer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, err := d.FetchAgentRelease(context.Background()); err == nil || errors.Is(err, ErrNoAgentRelease) {
		t.Fatalf("a bare 404: %v, want another error", err)
	}
}

func TestDownloadAgentPackageResumesAtTheOffset(t *testing.T) {
	pkg := bytes.Repeat([]byte("msi "), 5000)
	d := agentDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "ShadowAICapture.msi", time.Time{}, bytes.NewReader(pkg))
	})
	var got bytes.Buffer
	n, err := d.DownloadAgentPackage(context.Background(), &got, 0, 100)
	if err != nil || n != 100 {
		t.Fatalf("first part: %d, %v", n, err)
	}
	n, err = d.DownloadAgentPackage(context.Background(), &got, 100, int64(len(pkg))-100)
	if err != nil || n != int64(len(pkg))-100 || !bytes.Equal(got.Bytes(), pkg) {
		t.Fatalf("resumed: %d, %v, equal %v", n, err, bytes.Equal(got.Bytes(), pkg))
	}
}

func TestDownloadAgentPackageRefusesAWholeBodyForARange(t *testing.T) {
	d := agentDrainer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("whole"))
	})
	var got bytes.Buffer
	if _, err := d.DownloadAgentPackage(context.Background(), &got, 10, 100); !errors.Is(err, ErrRangeRefused) || got.Len() != 0 {
		t.Fatalf("a 200 for a range: %v, wrote %d", err, got.Len())
	}
}
