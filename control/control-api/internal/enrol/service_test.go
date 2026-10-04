package enrol_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/jose"
	"github.com/shadow-ai-capture/control-api/internal/signer"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

const (
	tenantA = "11111111-1111-7111-8111-111111111111"
	tenantB = "22222222-2222-7222-8222-222222222222"
	regionA = "eu-west"
	regionB = "us-east"
	htu     = "https://ingest.eu.example.com/v1/enrol"
)

// rig is the in-memory service under test.
type rig struct {
	store *store.Memory
	ca    *signer.LocalCA
	svc   *enrol.Service
	now   time.Time
}

func newRig(t *testing.T, tenant store.Tenant, region string) *rig {
	t.Helper()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	st := store.NewMemory()
	st.AddTenant(tenant)
	ca, err := signer.NewLocalCA(nil, nil, 90*24*time.Hour, []string{"ingest.eu.example.com"})
	if err != nil {
		t.Fatalf("NewLocalCA: %v", err)
	}
	svc, err := enrol.New(st, ca, enrol.Config{
		Region:        region,
		CredentialTTL: 90 * 24 * time.Hour,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("enrol.New: %v", err)
	}
	return &rig{store: st, ca: ca, svc: svc, now: now}
}

// addToken mints a plaintext token for the tenant and stores only its hash, as the schema does.
func (r *rig) addToken(t *testing.T, tenantID string) string {
	t.Helper()
	plaintext, err := enrol.MintEnrolmentToken(tenantID)
	if err != nil {
		t.Fatalf("MintEnrolmentToken: %v", err)
	}
	r.store.AddEnrolmentToken(store.EnrolmentToken{
		TenantID:  tenantID,
		TokenHash: enrol.HashEnrolmentToken(plaintext),
		IssuedAt:  r.now,
		ExpiresAt: r.now.Add(time.Hour),
	})
	return plaintext
}

func activeTenant(id, region string) store.Tenant {
	return store.Tenant{TenantID: id, Status: "active", IngestEnabled: true, ResidencyRegion: region}
}

func x509Request(token string, hwid string) (protocol.EnrolmentRequest, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return protocol.EnrolmentRequest{}, nil, err
	}
	tmpl := &x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return protocol.EnrolmentRequest{}, nil, err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	return protocol.EnrolmentRequest{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		EnrolmentToken: token,
		Mode:           protocol.AuthModeX509,
		CSR:            string(csrPEM),
		Device: protocol.DeviceInfo{
			OS: "windows", OSVersion: "11.0.26100", AgentVersion: "1.4.2", HardwareIdentityHash: hwid,
		},
	}, key, nil
}

func dpopRequest(token string, hwid string, now time.Time) (protocol.EnrolmentRequest, *ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return protocol.EnrolmentRequest{}, nil, "", err
	}
	jwk, err := jose.JWKFromPublic(&key.PublicKey)
	if err != nil {
		return protocol.EnrolmentRequest{}, nil, "", err
	}
	proof, err := dpopProof(key, jwk, "POST", htu, now)
	if err != nil {
		return protocol.EnrolmentRequest{}, nil, "", err
	}
	req := protocol.EnrolmentRequest{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		EnrolmentToken: token,
		Mode:           protocol.AuthModeDPoP,
		JWK:            &jwk,
		Device: protocol.DeviceInfo{
			OS: "macos", AgentVersion: "1.4.2", HardwareIdentityHash: hwid,
		},
	}
	return req, key, proof, nil
}

func dpopProof(key *ecdsa.PrivateKey, jwk protocol.JWK, htm, htu string, now time.Time) (string, error) {
	header := map[string]any{"typ": jose.TypDPoP, "alg": jose.AlgES256, "jwk": jwk}
	claims := map[string]any{"htm": htm, "htu": htu, "iat": now.Unix(), "jti": "jti-" + now.Format(time.RFC3339Nano)}
	return jose.SignES256(header, claims, key)
}

