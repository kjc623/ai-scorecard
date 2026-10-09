package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestRouteMirrorsTheGateway(t *testing.T) {
	for path, want := range map[string]struct {
		upstream string
		status   int
	}{
		"/v1/events":                          {upstreamIngest, 0},
		"/v1/enrol":                           {upstreamControl, 0},
		"/v1/policy":                          {upstreamControl, 0},
		"/v1/health":                          {upstreamControl, 0},
		"/v1/content/grant":                   {upstreamControl, 0},
		"/v1/content":                         {upstreamControl, 0},
		"/v1/extension/updates.xml":           {upstreamControl, 0},
		"/v1/extension/shadow-ai-capture.crx": {upstreamControl, 0},
		// Refused by the firewall rule.
		"/":                       {"", http.StatusForbidden},
		"/healthz":                {"", http.StatusForbidden},
		"/v1/query":               {"", http.StatusForbidden},
		"/admin/v1/deployment":    {"", http.StatusForbidden},
		"/internal/v1/auth/begin": {"", http.StatusForbidden},
		"/.well-known/jwks.json":  {"", http.StatusForbidden},
		"/v1/extension":           {"", http.StatusForbidden},
		// Allowed by the firewall, served by no path rule.
		"/v1/events/extra":   {"", http.StatusBadGateway},
		"/v1/contents":       {"", http.StatusBadGateway},
		"/v1/content/upload": {"", http.StatusBadGateway},
		"/v1/extension/":     {"", http.StatusBadGateway},
	} {
		upstream, status := route(path)
		if upstream != want.upstream || status != want.status {
			t.Errorf("route(%q) = %q, %d; want %q, %d", path, upstream, status, want.upstream, want.status)
		}
	}
}

func TestForwardsTheLeafAndNothingTheClientSent(t *testing.T) {
	seen := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	h := newHandler(map[string]*url.URL{upstreamIngest: u, upstreamControl: u})

	leaf := selfSigned(t)
	req := httptest.NewRequest(http.MethodPost, "https://127.0.0.1:8443/v1/events", nil)
	req.Header.Set(clientCertHeader, "forged")
	req.Header.Set("X-Forwarded-Host", "forged")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, leaf}}
	h.ServeHTTP(httptest.NewRecorder(), req)
	got := <-seen

	decoded, err := url.PathUnescape(got.Get(clientCertHeader))
	if err != nil {
		t.Fatalf("X-Client-Cert is not percent-encoded: %v", err)
	}
	block, rest := pem.Decode([]byte(decoded))
	if block == nil || len(rest) != 0 || string(block.Bytes) != string(leaf.Raw) {
		t.Fatalf("X-Client-Cert must carry exactly the leaf, got %q", decoded)
	}
	if got.Get("X-Forwarded-Proto") != "https" || got.Get("X-Forwarded-Host") != "127.0.0.1:8443" {
		t.Fatalf("forwarding headers are wrong: %v", got)
	}

	// No certificate: a client-supplied header does not survive.
	req = httptest.NewRequest(http.MethodPost, "https://127.0.0.1:8443/v1/enrol", nil)
	req.Header.Set(clientCertHeader, "forged")
	req.TLS = &tls.ConnectionState{}
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := <-seen; got.Get(clientCertHeader) != "" {
		t.Fatalf("a request without a certificate forwarded X-Client-Cert %q", got.Get(clientCertHeader))
	}
}

func TestConfigNeedsTheServerKeyPair(t *testing.T) {
	if _, err := configFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("a configuration without TLS material must be refused")
	}
	env := map[string]string{"EDGE_TLS_CERT_PEM": "c", "EDGE_TLS_KEY_PEM": "k", "EDGE_INGEST_URL": "not a url"}
	if _, err := configFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("a malformed upstream must be refused")
	}
	delete(env, "EDGE_INGEST_URL")
	c, err := configFromEnv(func(k string) string { return env[k] })
	if err != nil || c.upstream[upstreamIngest].String() != "http://ingest-api:8080" || c.addr != "0.0.0.0:8443" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
}

func selfSigned(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "device"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
