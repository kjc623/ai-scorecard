package drain

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/protocol"
)

// A first enrolment presents the deployment key, no client certificate, the attestation the OS
// states and the hardware identity derived from the hardware seed; the issued credential is
// stored, adopted and presented from then on.
func TestFirstEnrolmentPresentsTheDeploymentKey(t *testing.T) {
	e := newFakeEdge(t)
	var adopted *credential.Credential
	d := newTestDrainer(t, e, nil, nil, func(c *Config) {
		c.Attestation = func() *protocol.DeviceAttestation {
			return &protocol.DeviceAttestation{IntuneDeviceID: "intune-1", SerialNumber: "PF2X9K7Q"}
		}
		c.ManagedState = "managed"
		c.OnEnrolled = func(cred *credential.Credential) { adopted = cred }
	})
	if !d.EnsureEnrolled(context.Background()) {
		t.Fatal("EnsureEnrolled failed")
	}
	reqs, certs := e.enrolments()
	if len(reqs) != 1 {
		t.Fatalf("enrolments = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.DeploymentKey != "sacdk_test" || certs[0] != "" {
		t.Fatalf("first enrolment presented key %q and certificate %q", req.DeploymentKey, certs[0])
	}
	if req.Attestation == nil || req.Attestation.IntuneDeviceID != "intune-1" || req.Device.ManagedState != "managed" {
		t.Fatalf("attestation %+v managed_state %q", req.Attestation, req.Device.ManagedState)
	}
	if req.Device.HardwareIdentityHash != HardwareIdentityHash(testTenant, "smbios:test") {
		t.Fatal("the hardware identity is not derived from the hardware seed")
	}
	if adopted == nil || adopted.DeviceID != testDevice || adopted.TenantID != testTenant {
		t.Fatalf("adopted credential = %+v", adopted)
	}
	stored, err := d.creds.Load()
	if err != nil || stored.CertPEM != adopted.CertPEM {
		t.Fatalf("the issued credential was not stored: %v", err)
	}
	// A restart loads the stored credential and does not enrol again.
	again, err := New(d.cfg, d.store, d.creds, nil, time.Now)
	if err != nil || !again.Status().Enrolled {
		t.Fatalf("restart: enrolled=%v err=%v", again.Status().Enrolled, err)
	}
	if !again.EnsureEnrolled(context.Background()) {
		t.Fatal("restart could not use the stored credential")
	}
	if reqs, _ := e.enrolments(); len(reqs) != 1 {
		t.Fatalf("a restart enrolled again (%d enrolments)", len(reqs))
	}
}

// A credential past two thirds of its validity is rotated: the request presents the current
// certificate and no deployment key, and keeps the hardware identity it was first issued under.
func TestRotationPresentsTheCurrentCertificate(t *testing.T) {
	e := newFakeEdge(t)
	// Issued an hour ago for 80 minutes: past two thirds of its life, still valid.
	current := e.issued(80 * time.Minute)
	d := newTestDrainer(t, e, nil, current)
	if !d.EnsureEnrolled(context.Background()) {
		t.Fatal("EnsureEnrolled failed")
	}
	reqs, certs := e.enrolments()
	if len(reqs) != 1 {
		t.Fatalf("enrolments = %d, want one rotation", len(reqs))
	}
	if reqs[0].DeploymentKey != "" || certs[0] != testDevice {
		t.Fatalf("rotation presented key %q and certificate %q; want the certificate only", reqs[0].DeploymentKey, certs[0])
	}
	if reqs[0].Device.HardwareIdentityHash != current.HardwareIdentityHash {
		t.Fatal("rotation changed the hardware identity")
	}
	if got := d.credentialNow(); got.CertPEM == current.CertPEM || !got.NotAfter.After(current.NotAfter) {
		t.Fatal("the rotated certificate was not adopted")
	}
}

// A rotation the edge refuses keeps the current, still valid certificate in use.
func TestFailedRotationKeepsTheCurrentCertificate(t *testing.T) {
	e := newFakeEdge(t)
	current := e.issued(80 * time.Minute)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enrol", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusServiceUnavailable, []byte(`{"error":{"code":"unavailable"}}`))
	})
	e.srv.Config.Handler = mux
	d := newTestDrainer(t, e, nil, current)
	if !d.EnsureEnrolled(context.Background()) {
		t.Fatal("a failed rotation made a valid credential unusable")
	}
	if d.credentialNow().CertPEM != current.CertPEM {
		t.Fatal("the current certificate was replaced after a failed rotation")
	}
}

// An expired certificate cannot authenticate a rotation, so the device enrols again with the
// deployment key under its original hardware identity, which returns its existing device_id.
func TestExpiredCertificateEnrolsAgainWithTheDeploymentKey(t *testing.T) {
	e := newFakeEdge(t)
	expired := e.issued(30 * time.Minute) // issued an hour ago: expired half an hour ago
	d := newTestDrainer(t, e, nil, expired)
	if !d.EnsureEnrolled(context.Background()) {
		t.Fatal("EnsureEnrolled failed")
	}
	reqs, _ := e.enrolments()
	if len(reqs) != 1 || reqs[0].DeploymentKey != "sacdk_test" || reqs[0].Device.HardwareIdentityHash != expired.HardwareIdentityHash {
		t.Fatalf("re-enrolment = %+v", reqs)
	}
}

// With no deployment key and no usable credential the device cannot enrol, and says why.
func TestNoDeploymentKeyIsANamedDegradedState(t *testing.T) {
	e := newFakeEdge(t)
	d := newTestDrainer(t, e, nil, e.issued(30*time.Minute), func(c *Config) { c.DeploymentKey = "" })
	if d.EnsureEnrolled(context.Background()) {
		t.Fatal("an expired credential with no deployment key reported usable")
	}
	if st := d.Status(); st.State != protocol.StateDegraded || st.Detail != protocol.DetailCredentialExpired {
		t.Fatalf("status = %+v, want degraded/credential_expired", st)
	}
}

func TestEnrolmentRefusalIsClassified(t *testing.T) {
	e := newFakeEdge(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enrol", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, []byte(`{"error":{"code":"unknown_tenant"}}`))
	})
	e.srv.Config.Handler = mux
	d := newTestDrainer(t, e, nil, nil)
	if d.EnsureEnrolled(context.Background()) {
		t.Fatal("a refused enrolment reported success")
	}
	if st := d.Status(); st.State != protocol.StateDegraded || st.Detail != protocol.DetailUpstreamFailure {
		t.Fatalf("status = %+v, want degraded/upstream_failure", st)
	}
}

func TestHardwareIdentityHashIsPerTenantAndPerSeed(t *testing.T) {
	a := HardwareIdentityHash("t1", "smbios:1")
	if a != HardwareIdentityHash("t1", "smbios:1") {
		t.Fatal("not deterministic")
	}
	if a == HardwareIdentityHash("t2", "smbios:1") || a == HardwareIdentityHash("t1", "smbios:2") {
		t.Fatal("the tenant and the seed must both be part of the hash")
	}
}

func TestEnrolmentOSUsesTheEnrolmentVocabulary(t *testing.T) {
	for goos, want := range map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"} {
		if got := enrolmentOS(goos); got != want {
			t.Errorf("enrolmentOS(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestNewRefusesAPlainHTTPEndpoint(t *testing.T) {
	if _, err := newClient("http://edge.example", ""); err == nil {
		t.Fatal("a plain http endpoint was accepted")
	}
}
