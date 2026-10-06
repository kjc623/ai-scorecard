package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

const (
	tenant = "22222222-2222-4222-8222-222222222222"
	device = "33333333-3333-4333-8333-333333333333"
	region = "eastus"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test device CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return ca{cert: cert, key: key}
}

func (c ca) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// leaf issues a device certificate the way control-api does: device id as CN, tenant id as OU.
func (c ca) leaf(t *testing.T, cn, ou string, usage x509.ExtKeyUsage) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: []string{ou}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(12 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func pemOf(cert *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

// statuses answers PrincipalStatus from a fixed value and records what it was asked.
type statuses struct {
	st     store.PrincipalStatus
	err    error
	gotKey [3]string
}

func (s *statuses) PrincipalStatus(_ context.Context, tenantID, deviceID, credentialID string) (store.PrincipalStatus, error) {
	s.gotKey = [3]string{tenantID, deviceID, credentialID}
	return s.st, s.err
}

func active() store.PrincipalStatus {
	return store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: region,
		DeviceKnown: true, CredentialKnown: true, CredentialExpiry: now.Add(90 * 24 * time.Hour),
	}
}

func authenticate(t *testing.T, a *Certificates, header string) (Principal, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/events", nil)
	if header != "" {
		r.Header.Set(protocol.HeaderClientCert, header)
	}
	return a.Authenticate(context.Background(), r)
}

func TestForwardedCertificateAuthenticates(t *testing.T) {
	authority := newCA(t)
	leaf := authority.leaf(t, device, tenant, x509.ExtKeyUsageClientAuth)
	for name, header := range map[string]string{
		"url-encoded, %20 spaces": url.PathEscape(pemOf(leaf)),
		"url-encoded, + spaces":   url.QueryEscape(pemOf(leaf)),
		"raw PEM":                 pemOf(leaf),
	} {
		t.Run(name, func(t *testing.T) {
			st := &statuses{st: active()}
			a := &Certificates{Store: st, Roots: authority.pool(), Region: region, Now: func() time.Time { return now }}
			p, err := authenticate(t, a, header)
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			want := Principal{TenantID: tenant, DeviceID: device, CredentialID: protocol.CredentialID(leaf.Raw)}
			if p != want {
				t.Errorf("principal = %+v, want %+v", p, want)
			}
			if st.gotKey != [3]string{want.TenantID, want.DeviceID, want.CredentialID} {
				t.Errorf("status read for %v", st.gotKey)
			}
		})
	}
}

func TestCertificatesThatAreNotDeviceCredentialsAreRefused(t *testing.T) {
	authority, other := newCA(t), newCA(t)
	cases := map[string]string{
		"no header":              "",
		"garbage":                "not a certificate",
		"another CA":             url.PathEscape(pemOf(other.leaf(t, device, tenant, x509.ExtKeyUsageClientAuth))),
		"server certificate":     url.PathEscape(pemOf(authority.leaf(t, device, tenant, x509.ExtKeyUsageServerAuth))),
		"CN is not a device id":  url.PathEscape(pemOf(authority.leaf(t, "laptop-7", tenant, x509.ExtKeyUsageClientAuth))),
		"OU is not a tenant id":  url.PathEscape(pemOf(authority.leaf(t, device, "contoso", x509.ExtKeyUsageClientAuth))),
		"the CA itself, no leaf": url.PathEscape(pemOf(authority.cert)),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			st := &statuses{st: active()}
			a := &Certificates{Store: st, Roots: authority.pool(), Region: region, Now: func() time.Time { return now }}
			_, err := authenticate(t, a, header)
			want := ErrBadCredential
			if header == "" {
				want = ErrNoCredential
			}
			if !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
			if st.gotKey != [3]string{} {
				t.Error("the store was consulted for a certificate that failed verification")
			}
		})
	}
}

func TestCredentialStatusIsCheckedOnEveryRequest(t *testing.T) {
	authority := newCA(t)
	header := url.PathEscape(pemOf(authority.leaf(t, device, tenant, x509.ExtKeyUsageClientAuth)))
	revokedAt := now.Add(-time.Minute)
	cases := map[string]struct {
		mutate func(*store.PrincipalStatus)
		want   error
	}{
		"unknown tenant":     {func(s *store.PrincipalStatus) { *s = store.PrincipalStatus{} }, store.ErrUnknownTenant},
		"ingest disabled":    {func(s *store.PrincipalStatus) { s.IngestEnabled = false }, store.ErrTenantSuspended},
		"another region":     {func(s *store.PrincipalStatus) { s.TenantRegion = "westeurope" }, store.ErrRegionMismatch},
		"unknown device":     {func(s *store.PrincipalStatus) { s.DeviceKnown = false }, store.ErrCredentialUnknown},
		"revoked device":     {func(s *store.PrincipalStatus) { s.DeviceRevokedAt = &revokedAt }, store.ErrDeviceRevoked},
		"unknown credential": {func(s *store.PrincipalStatus) { s.CredentialKnown = false }, store.ErrCredentialUnknown},
		"revoked credential": {func(s *store.PrincipalStatus) { s.CredentialRevoked = &revokedAt }, store.ErrCredentialRevoked},
		"expired credential": {func(s *store.PrincipalStatus) { s.CredentialExpiry = now }, store.ErrCredentialExpired},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st := active()
			c.mutate(&st)
			a := &Certificates{Store: &statuses{st: st}, Roots: authority.pool(), Region: region, Now: func() time.Time { return now }}
			if _, err := authenticate(t, a, header); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}

	t.Run("status read fails", func(t *testing.T) {
		boom := errors.New("connection refused")
		a := &Certificates{Store: &statuses{err: boom}, Roots: authority.pool(), Region: region, Now: func() time.Time { return now }}
		_, err := authenticate(t, a, header)
		if !errors.Is(err, boom) || errors.Is(err, ErrBadCredential) {
			t.Fatalf("err = %v, want the store failure, not a credential verdict", err)
		}
	})
}