// TestX509EnrolmentIssuesAVerifiableLeaf is the x509 half of ADR 0020 decision 3: a CSR goes in, a
// leaf whose chain verifies, whose CN is the device, whose OU is the tenant and whose EKU is
// clientAuth comes out.
func TestX509EnrolmentIssuesAVerifiableLeaf(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)
	token := r.addToken(t, tenantA)
	req, _, err := x509Request(token, "hw-x509")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if !store.IsUUID(resp.DeviceID) {
		t.Fatalf("device_id %q is not a uuid", resp.DeviceID)
	}
	if resp.TenantID != tenantA || resp.Region != regionA {
		t.Fatalf("response tenant/region = %s/%s, want %s/%s", resp.TenantID, resp.Region, tenantA, regionA)
	}
	if resp.Reenrolled {
		t.Fatal("a first enrolment reported reenrolled")
	}
	if resp.Credential.Mode != protocol.AuthModeX509 || resp.Credential.CertPEM == "" {
		t.Fatalf("credential = %+v, want an x509 leaf", resp.Credential)
	}

	leaf := parsePEMCert(t, resp.Credential.CertPEM)
	if leaf.Subject.CommonName != resp.DeviceID {
		t.Errorf("leaf CN = %q, want the device id %q", leaf.Subject.CommonName, resp.DeviceID)
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != tenantA {
		t.Errorf("leaf OU = %v, want [%s]", leaf.Subject.OrganizationalUnit, tenantA)
	}
	hasClientAuth := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		t.Errorf("leaf EKU = %v, want clientAuth", leaf.ExtKeyUsage)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "ingest.eu.example.com" {
		t.Errorf("leaf SAN = %v, want the configured SAN", leaf.DNSNames)
	}
	if !resp.Credential.NotAfter.Equal(leaf.NotAfter) {
		t.Errorf("not_after = %s, leaf says %s", resp.Credential.NotAfter, leaf.NotAfter)
	}

	// The chain verifies against the CA the service used, with clientAuth as the purpose.
	roots := x509.NewCertPool()
	for _, chainPEM := range resp.Credential.ChainPEM {
		if !roots.AppendCertsFromPEM([]byte(chainPEM)) {
			t.Fatalf("chain PEM did not parse")
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("issued leaf does not verify against its chain: %v", err)
	}

	// The credential row carries the x509 mode and the SPKI thumbprint, not a JWK.
	creds := r.store.Credentials()
	if len(creds) != 1 {
		t.Fatalf("credentials = %d, want 1", len(creds))
	}
	if creds[0].Type != protocol.AuthModeX509 || creds[0].PublicKeyThumbprint == "" || len(creds[0].PublicKeyJWK) != 0 {
		t.Fatalf("stored credential = %+v, want an x509 row with a thumbprint and no jwk", creds[0])
	}
}

// TestLinuxEnrolmentIsAccepted pins the third endpoint platform in the closed OS set. Linux is a
// supported deployment target (database/schema.sql admits it), so the service must not refuse it;
// this would have caught the constraint and the validator drifting apart.
func TestLinuxEnrolmentIsAccepted(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)
	token := r.addToken(t, tenantA)
	req, _, err := x509Request(token, "hw-linux")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Device.OS = "linux"
	req.Device.OSVersion = "6.11.0"
	resp, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu})
	if err != nil {
		t.Fatalf("Enrol(linux): %v", err)
	}
	if !store.IsUUID(resp.DeviceID) {
		t.Fatalf("device_id %q is not a uuid", resp.DeviceID)
	}
}

// TestUnknownOSIsRefused keeps the set closed: a value outside it is still a schema violation.
func TestUnknownOSIsRefused(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)
	token := r.addToken(t, tenantA)
	req, _, err := x509Request(token, "hw-unknown")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Device.OS = "solaris"
	if _, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu}); err == nil {
		t.Fatal("an unknown device.os was accepted")
	}
}

