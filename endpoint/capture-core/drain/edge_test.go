package drain

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	testTenant = "tenant-1"
	testDevice = "device-1"
)

// fakeEdge is the device-facing edge over real TLS: it requests (but does not require) a client
// certificate, signs enrolment CSRs with its own device CA, and lets a test add handlers.
type fakeEdge struct {
	t      *testing.T
	srv    *httptest.Server
	mux    *http.ServeMux
	caFile string

	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	mu        sync.Mutex
	validity  time.Duration
	enrols    []protocol.EnrolmentRequest
	enrolCert []string // the client certificate CN presented on each enrolment ("" for none)
}

func newFakeEdge(t *testing.T) *fakeEdge {
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
	e := &fakeEdge{t: t, mux: http.NewServeMux(), caCert: caCert, caKey: caKey, validity: 24 * time.Hour}
	e.mux.HandleFunc("/v1/enrol", e.handleEnrol)
	e.srv = httptest.NewUnstartedServer(e.mux)
	e.srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS13}
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	e.caFile = filepath.Join(t.TempDir(), "edge-ca.pem")
	if err := os.WriteFile(e.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *fakeEdge) handleEnrol(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrolmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(e.t, w, http.StatusBadRequest, []byte(`{"error":{"code":"schema_violation"}}`))
		return
	}
	presented := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		presented = r.TLS.PeerCertificates[0].Subject.CommonName
	}
	e.mu.Lock()
	e.enrols = append(e.enrols, req)
	e.enrolCert = append(e.enrolCert, presented)
	validity := e.validity
	e.mu.Unlock()
	if req.DeploymentKey == "" && presented == "" {
		writeJSON(e.t, w, http.StatusUnauthorized, []byte(`{"error":{"code":"unknown_tenant"}}`))
		return
	}
	block, _ := pem.Decode([]byte(req.CSR))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		writeJSON(e.t, w, http.StatusBadRequest, []byte(`{"error":{"code":"schema_violation"}}`))
		return
	}
	certPEM, notAfter := e.sign(csr.PublicKey, validity)
	resp, _ := json.Marshal(protocol.EnrolmentResponse{
		SchemaVersion: protocol.EnrolmentSchemaVersion, DeviceID: testDevice, TenantID: testTenant, Region: "eu",
		Credential: protocol.IssuedCredential{CertPEM: certPEM, NotAfter: notAfter},
		ServerTime: time.Now().UTC(),
	})
	writeJSON(e.t, w, http.StatusOK, resp)
}

// sign issues a device leaf for pub, valid from an hour ago for validity.
func (e *fakeEdge) sign(pub any, validity time.Duration) (string, time.Time) {
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	notBefore := time.Now().Add(-time.Hour).Truncate(time.Second)
	notAfter := notBefore.Add(validity)
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: testDevice},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, e.caCert, pub, e.caKey)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), notAfter
}

// issued returns a credential the edge signed, as if the device had enrolled earlier.
func (e *fakeEdge) issued(validity time.Duration) *credential.Credential {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	certPEM, notAfter := e.sign(&key.PublicKey, validity)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return &credential.Credential{
		DeviceID: testDevice, TenantID: testTenant, HardwareIdentityHash: "sha256:first-issue",
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		CertPEM:    certPEM, NotAfter: notAfter,
	}
}

func (e *fakeEdge) enrolments() ([]protocol.EnrolmentRequest, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.EnrolmentRequest(nil), e.enrols...), append([]string(nil), e.enrolCert...)
}

// newTestDrainer builds a drainer against the edge. A non-nil cred is stored first, so the drainer
// starts enrolled.
func newTestDrainer(t *testing.T, e *fakeEdge, store StoreFunc, cred *credential.Credential, tweak ...func(*Config)) *Drainer {
	t.Helper()
	creds, err := credential.Open(filepath.Join(t.TempDir(), "credential.sealed"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if cred != nil {
		if err := creds.Save(cred); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		Endpoint:      e.srv.URL,
		CAFile:        e.caFile,
		TenantID:      testTenant,
		DeploymentKey: "sacdk_test",
		HardwareSeed:  "smbios:test",
		AgentVersion:  "test",
		BackoffBase:   time.Millisecond,
		BackoffCap:    20 * time.Millisecond,
		DrainInterval: time.Millisecond,
	}
	for _, f := range tweak {
		f(&cfg)
	}
	if store == nil {
		store = func() (protocol.Store, error) { return newMemStore(), nil }
	}
	d, err := New(cfg, store, creds, testLogger{t}, time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) { l.t.Logf(format, args...) }

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body []byte) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
