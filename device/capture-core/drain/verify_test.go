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
	"os/exec"
	"path/filepath"
	"runtime"
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

// The environment variables of the helper process that checks the edge's roots: the marker that
// makes the test binary run as that helper, and the fake edge's URL.
const (
	envRootsHelper = "SAC_DRAIN_TEST_ROOTS_HELPER"
	envRootsEdge   = "SAC_DRAIN_TEST_ROOTS_EDGE"
)

// TestHelperEdgeRootsAreThePlatforms is the helper process: SSL_CERT_FILE is set in its
// environment, and the edge's roots must still be the platform's, so an edge that only the named
// file trusts does not verify.
func TestHelperEdgeRootsAreThePlatforms(t *testing.T) {
	if os.Getenv(envRootsHelper) == "" {
		t.Skip("run as a helper process only")
	}
	pool, err := edgeRoots("")
	if err != nil {
		t.Fatalf("edgeRoots: %v", err)
	}
	if pool.Equal(x509.NewCertPool()) {
		t.Fatalf("the roots are an empty pool, not the platform's, with SSL_CERT_FILE=%q", os.Getenv("SSL_CERT_FILE"))
	}
	err = VerifyEdge(context.Background(), os.Getenv(envRootsEdge), "")
	var cv *tls.CertificateVerificationError
	if !errors.As(err, &cv) {
		t.Fatalf("an edge trusted only by the file SSL_CERT_FILE names: %v, want a certificate verification error", err)
	}
}

// The agent's trust in the edge is the platform's. A CA file that SSL_CERT_FILE names in the
// environment (the CLI trust shim sets it machine-wide for command-line runtimes, and a service
// started afterwards inherits it) neither replaces the system roots, whether the file exists or
// not, nor adds to them. Only Windows and macOS verify through the platform.
func TestEdgeRootsAreThePlatformsWhateverSSLCertFileNames(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skipf("%s has no platform verifier: its system roots are the files SSL_CERT_FILE names", runtime.GOOS)
	}
	e := newFakeEdge(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{
		"the edge's CA":            e.caFile,
		"a file that is not there": filepath.Join(t.TempDir(), "ca-bundle.pem"),
	} {
		cmd := exec.Command(exe, "-test.run=^TestHelperEdgeRootsAreThePlatforms$", "-test.v")
		cmd.Env = append(os.Environ(), envRootsHelper+"=1", envRootsEdge+"="+e.srv.URL, "SSL_CERT_FILE="+file)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("SSL_CERT_FILE naming %s: %v\n%s", name, err, out)
		}
	}
}
