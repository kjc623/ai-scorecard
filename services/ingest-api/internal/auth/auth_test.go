package auth

import (
	"context"
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
	"testing"
	"time"

	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

const (
	testTenant = "11111111-1111-1111-1111-111111111111"
	testDevice = "22222222-2222-2222-2222-222222222222"
)

// issueDeviceCertificate builds a CA and a device certificate shaped as §2.2 requires: device_id as
// the subject CN, the tenant in an organisational unit, clientAuth EKU.
func issueDeviceCertificate(t *testing.T, deviceID, tenantID string) (tls.Certificate, *x509.Certificate, *x509.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sac-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			CommonName:         deviceID,
			OrganizationalUnit: []string{tenantID},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{"ingest.eu.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("device cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse device cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	pair.Certificate = append(pair.Certificate, caDER)
	return pair, leaf, caCert
}

func credentialIDOf(leaf *x509.Certificate) string {
	return contract.DeterministicUUID("sac-credential\x1f", leaf.Raw)
}

// TestMTLSAuthenticatorOverARealHandshake drives the authenticator through an actual TLS handshake
// with a client certificate, because the identity extraction (CN and the tenant organisational
// unit) is the part a unit test with a fabricated http.Request would not exercise.
func TestMTLSAuthenticatorOverARealHandshake(t *testing.T) {
	pair, leaf, caCert := issueDeviceCertificate(t, testDevice, testTenant)

	routes, err := store.LoadRouteTable("../../testdata/route-fidelity.seed.json")
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	mem := store.NewMemory(routes)
	mem.SetPrincipal(testTenant, testDevice, credentialIDOf(leaf), store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: true,
		CredentialExpiry: time.Now().Add(90 * 24 * time.Hour),
	})

	authenticator := &MTLSAuthenticator{Store: mem, Region: "eu-west"}

	var got Principal
	var gotErr error
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotErr = authenticator.Authenticate(context.Background(), r)
		w.WriteHeader(http.StatusOK)
	}))
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert) // the issuing CA, which is what the server verifies client chains against
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS13, // §2.1
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  caPool,
		// No Certificates: the server keeps httptest's own certificate (which the client below
		// trusts), so the handshake fails on the client certificate rather than on the server name.
	}
	srv.StartTLS()
	defer srv.Close()

	// srv.Client() already trusts the test server's own certificate; the client certificate is added
	// to a clone of its transport, because the shared one belongs to the server.
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.Certificates = []tls.Certificate{pair}
	tr.TLSClientConfig.MinVersion = tls.VersionTLS13
	client := &http.Client{Transport: tr}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer resp.Body.Close()

	if gotErr != nil {
		t.Fatalf("Authenticate: %v", gotErr)
	}
	if got.TenantID != testTenant {
		t.Errorf("tenant = %q, want %q (from the certificate's organisational unit)", got.TenantID, testTenant)
	}
	if got.DeviceID != testDevice {
		t.Errorf("device = %q, want %q (from the certificate's subject CN)", got.DeviceID, testDevice)
	}
	if got.CredentialID != credentialIDOf(leaf) {
		t.Errorf("credential = %q, want the id derived from the certificate", got.CredentialID)
	}
	if got.NotAfter.IsZero() {
		t.Error("the principal carries no credential expiry")
	}
}

func TestMTLSAuthenticatorRefusesWhatItCannotVerify(t *testing.T) {
	routes, err := store.LoadRouteTable("../../testdata/route-fidelity.seed.json")
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	_, leaf, _ := issueDeviceCertificate(t, testDevice, testTenant)
	credential := credentialIDOf(leaf)

	base := store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: true, CredentialExpiry: time.Now().Add(time.Hour),
	}
	revokedAt := time.Now().Add(-time.Minute)

	cases := []struct {
		name   string
		status store.PrincipalStatus
		region string
		want   error
	}{
		{"active", base, "eu-west", nil},
		{"unknown tenant", store.PrincipalStatus{}, "", ErrUnknownTenant},
		{"suspended tenant", func() store.PrincipalStatus { s := base; s.IngestEnabled = false; return s }(), "", ErrTenantSuspended},
		{"revoked credential", func() store.PrincipalStatus { s := base; s.CredentialRevoked = &revokedAt; return s }(), "", ErrCredentialRevoked},
		{"expired credential", func() store.PrincipalStatus { s := base; s.CredentialExpiry = time.Now().Add(-time.Second); return s }(), "", ErrCredentialExpired},
		{"unknown credential", func() store.PrincipalStatus { s := base; s.CredentialKnown = false; return s }(), "", ErrCredentialUnknown},
		{"revoked device", func() store.PrincipalStatus { s := base; s.DeviceRevokedAt = &revokedAt; return s }(), "", ErrDeviceRevoked},
		{"region mismatch", base, "us-east", ErrRegionMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mem := store.NewMemory(routes)
			mem.SetPrincipal(testTenant, testDevice, credential, c.status)
			a := &MTLSAuthenticator{Store: mem, Region: c.region}
			req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
			_, err := a.Authenticate(context.Background(), req)
			if err != c.want {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestAuthenticateWithoutACredentialIsRefused covers the path where the edge let a request through
// without a client certificate: §2.2 treats the edge as a filter, never the authority.
func TestAuthenticateWithoutACredentialIsRefused(t *testing.T) {
	a := &MTLSAuthenticator{Store: store.NewMemory(nil)}
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	if _, err := a.Authenticate(context.Background(), req); err != ErrNoCredential {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
	req.TLS = &tls.ConnectionState{}
	if _, err := a.Authenticate(context.Background(), req); err != ErrNoCredential {
		t.Fatalf("empty TLS state: err = %v, want ErrNoCredential", err)
	}
}

// TestIdentityFromCertificateRejectsMalformedSubjects covers the case a certificate is issued with
// the wrong subject shape: §5.3 puts identity in the credential, so a credential that does not carry
// it cannot be trusted to name a device.
func TestIdentityFromCertificateRejectsMalformedSubjects(t *testing.T) {
	cases := []struct {
		name string
		cert *x509.Certificate
	}{
		{"no CN", &x509.Certificate{Subject: pkix.Name{OrganizationalUnit: []string{testTenant}}}},
		{"CN is not a uuid", &x509.Certificate{Subject: pkix.Name{CommonName: "device-1", OrganizationalUnit: []string{testTenant}}}},
		{"no tenant organisational unit", &x509.Certificate{Subject: pkix.Name{CommonName: testDevice}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, _, err := identityFromCertificate(c.cert); err == nil {
				t.Fatal("expected the certificate to be refused")
			}
		})
	}
}
