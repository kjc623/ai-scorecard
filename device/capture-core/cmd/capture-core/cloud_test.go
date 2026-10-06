package main

import (
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

const (
	cloudTenant = "33333333-3333-4333-8333-333333333333"
	cloudDevice = "3f0c1a2b-0000-4000-8000-000000000001"
)

// userRefKey is a fixed tenant user-reference key, so a test can derive the expected user_ref.
var userRefKey = base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

// fakeCloud is the device-facing edge for one tenant over real TLS: enrolment by deployment key
// (signing the CSR with its own device CA), GET /v1/policy, POST /v1/health and POST /v1/events.
type fakeCloud struct {
	t      *testing.T
	srv    *httptest.Server
	caFile string
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	bundle []byte // the signed envelope served; nil answers 404 no_policy_bundle

	mu       sync.Mutex
	enrols   []protocol.EnrolmentRequest
	health   []protocol.HealthRequest
	events   []json.RawMessage
	identity protocol.DeviceIdentity
}

func startFakeCloud(t *testing.T, bundle []byte) *fakeCloud {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test device CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(der)
	c := &fakeCloud{t: t, bundle: bundle, caCert: caCert, caKey: caKey, identity: protocol.DeviceIdentityClear}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enrol", c.enrol)
	mux.HandleFunc("/v1/policy", func(w http.ResponseWriter, r *http.Request) {
		if c.bundle == nil {
			writeTestJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "no_policy_bundle"}})
			return
		}
		w.Header().Set(protocol.HeaderETag, protocol.PolicyETag("5"))
		if protocol.ETagMatches(r.Header.Get(protocol.HeaderIfNoneMatch), protocol.PolicyETag("5")) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeTestJSON(w, http.StatusOK, protocol.PolicyResponse{SchemaVersion: protocol.PolicySchemaVersion, BundleVersion: "5",
			SignedBundle: c.bundle, ServerTime: time.Now().UTC()})
	})
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.HealthRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.mu.Lock()
		c.health = append(c.health, req)
		identity := c.identity
		c.mu.Unlock()
		now := time.Now().UTC()
		writeTestJSON(w, http.StatusOK, protocol.HealthResponse{AckedAt: now, ServerTime: now, NextReportAfterS: 900, DeviceIdentity: identity})
	})
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(gz)
		var batch protocol.EventBatch
		_ = json.Unmarshal(body, &batch)
		results := make([]protocol.EventResult, len(batch.Events))
		for i, ev := range batch.Events {
			var env struct {
				EventID string `json:"event_id"`
			}
			_ = json.Unmarshal(ev, &env)
			results[i] = protocol.EventResult{EventID: env.EventID, Outcome: protocol.OutcomeAccepted}
		}
		c.mu.Lock()
		c.events = append(c.events, batch.Events...)
		c.mu.Unlock()
		writeTestJSON(w, http.StatusOK, protocol.EventBatchResponse{SchemaVersion: "1.0", BatchID: batch.BatchID,
			ReceivedAt: time.Now().UTC(), ServerTime: time.Now().UTC(), Counts: protocol.BatchCounts{Accepted: len(results)}, Results: results})
	})
	c.srv = httptest.NewUnstartedServer(mux)
	c.srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS13}
	c.srv.StartTLS()
	t.Cleanup(c.srv.Close)
	c.caFile = filepath.Join(t.TempDir(), "edge-ca.pem")
	if err := os.WriteFile(c.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *fakeCloud) enrol(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrolmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeploymentKey != "sacdk_test" {
		writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unknown_tenant"}})
		return
	}
	block, _ := pem.Decode([]byte(req.CSR))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "schema_violation"}})
		return
	}
	notAfter := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cloudDevice},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.caCert, csr.PublicKey, c.caKey)
	if err != nil {
		c.t.Error(err)
		return
	}
	c.mu.Lock()
	c.enrols = append(c.enrols, req)
	identity := c.identity
	c.mu.Unlock()
	writeTestJSON(w, http.StatusOK, protocol.EnrolmentResponse{
		SchemaVersion: protocol.EnrolmentSchemaVersion, DeviceID: cloudDevice, TenantID: cloudTenant, Region: "eu",
		Credential:     protocol.IssuedCredential{CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), NotAfter: notAfter},
		DeviceIdentity: identity, UserRefKey: userRefKey, ServerTime: time.Now().UTC(),
	})
}

func (c *fakeCloud) enrolments() []protocol.EnrolmentRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.EnrolmentRequest(nil), c.enrols...)
}

func (c *fakeCloud) receivedEvents() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]json.RawMessage(nil), c.events...)
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// testConfig is what the vendor file and a tenant file give a device between them.
func testConfig(t *testing.T, cloud *fakeCloud, pub ed25519.PublicKey) Config {
	t.Helper()
	cfg := Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		TenantID:       cloudTenant,
		DeviceEndpoint: cloud.srv.URL,
		DeploymentKey:  "sacdk_test",
		CAFile:         cloud.caFile,
		PolicyKeyID:    "policy-key-1",
		DeviceIdentity: string(protocol.DeviceIdentityClear),
		LogLevel:       "info",
	}
	if pub != nil {
		cfg.PolicyKey = hex.EncodeToString(pub)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("the test configuration does not validate: %v", err)
	}
	return cfg
}

// testLogger discards the agent's log: background loops may log after a test has returned.
func testLogger(*testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
