package main

import (
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// fakeIngest is the selftest's stand-in for the device cloud (ADR 0020). It serves /v1/enrol,
// /v1/token and /v1/events over a real TLS listener pinned by a generated CA, and records what it
// received, so the assembled endpoint's drain path is exercised end to end rather than simulated.
type fakeIngest struct {
	srv    *http.Server
	ln     net.Listener
	caFile string

	deviceID string
	tenantID string

	mu       sync.Mutex
	enrolled bool
	tokens   int
	batches  int
	events   int
	health   int
}

// startFakeIngest generates a throwaway CA + server leaf, writes the CA to a file, and serves the
// three device endpoints over TLS on an ephemeral loopback port. The CA file is what --ca-file pins
// the edge to, exactly as a production device pins its issuing CA set.
func startFakeIngest(log *slog.Logger, work, deviceID, tenantID string) (*fakeIngest, error) {
	caCert, caKey, err := newIngestCA()
	if err != nil {
		return nil, err
	}
	leaf, err := newIngestLeaf(caCert, caKey)
	if err != nil {
		return nil, err
	}
	caFile := filepath.Join(work, "ingest-ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw}), 0o600); err != nil {
		return nil, err
	}

	fi := &fakeIngest{caFile: caFile, deviceID: deviceID, tenantID: tenantID}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enrol", fi.handleEnrol)
	mux.HandleFunc("/v1/token", fi.handleToken)
	mux.HandleFunc("/v1/events", fi.handleEvents)
	mux.HandleFunc("/v1/health", fi.handleHealth)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	fi.ln = ln
	fi.srv = &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{leaf},
		},
	}
	go func() {
		if err := fi.srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			log.Warn("selftest ingest peer stopped", "error", err)
		}
	}()
	return fi, nil
}

func (f *fakeIngest) baseURL() string { return "https://" + f.ln.Addr().String() }

func (f *fakeIngest) close() {
	if f.srv != nil {
		_ = f.srv.Close()
	}
}

func (f *fakeIngest) receivedEvents() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events
}

func (f *fakeIngest) receivedBatches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batches
}

func (f *fakeIngest) receivedHealth() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health
}

// handleHealth is the peer's POST /v1/health. It validates the body as the real endpoint would, so
// the selftest asserts the heartbeat's shape rather than only its arrival.
func (f *fakeIngest) handleHealth(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req protocol.HealthRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad health body", http.StatusBadRequest)
		return
	}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.health++
	f.mu.Unlock()
	now := time.Now().UTC()
	writeIngestJSON(w, http.StatusOK, protocol.HealthResponse{AckedAt: now, ServerTime: now, NextReportAfterS: 900})
}

func (f *fakeIngest) handleEnrol(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req protocol.EnrolmentRequest
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.enrolled = true
	f.mu.Unlock()

	resp := protocol.EnrolmentResponse{
		SchemaVersion: protocol.EnrolmentSchemaVersion,
		DeviceID:      f.deviceID,
		TenantID:      f.tenantID,
		Region:        "eu",
		Credential: protocol.IssuedCredential{
			Mode: req.Mode,
			JWK:  &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"},
		},
		ServerTime: time.Now().UTC(),
	}
	writeIngestJSON(w, http.StatusOK, resp)
}

func (f *fakeIngest) handleToken(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	f.tokens++
	f.mu.Unlock()
	writeIngestJSON(w, http.StatusOK, protocol.TokenResponse{
		AccessToken: "selftest-access-token",
		TokenType:   protocol.TokenTypeDPoP,
		ExpiresIn:   900,
		ServerTime:  time.Now().UTC(),
	})
}

func (f *fakeIngest) handleEvents(w http.ResponseWriter, r *http.Request) {
	body, err := readIngestBody(r)
	if err != nil {
		writeIngestJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": protocol.ReasonSchemaViolation}})
		return
	}
	var batch protocol.EventBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		writeIngestJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": protocol.ReasonSchemaViolation}})
		return
	}

	f.mu.Lock()
	f.batches++
	f.events += len(batch.Events)
	f.mu.Unlock()

	results := make([]protocol.EventResult, len(batch.Events))
	for i, ev := range batch.Events {
		var env struct {
			EventID string `json:"event_id"`
		}
		_ = json.Unmarshal(ev, &env)
		results[i] = protocol.EventResult{EventID: env.EventID, Outcome: protocol.OutcomeAccepted}
	}
	writeIngestJSON(w, http.StatusOK, protocol.EventBatchResponse{
		SchemaVersion: "1.0",
		BatchID:       batch.BatchID,
		ReceivedAt:    time.Now().UTC(),
		ServerTime:    time.Now().UTC(),
		Counts:        protocol.BatchCounts{Accepted: len(results)},
		Results:       results,
	})
}

func readIngestBody(r *http.Request) ([]byte, error) {
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

func writeIngestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- throwaway PKI for the fake peer ------------------------------------------------------

func newIngestCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "selftest ingest CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func newIngestLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}
