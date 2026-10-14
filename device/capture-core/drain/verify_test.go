package drain

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The client records when the edge's certificate first failed verification, keeps that time while
// it keeps failing, and clears it once a handshake verifies the edge.
func TestClientTracksWhetherTheEdgeVerifies(t *testing.T) {
	e := newFakeEdge(t)
	e.mux.HandleFunc("/v1/policy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	c, err := newClient(e.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }
	if _, n := c.unverified(); n != 0 {
		t.Fatal("unverified before any request")
	}

	if _, _, err := c.do(context.Background(), http.MethodGet, "/v1/policy", nil, nil); err == nil {
		t.Fatal("a request to an edge the roots do not trust succeeded")
	}
	first := now
	now = now.Add(time.Minute)
	if _, _, err := c.do(context.Background(), http.MethodGet, "/v1/policy", nil, nil); err == nil {
		t.Fatal("a request to an edge the roots do not trust succeeded")
	}
	if since, n := c.unverified(); n != 2 || !since.Equal(first) {
		t.Fatalf("unverified = %v %d, want %v 2", since, n, first)
	}

	pool := x509.NewCertPool()
	pool.AddCert(e.srv.Certificate())
	c.tr.TLSClientConfig.RootCAs = pool
	if _, _, err := c.do(context.Background(), http.MethodGet, "/v1/policy", nil, nil); err != nil {
		t.Fatalf("request after trusting the edge: %v", err)
	}
	if _, n := c.unverified(); n != 0 {
		t.Fatal("still unverified after a handshake verified the edge")
	}
}

// A request that never reached a certificate check says nothing about the edge's certificate.
func TestUnreachableEdgeIsNotUnverified(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := newClient(url, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.do(context.Background(), http.MethodGet, "/v1/policy", nil, nil); err == nil {
		t.Fatal("a request to a closed edge succeeded")
	}
	if _, n := c.unverified(); n != 0 {
		t.Fatal("a connection failure was recorded as a verification failure")
	}
}

func TestVerifyEdge(t *testing.T) {
	e := newFakeEdge(t)
	if err := VerifyEdge(context.Background(), e.srv.URL, e.caFile); err != nil {
		t.Fatalf("an edge the CA file trusts: %v", err)
	}
	err := VerifyEdge(context.Background(), e.srv.URL, "")
	var cv *tls.CertificateVerificationError
	if !errors.As(err, &cv) {
		t.Fatalf("an edge the roots do not trust: %v, want a certificate verification error", err)
	}
	if err := VerifyEdge(context.Background(), "http://edge.example", ""); err == nil {
		t.Fatal("a plain http endpoint was accepted")
	}
}

// An edge that refuses the connection for want of a device certificate has still shown a
// certificate that verified.
func TestVerifyEdgeCountsARefusalAfterTheCertificateVerified(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "edge-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEdge(context.Background(), srv.URL, caFile); err != nil {
		t.Fatalf("VerifyEdge: %v", err)
	}
}
