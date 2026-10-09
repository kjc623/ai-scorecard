package integration

import (
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/protocol"
)

// The real capture-spool and the real drain against a fake device edge: an observation minted by
// the pipeline and spooled encrypted at rest is drained oldest-first to POST /v1/events over
// mutual TLS, with the device certificate the edge's device CA issued, and settled delivered.
// Nothing is a fake except the edge, which this module never runs.

type ingestPeer struct {
	mu         sync.Mutex
	events     int
	batches    int
	clientCNs  []string
	clientRoot *x509.CertPool
	// bodies are the /v1/events request bodies and health the /v1/health ones, as received.
	bodies [][]byte
	health [][]byte
}

func TestDrainDeliversOverMutualTLSToTheEdge(t *testing.T) {
	sp := openSpool(t)
	cl := &recordingClassifier{resp: protocol.ClassifyResponse{
		ClassifierVersion: "integration-rules-1",
		Confidence:        protocol.ConfidenceHigh,
		Labels:            []protocol.Label{{Class: "customer_pii", Score: 0.91, RuleID: "PII_1"}},
	}}
	p := newPipeline(t, sp, cl, protocol.ModeM1)
	obs := observationFromFrame(t, filepath.Join("testdata", "native", "text-ascii.json"))
	out, err := p.Process(context.Background(), toCoreObservation(obs, &countingReader{body: obs.Content}, &passthroughExtractor{}))
	if err != nil || !out.Emitted {
		t.Fatalf("Process: emitted=%v err=%v", out.Emitted, err)
	}

	deviceCA, deviceCAKey := mustCA(t, "integration device CA")
	peer := &ingestPeer{clientRoot: x509.NewCertPool()}
	peer.clientRoot.AddCert(deviceCA)
	srv, caFile := startTLSIngest(t, peer)

	// A credential the device CA issued, so the drain is already enrolled.
	credStore, err := credential.Open(filepath.Join(t.TempDir(), "credential.sealed"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := credStore.Save(issuedCredential(t, deviceCA, deviceCAKey)); err != nil {
		t.Fatalf("Save credential: %v", err)
	}

	d, err := drain.New(drain.Config{
		Endpoint:      srv.URL,
		CAFile:        caFile,
		TenantID:      testTenant,
		AgentVersion:  "integration",
		BackoffBase:   time.Millisecond,
		BackoffCap:    50 * time.Millisecond,
		DrainInterval: time.Second,
	}, func() (protocol.Store, error) { return sp, nil }, credStore, nil, time.Now)
	if err != nil {
		t.Fatalf("drain.New: %v", err)
	}
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Delivered != 1 {
		t.Fatalf("Delivered = %d, want 1", res.Delivered)
	}
	if st := sp.Stats(); st.DeliveredTotal != 1 {
		t.Fatalf("spool delivered_total = %d, want 1", st.DeliveredTotal)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.events != 1 || peer.batches != 1 {
		t.Fatalf("the edge received %d events in %d batches, want 1 in 1", peer.events, peer.batches)
	}
	if len(peer.clientCNs) != 1 || peer.clientCNs[0] != testDevice {
		t.Fatalf("client certificates verified by the edge = %v, want the device leaf", peer.clientCNs)
	}
}

// startTLSIngest serves /v1/events and /v1/health over TLS with a server certificate from a
// throwaway CA written to a file the drain trusts, and verifies each client certificate against the
// device CA, as the edge does before it forwards the leaf.
func startTLSIngest(t *testing.T, peer *ingestPeer) (*httptest.Server, string) {
	t.Helper()
	ca, caKey := mustCA(t, "integration edge CA")
	leaf := mustServerLeaf(t, ca, caKey)
	caFile := filepath.Join(t.TempDir(), "edge-ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			body, err := readGunzip(r)
			if err != nil {
				writeIngestJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": protocol.ReasonSchemaViolation}})
				return
			}
			peer.mu.Lock()
			peer.health = append(peer.health, body)
			peer.mu.Unlock()
			now := time.Now().UTC()
			writeIngestJSON(w, http.StatusOK, protocol.HealthResponse{AckedAt: now, ServerTime: now, NextReportAfterS: 60})
			return
		}
		if r.URL.Path != "/v1/events" {
			writeIngestJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "unknown"}})
			return
		}
		cn := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		body, err := readGunzip(r)
		var batch protocol.EventBatch
		if err == nil {
			err = json.Unmarshal(body, &batch)
		}
		if err != nil {
			writeIngestJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": protocol.ReasonSchemaViolation}})
			return
		}
		peer.mu.Lock()
		peer.bodies = append(peer.bodies, body)
		peer.batches++
		peer.events += len(batch.Events)
		peer.clientCNs = append(peer.clientCNs, cn)
		peer.mu.Unlock()
		results := make([]protocol.EventResult, len(batch.Events))
		for i, ev := range batch.Events {
			var env struct {
				EventID string `json:"event_id"`
			}
			_ = json.Unmarshal(ev, &env)
			results[i] = protocol.EventResult{EventID: env.EventID, Outcome: protocol.OutcomeAccepted}
		}
		writeIngestJSON(w, http.StatusOK, protocol.EventBatchResponse{
			SchemaVersion: "1.0", BatchID: batch.BatchID, ReceivedAt: time.Now().UTC(), ServerTime: time.Now().UTC(),
			Counts: protocol.BatchCounts{Accepted: len(results)}, Results: results,
		})
	}))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    peer.clientRoot,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, caFile
}

// issuedCredential is a device credential whose leaf the device CA signed.
func issuedCredential(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) *credential.Credential {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: testDevice},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return &credential.Credential{
		DeviceID: testDevice, TenantID: testTenant, HardwareIdentityHash: "sha256:integration",
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		CertPEM:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		NotAfter:   notAfter,
	}
}

func writeIngestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readGunzip(r *http.Request) ([]byte, error) {
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		rd = gz
	}
	return io.ReadAll(io.LimitReader(rd, 32<<20))
}

func mustCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func mustServerLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, DNSNames: []string{"localhost"},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
