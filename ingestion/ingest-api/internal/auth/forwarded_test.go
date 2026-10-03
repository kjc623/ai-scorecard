package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// --- certificate fixtures shared by the x509 tests ---------------------------------------------

// testAuthority is a throwaway CA for the forwarded-certificate tests: the real path's trust is an
// x509.CertPool, so the tests build one the same way a deployment would from its CA bundle.
type testAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestAuthority(t *testing.T, cn string) *testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &testAuthority{cert: cert, key: key}
}

func (ca *testAuthority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

type leafSpec struct {
	cn        string
	tenant    string
	notBefore time.Time
	notAfter  time.Time
	eku       []x509.ExtKeyUsage
}

var testSerial int64

func (ca *testAuthority) issue(t *testing.T, spec leafSpec) *x509.Certificate {
	t.Helper()
	testSerial++
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	if spec.notBefore.IsZero() {
		spec.notBefore = time.Now().Add(-time.Hour)
	}
	if spec.notAfter.IsZero() {
		spec.notAfter = time.Now().Add(90 * 24 * time.Hour)
	}
	if spec.eku == nil {
		spec.eku = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(testSerial),
		Subject: pkix.Name{
			CommonName:         spec.cn,
			OrganizationalUnit: []string{spec.tenant},
		},
		NotBefore:   spec.notBefore,
		NotAfter:    spec.notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: spec.eku,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}

// pemChainOf renders certificates as the PEM sequence an edge would put in X-Client-Cert.
func pemChainOf(certs ...*x509.Certificate) string {
	out := ""
	for _, c := range certs {
		out += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	return out
}

// spkiThumbprintOf computes the binding independently of the production helper, so a wrong
// implementation on either side is a test failure rather than a tautology.
func spkiThumbprintOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func activeStatus(thumbprint, credentialType string) store.PrincipalStatus {
	return store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: true, CredentialType: credentialType,
		PublicKeyThumbprint: thumbprint, CredentialExpiry: time.Now().Add(time.Hour),
	}
}

func forwardedRequest(header, pemChain string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	if pemChain != "" {
		req.Header.Set(header, pemChain)
	}
	return req
}

// TestForwardedCertAuthenticatorBindsAValidChain is the happy path: a clientAuth leaf signed by the
// configured CA, whose SPKI thumbprint matches the credential row, authenticates as the identity the
// certificate carries.
func TestForwardedCertAuthenticatorBindsAValidChain(t *testing.T) {
	ca := newTestAuthority(t, "sac-forwarded-ca")
	leaf := ca.issue(t, leafSpec{cn: testDevice, tenant: testTenant})

	mem := store.NewMemory(nil)
	mem.SetPrincipal(testTenant, testDevice, credentialIDOf(leaf),
		activeStatus(spkiThumbprintOf(leaf), "x509"))
	a := &ForwardedCertAuthenticator{
		Store: mem, ClientCAs: ca.pool(), Region: "eu-west", Header: protocol.HeaderClientCert,
	}

	got, err := a.Authenticate(context.Background(), forwardedRequest(protocol.HeaderClientCert,
		pemChainOf(leaf, ca.cert)))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.TenantID != testTenant || got.DeviceID != testDevice {
		t.Errorf("principal = %s/%s, want %s/%s from the certificate", got.TenantID, got.DeviceID, testTenant, testDevice)
	}
	if got.CredentialID != credentialIDOf(leaf) {
		t.Errorf("credential = %q, want the id derived from the certificate", got.CredentialID)
	}
}

func TestForwardedCertAuthenticatorRefusesWhatItCannotVerify(t *testing.T) {
	trusted := newTestAuthority(t, "sac-trusted-ca")
	stranger := newTestAuthority(t, "sac-stranger-ca")
	leaf := trusted.issue(t, leafSpec{cn: testDevice, tenant: testTenant})
	credential := credentialIDOf(leaf)

	tampered := *leaf
	tampered.Raw = append([]byte(nil), leaf.Raw...)
	tampered.Raw[len(tampered.Raw)-1] ^= 0xff
	tamperedLeaf, err := x509.ParseCertificate(tampered.Raw)
	if err != nil {
		t.Fatalf("tampered leaf did not parse; the tamper is meant to keep the structure: %v", err)
	}

	expired := trusted.issue(t, leafSpec{
		cn: testDevice, tenant: testTenant,
		notBefore: time.Now().Add(-48 * time.Hour), notAfter: time.Now().Add(-time.Hour),
	})
	unknownCA := stranger.issue(t, leafSpec{cn: testDevice, tenant: testTenant})

	cases := []struct {
		name       string
		chain      string
		thumbprint string
		want       error
	}{
		{"tampered signature", pemChainOf(tamperedLeaf), spkiThumbprintOf(tamperedLeaf), ErrBadCredential},
		{"expired leaf", pemChainOf(expired, trusted.cert), spkiThumbprintOf(expired), ErrBadCredential},
		{"unknown ca", pemChainOf(unknownCA), spkiThumbprintOf(unknownCA), ErrBadCredential},
		{"not a certificate", "this is not PEM", "", ErrBadCredential},
		{"wrong spki thumbprint", pemChainOf(leaf, trusted.cert), "a-different-thumbprint", ErrThumbprintMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mem := store.NewMemory(nil)
			mem.SetPrincipal(testTenant, testDevice, credential, activeStatus(c.thumbprint, "x509"))
			a := &ForwardedCertAuthenticator{Store: mem, ClientCAs: trusted.pool(), Region: "eu-west"}
			_, err := a.Authenticate(context.Background(), forwardedRequest(protocol.HeaderClientCert, c.chain))
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestForwardedCertAuthenticatorRefusesAMissingHeaderAndAWrongMode covers the selection edge and the
// mode binding: a present header that is empty is no credential, and a dpop row cannot be
// authenticated by a certificate.
func TestForwardedCertAuthenticatorRefusesAMissingHeaderAndAWrongMode(t *testing.T) {
	ca := newTestAuthority(t, "sac-forwarded-ca")
	leaf := ca.issue(t, leafSpec{cn: testDevice, tenant: testTenant})

	t.Run("missing header is no credential", func(t *testing.T) {
		a := &ForwardedCertAuthenticator{Store: store.NewMemory(nil), ClientCAs: ca.pool()}
		if _, err := a.Authenticate(context.Background(), forwardedRequest(protocol.HeaderClientCert, "")); !errors.Is(err, ErrNoCredential) {
			t.Fatalf("err = %v, want ErrNoCredential", err)
		}
		if a.Presents(forwardedRequest(protocol.HeaderClientCert, "")) {
			t.Error("Presents reported a header that is absent")
		}
	})

	t.Run("a dpop credential cannot be presented as x509", func(t *testing.T) {
		mem := store.NewMemory(nil)
		mem.SetPrincipal(testTenant, testDevice, credentialIDOf(leaf),
			activeStatus(spkiThumbprintOf(leaf), "dpop"))
		a := &ForwardedCertAuthenticator{Store: mem, ClientCAs: ca.pool()}
		_, err := a.Authenticate(context.Background(), forwardedRequest(protocol.HeaderClientCert, pemChainOf(leaf)))
		if !errors.Is(err, ErrCredentialTypeMismatch) {
			t.Fatalf("err = %v, want ErrCredentialTypeMismatch", err)
		}
	})

	t.Run("no client CA refuses rather than trusts", func(t *testing.T) {
		a := &ForwardedCertAuthenticator{Store: store.NewMemory(nil), ClientCAs: nil}
		_, err := a.Authenticate(context.Background(), forwardedRequest(protocol.HeaderClientCert, pemChainOf(leaf)))
		if !errors.Is(err, ErrBadCredential) {
			t.Fatalf("err = %v, want ErrBadCredential", err)
		}
	})
}
