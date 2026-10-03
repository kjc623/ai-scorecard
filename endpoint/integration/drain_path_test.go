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
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// This test wires the real capture-spool and the real drain package to a fake ingest peer, proving
// the device-to-cloud half of the endpoint composes: an observation minted by the pipeline and
// spooled encrypted-at-rest is drained oldest-first, POSTed to /v1/events, and settled delivered.
//
// Nothing here is a fake except the ingest peer (the server side, which this module never runs) and
// the throwaway TLS CA the drain pins to, exactly as a production device pins its issuing CA set.

type ingestPeer struct {
	mu      sync.Mutex
	events  int
	batches int
}

func TestDevicePath_DrainDeliversToAnIngestPeer(t *testing.T) {
	sp := openSpool(t)

	// Spool one real observation through the pipeline.
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

	entries, err := sp.Peek(10)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d entries, want 1", len(entries))
	}

	// Fake ingest peer over real TLS, pinned by a throwaway CA the drain trusts.
	peer := &ingestPeer{}
	srv, caFile := startTLSIngest(t, peer)

	// A pre-seeded dpop credential, so the drain is already enrolled (enrolment is covered by the
	// drain package's own suite and the selftest).
	credStore := openCredentialStore(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := credStore.Save(&credential.Credential{
		Mode:       protocol.AuthModeDPoP,
		DeviceID:   testDevice,
		TenantID:   testTenant,
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		JWK:        &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"},
	}); err != nil {
		t.Fatalf("Save credential: %v", err)
	}

	d, err := drain.New(drain.Config{
		Endpoint:      srv.URL,
		AuthMode:      protocol.AuthModeDPoP,
		CAFile:        caFile,
		TenantID:      testTenant,
		DeviceID:      testDevice,
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
	if peer.events != 1 {
		t.Fatalf("ingest peer received %d events, want 1", peer.events)
	}
	if peer.batches != 1 {
		t.Fatalf("ingest peer received %d batches, want 1", peer.batches)
	}
}

func openCredentialStore(t *testing.T) *credential.Store {
	t.Helper()
	keys, err := capturespool.NewRandomMemoryKeyProvider()
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	store, err := credential.Open(filepath.Join(t.TempDir(), "credential.sealed"), keys)
	if err != nil {
		t.Fatalf("credential.Open: %v", err)
	}
	return store
}

// startTLSIngest starts an httptest TLS server answering /v1/enrol, /v1/token and /v1/events,
// pinned by a throwaway CA written to a file (returned) that the drain trusts, exactly as a
// production device pins its issuing CA set.
func startTLSIngest(t *testing.T, peer *ingestPeer) (*httptest.Server, string) {
	t.Helper()
	ca, caKey, err := integrationCA()
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	leaf, err := integrationLeaf(ca, caKey)
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	caFile := filepath.Join(t.TempDir(), "ingest-ca.crt")
	if err := writeCAFile(caFile, ca); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/enrol":
			writeIngestJSON(w, http.StatusOK, protocol.EnrolmentResponse{
				SchemaVersion: protocol.EnrolmentSchemaVersion,
				DeviceID:      testDevice,
				TenantID:      testTenant,
				Region:        "eu",
				Credential:    protocol.IssuedCredential{Mode: protocol.AuthModeDPoP, JWK: &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}},
				ServerTime:    time.Now().UTC(),
			})
		case "/v1/token":
			writeIngestJSON(w, http.StatusOK, protocol.TokenResponse{AccessToken: "integration-token", TokenType: protocol.TokenTypeDPoP, ExpiresIn: 900, ServerTime: time.Now().UTC()})
		case "/v1/events":
			body, err := readGunzip(r)
			if err != nil {
				writeIngestJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": protocol.ReasonSchemaViolation}})
				return
			}
			var batch protocol.EventBatch
			if err := json.Unmarshal(body, &batch); err != nil {
				writeIngestJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": protocol.ReasonSchemaViolation}})
				return
			}
			peer.mu.Lock()
			peer.batches++
			peer.events += len(batch.Events)
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
		default:
			writeIngestJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "unknown"}})
		}
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, caFile
}

func writeCAFile(path string, ca *x509.Certificate) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600)
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

func integrationCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "integration ingest CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func integrationLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:    []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
