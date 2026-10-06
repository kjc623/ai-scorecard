package enrol_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deviceca/devicecatest"
	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

const (
	tenantA = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
	tenantB = "6b4d1ef1-8f68-4b22-9111-1111111e4b12"
	regionA = "eu-west"
	regionB = "us-east"
)

func activeTenant(id, region string) store.Tenant {
	return store.Tenant{TenantID: id, Status: "active", IngestEnabled: true, ResidencyRegion: region}
}

func request(t *testing.T, key, hwid string) protocol.EnrolmentRequest {
	t.Helper()
	return protocol.EnrolmentRequest{
		SchemaVersion: protocol.EnrolmentSchemaVersion,
		DeploymentKey: key,
		CSR:           devicecatest.NewDeviceKey(t).CSRPEM,
		Device: protocol.DeviceInfo{
			OS: "windows", OSVersion: "11.0.26100", AgentVersion: "1.4.2", HardwareIdentityHash: hwid,
		},
	}
}

func parseCert(t *testing.T, pemText string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		t.Fatal("no PEM block in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func isCode(err error, code string) bool {
	var e *apierr.Error
	return errors.As(err, &e) && e.Code == code
}

func TestFirstEnrolmentIssuesAVerifiableCertificate(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	resp, err := r.enrol(request(t, key, "hw-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !store.IsUUID(resp.DeviceID) || resp.TenantID != tenantA || resp.Region != regionA || resp.Reenrolled {
		t.Fatalf("response = %+v", resp)
	}
	leaf := parseCert(t, resp.Credential.CertPEM)
	if leaf.Subject.CommonName != resp.DeviceID ||
		len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != tenantA {
		t.Fatalf("subject = %v, want CN=device OU=tenant", leaf.Subject)
	}
	if !resp.Credential.NotAfter.Equal(leaf.NotAfter) {
		t.Errorf("not_after = %s, certificate says %s", resp.Credential.NotAfter, leaf.NotAfter)
	}
	if err := r.ca.CA.Verify([]*x509.Certificate{leaf}, r.now); err != nil {
		t.Fatalf("the issued certificate does not verify against the device CA: %v", err)
	}
	if len(resp.Credential.ChainPEM) != 1 || resp.Credential.ChainPEM[0] != string(r.ca.CertPEM) {
		t.Error("the chain is not the device CA certificate")
	}
	creds := r.store.Credentials()
	if len(creds) != 1 {
		t.Fatalf("credentials = %d, want 1", len(creds))
	}
	c := creds[0]
	if c.CredentialID != protocol.CredentialID(leaf.Raw) || c.DeviceID != resp.DeviceID ||
		c.PublicKeyThumbprint != enrol.SPKIThumbprint(leaf.RawSubjectPublicKeyInfo) || !c.ExpiresAt.Equal(leaf.NotAfter) {
		t.Fatalf("credential = %+v", c)
	}
}

func TestOperatingSystems(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	for _, os := range []string{"windows", "macos", "linux"} {
		req := request(t, key, "hw-"+os)
		req.Device.OS = os
		if _, err := r.enrol(req); err != nil {
			t.Errorf("%s: %v", os, err)
		}
	}
	req := request(t, key, "hw-solaris")
	req.Device.OS = "solaris"
	if _, err := r.enrol(req); !isCode(err, apierr.CodeSchemaViolation) {
		t.Errorf("solaris: err = %v", err)
	}
}

func TestRequestShapeIsCheckedBeforeAnythingIsWritten(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	version := request(t, key, "hw-v")
	version.SchemaVersion = "9.9"
	if _, err := r.enrol(version); !isCode(err, apierr.CodeUnsupportedSchemaVersion) {
		t.Errorf("unsupported version: err = %v", err)
	}
	for name, csr := range map[string]string{
		"missing":   "",
		"not PEM":   "not a csr",
		"wrong PEM": string(r.ca.CertPEM),
	} {
		req := request(t, key, "hw-"+name)
		req.CSR = csr
		if _, err := r.enrol(req); !isCode(err, apierr.CodeInvalidCSR) {
			t.Errorf("%s csr: err = %v", name, err)
		}
	}
	if len(r.store.Devices()) != 0 || len(r.store.Credentials()) != 0 {
		t.Fatal("a refused request wrote a device or a credential")
	}
}

func TestNoCredentialIsRefused(t *testing.T) {
	r := newRig(t, nil)
	if _, err := r.enrol(request(t, "", "hw-anon")); !isCode(err, apierr.CodeUnauthenticated) {
		t.Fatalf("err = %v, want %s", err, apierr.CodeUnauthenticated)
	}
}

func TestReenrolmentIsIdempotentOnHardwareIdentity(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	first, err := r.enrol(request(t, key, "hw-idem"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.enrol(request(t, key, "hw-idem"))
	if err != nil {
		t.Fatal(err)
	}
	if second.DeviceID != first.DeviceID || !second.Reenrolled {
		t.Fatalf("second enrolment = %s (reenrolled %v), want %s", second.DeviceID, second.Reenrolled, first.DeviceID)
	}
	if n := len(r.store.Devices()); n != 1 {
		t.Fatalf("devices = %d, want 1", n)
	}
	assertOneLiveCredential(t, r, first.DeviceID, parseCert(t, second.Credential.CertPEM))
}

func assertOneLiveCredential(t *testing.T, r *rig, deviceID string, want *x509.Certificate) {
	t.Helper()
	live := 0
	for _, c := range r.store.Credentials() {
		if c.DeviceID != deviceID || c.RevokedAt != nil {
			continue
		}
		live++
		if c.CredentialID != protocol.CredentialID(want.Raw) {
			t.Fatalf("the live credential is %s, not the newest certificate", c.CredentialID)
		}
	}
	if live != 1 {
		t.Fatalf("live credentials = %d, want 1", live)
	}
}

func TestRotationWithTheCurrentCertificate(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	first, err := r.enrol(request(t, key, "hw-rot"))
	if err != nil {
		t.Fatal(err)
	}
	cur := enrol.Current{TenantID: tenantA, DeviceID: first.DeviceID,
		CredentialID: protocol.CredentialID(parseCert(t, first.Credential.CertPEM).Raw)}
	rotate := func(hwid string, cur enrol.Current) (protocol.EnrolmentResponse, error) {
		return r.svc.Enrol(context.Background(), enrol.Input{
			Request: request(t, "", hwid),
			Current: func() (enrol.Current, error) { return cur, nil },
		})
	}

	rotated, err := rotate("hw-rot", cur)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.DeviceID != first.DeviceID || !rotated.Reenrolled || rotated.Credential.CertPEM == first.Credential.CertPEM {
		t.Fatalf("rotation = %+v", rotated)
	}
	assertOneLiveCredential(t, r, first.DeviceID, parseCert(t, rotated.Credential.CertPEM))

	if _, err := rotate("hw-other", cur); !isCode(err, apierr.CodeHardwareConflict) {
		t.Errorf("another hardware identity: err = %v", err)
	}
	unknown := cur
	unknown.DeviceID = "77777777-7777-4777-8777-777777777777"
	if _, err := rotate("", unknown); !isCode(err, apierr.CodeRevokedDevice) {
		t.Errorf("unknown device: err = %v", err)
	}
	wrongRegion := cur
	wrongRegion.TenantID = tenantB
	r.store.AddTenant(activeTenant(tenantB, regionB))
	if _, err := rotate("", wrongRegion); !isCode(err, apierr.CodeRegionMismatch) {
		t.Errorf("tenant pinned elsewhere: err = %v", err)
	}
	r.store.RevokeDevice(tenantA, first.DeviceID, r.now)
	if _, err := rotate("hw-rot", cur); !isCode(err, apierr.CodeRevokedDevice) {
		t.Errorf("revoked device: err = %v", err)
	}

	refused := apierr.New(401, apierr.CodeInvalidClientCert, "no")
	_, err = r.svc.Enrol(context.Background(), enrol.Input{
		Request: request(t, "", "hw-rot"),
		Current: func() (enrol.Current, error) { return enrol.Current{}, refused },
	})
	if !errors.Is(err, refused) {
		t.Errorf("the certificate's refusal was not passed through: %v", err)
	}
}

func TestADeploymentKeyIsUsedEvenWithACertificateOnTheConnection(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	called := false
	_, err := r.svc.Enrol(context.Background(), enrol.Input{
		Request: request(t, key, "hw-both"),
		Current: func() (enrol.Current, error) { called = true; return enrol.Current{}, errors.New("expired") },
	})
	if err != nil || called {
		t.Fatalf("err = %v, certificate resolved = %v; a deployment-key enrolment must not depend on the certificate", err, called)
	}
}

func TestRevokedDeviceIsRefusedAFreshIdentity(t *testing.T) {
	r := newRig(t, nil)
	key, _ := r.addKey(t, tenantA, nil)
	revokedAt := r.now.Add(-time.Hour)
	r.store.AddDevice(store.Device{TenantID: tenantA, DeviceID: "33333333-3333-4333-8333-333333333333",
		OS: "windows", HardwareIdentityHash: "hw-revoked", RevokedAt: &revokedAt})
	if _, err := r.enrol(request(t, key, "hw-revoked")); !isCode(err, apierr.CodeRevokedDevice) {
		t.Fatalf("err = %v, want %s", err, apierr.CodeRevokedDevice)
	}
	if len(r.store.Credentials()) != 0 {
		t.Fatal("a revoked device was issued a certificate")
	}
}

func TestRegionMismatchFailsClosed(t *testing.T) {
	r := newRig(t, nil)
	r.store.AddTenant(activeTenant(tenantB, regionB))
	key, _ := r.addKey(t, tenantB, nil)
	if _, err := r.enrol(request(t, key, "hw-region")); !isCode(err, apierr.CodeRegionMismatch) {
		t.Fatalf("err = %v, want %s", err, apierr.CodeRegionMismatch)
	}
}

func TestIdentitySettingGatesTheStoredHostname(t *testing.T) {
	for _, tc := range []struct {
		setting  protocol.DeviceIdentity
		wantHost string
		wantHash string
	}{
		{protocol.DeviceIdentityClear, "LAPTOP-7", ""},
		{protocol.DeviceIdentityHashed, "", "hash-7"},
	} {
		t.Run(string(tc.setting), func(t *testing.T) {
			r := newRig(t, nil)
			tenant := activeTenant(tenantA, regionA)
			tenant.DeviceIdentity = tc.setting
			r.store.AddTenant(tenant)
			key, _ := r.addKey(t, tenantA, nil)
			req := request(t, key, "hw-identity")
			req.Device.Hostname, req.Device.HostnameHash, req.Device.ManagedState = "LAPTOP-7", "hash-7", "managed"
			resp, err := r.enrol(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.DeviceIdentity != tc.setting {
				t.Errorf("device_identity = %q, want %q", resp.DeviceIdentity, tc.setting)
			}
			d, _ := r.store.Device(context.Background(), tenantA, resp.DeviceID)
			if d.Hostname != tc.wantHost || d.HostnameHash != tc.wantHash || d.ManagedState != "managed" || d.AgentVersion != "1.4.2" {
				t.Errorf("stored device = %+v", d)
			}
		})
	}
}