// TestDPoPEnrolmentStoresTheJWKAndThumbprint is the dpop half: the registered public key is stored
// and public_key_thumbprint is its RFC 7638 value.
func TestDPoPEnrolmentStoresTheJWKAndThumbprint(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)
	token := r.addToken(t, tenantA)
	req, _, proof, err := dpopRequest(token, "hw-dpop", r.now)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	wantThumb, err := req.JWK.Thumbprint()
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}

	resp, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, Proof: proof, HTM: "POST", HTU: htu})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if resp.Credential.Mode != protocol.AuthModeDPoP || resp.Credential.JWK == nil {
		t.Fatalf("credential = %+v, want a dpop credential carrying the jwk", resp.Credential)
	}
	if got, _ := resp.Credential.JWK.Thumbprint(); got != wantThumb {
		t.Errorf("returned jwk thumbprint = %q, want %q", got, wantThumb)
	}
	creds := r.store.Credentials()
	if len(creds) != 1 {
		t.Fatalf("credentials = %d, want 1", len(creds))
	}
	if creds[0].Type != protocol.AuthModeDPoP {
		t.Errorf("stored type = %q, want dpop", creds[0].Type)
	}
	if creds[0].PublicKeyThumbprint != wantThumb {
		t.Errorf("stored thumbprint = %q, want %q", creds[0].PublicKeyThumbprint, wantThumb)
	}
	if len(creds[0].PublicKeyJWK) == 0 {
		t.Error("stored dpop credential carries no public_key_jwk")
	}
}

// TestDPoPEnrolmentRefusesAMissingOrWrongProof proves possession is required: no proof, and a proof
// for a different key, are both refused before any credential is written.
func TestDPoPEnrolmentRefusesAMissingOrWrongProof(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)
	token := r.addToken(t, tenantA)
	req, _, _, err := dpopRequest(token, "hw-dpop", r.now)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if _, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu}); !isCode(err, apierr.CodeInvalidProof) {
		t.Fatalf("missing proof: err = %v, want code %s", err, apierr.CodeInvalidProof)
	}

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	otherJWK, _ := jose.JWKFromPublic(&other.PublicKey)
	badProof, _ := dpopProof(other, otherJWK, "POST", htu, r.now)
	if _, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, Proof: badProof, HTM: "POST", HTU: htu}); !isCode(err, apierr.CodeInvalidProof) {
		t.Fatalf("proof for another key: err = %v, want code %s", err, apierr.CodeInvalidProof)
	}
	if len(r.store.Credentials()) != 0 {
		t.Fatal("a credential was written despite a failed proof of possession")
	}
}

// TestReenrolmentIsIdempotentOnHardwareIdentity is C11: a re-image with the same hardware identity
// returns the existing device_id with reenrolled true and creates no second device row.
func TestReenrolmentIsIdempotentOnHardwareIdentity(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)

	first, _, err := x509Request(r.addToken(t, tenantA), "hw-idem")
	if err != nil {
		t.Fatalf("build first request: %v", err)
	}
	resp1, err := r.svc.Enrol(context.Background(), enrol.Input{Request: first, HTM: "POST", HTU: htu})
	if err != nil {
		t.Fatalf("first Enrol: %v", err)
	}

	second, _, err := x509Request(r.addToken(t, tenantA), "hw-idem")
	if err != nil {
		t.Fatalf("build second request: %v", err)
	}
	resp2, err := r.svc.Enrol(context.Background(), enrol.Input{Request: second, HTM: "POST", HTU: htu})
	if err != nil {
		t.Fatalf("second Enrol: %v", err)
	}
	if resp2.DeviceID != resp1.DeviceID {
		t.Fatalf("re-enrolment returned device %s, want the existing %s", resp2.DeviceID, resp1.DeviceID)
	}
	if !resp2.Reenrolled {
		t.Fatal("re-enrolment did not report reenrolled")
	}
	if n := len(r.store.Devices()); n != 1 {
		t.Fatalf("devices = %d, want 1 (no duplicate row for one hardware identity)", n)
	}
	// A rotation revokes the previous credential and leaves exactly one live one.
	live, err := r.store.DeviceCredentialByDevice(context.Background(), tenantA, resp1.DeviceID)
	if err != nil {
		t.Fatalf("live credential: %v", err)
	}
	revoked := 0
	for _, c := range r.store.Credentials() {
		if c.RevokedAt != nil {
			revoked++
		}
	}
	if revoked != 1 || live.RevokedAt != nil {
		t.Fatalf("rotated credentials: revoked=%d live=%+v, want exactly the previous one revoked", revoked, live)
	}
}

// TestRevokedDeviceIsRefusedAFreshIdentity covers the ADR's "revocation is not bypassable by
// re-imaging": a revoked device may not re-enrol even with a valid token and matching identity.
func TestRevokedDeviceIsRefusedAFreshIdentity(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)
	revokedAt := r.now.Add(-time.Hour)
	r.store.AddDevice(store.Device{
		TenantID: tenantA, DeviceID: "33333333-3333-7333-8333-333333333333",
		OS: "windows", HardwareIdentityHash: "hw-revoked", RevokedAt: &revokedAt,
	})
	req, _, err := x509Request(r.addToken(t, tenantA), "hw-revoked")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	_, err = r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu})
	if !isCode(err, apierr.CodeRevokedDevice) {
		t.Fatalf("err = %v, want code %s", err, apierr.CodeRevokedDevice)
	}
	if len(r.store.Credentials()) != 0 {
		t.Fatal("a revoked device was issued a credential")
	}
}

// TestRegionMismatchFailsClosed is docs/02 §12: a deployment that is not the tenant's pinned region
// refuses rather than writing cross-region.
func TestRegionMismatchFailsClosed(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionB), regionA)
	req, _, err := x509Request(r.addToken(t, tenantA), "hw-region")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	_, err = r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu})
	if !isCode(err, apierr.CodeRegionMismatch) {
		t.Fatalf("err = %v, want code %s", err, apierr.CodeRegionMismatch)
	}
}

// TestEnrolmentTokenIsSingleUseAndUnsupportedVersionsAreRefused covers the two cheap guards: a used
// token is refused, and a schema_version outside the advertised set is refused with the supported
// list in the detail.
func TestEnrolmentTokenIsSingleUseAndUnsupportedVersionsAreRefused(t *testing.T) {
	r := newRig(t, activeTenant(tenantA, regionA), regionA)
	token := r.addToken(t, tenantA)
	req, _, err := x509Request(token, "hw-single")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req, HTM: "POST", HTU: htu}); err != nil {
		t.Fatalf("first Enrol: %v", err)
	}
	req2, _, err := x509Request(token, "hw-single-2")
	if err != nil {
		t.Fatalf("build second request: %v", err)
	}
	if _, err := r.svc.Enrol(context.Background(), enrol.Input{Request: req2, HTM: "POST", HTU: htu}); !isCode(err, apierr.CodeEnrolmentTokenInvalid) {
		t.Fatalf("reused token: err = %v, want code %s", err, apierr.CodeEnrolmentTokenInvalid)
	}

	bad := req2
	bad.EnrolmentToken = r.addToken(t, tenantA)
	bad.SchemaVersion = "9.9"
	if _, err := r.svc.Enrol(context.Background(), enrol.Input{Request: bad, HTM: "POST", HTU: htu}); !isCode(err, apierr.CodeUnsupportedSchemaVersion) {
		t.Fatalf("unsupported version: err = %v, want code %s", err, apierr.CodeUnsupportedSchemaVersion)
	}
}

func parsePEMCert(t *testing.T, pemText string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		t.Fatal("no PEM block in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func isCode(err error, code string) bool {
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == code
}
