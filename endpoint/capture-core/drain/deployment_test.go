package drain

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// enrolResponder answers /v1/enrol with a dpop credential and, when key is non-empty, a
// user_ref_key, recording the request it was sent.
func enrolResponder(t *testing.T, got *protocol.EnrolmentRequest, raw *map[string]any, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := gunzipBody(t, r)
		if err := json.Unmarshal(body, got); err != nil {
			t.Fatalf("decode enrol request: %v", err)
		}
		if raw != nil {
			_ = json.Unmarshal(body, raw)
		}
		resp, _ := json.Marshal(protocol.EnrolmentResponse{
			SchemaVersion: protocol.EnrolmentSchemaVersion,
			DeviceID:      "3f0c1a2b-0000-4000-8000-000000000001",
			TenantID:      "tenant-1",
			Region:        "eu",
			Credential:    protocol.IssuedCredential{Mode: protocol.AuthModeDPoP, JWK: &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}},
			UserRefKey:    key,
			ServerTime:    time.Now().UTC(),
		})
		writeJSON(t, w, http.StatusOK, resp)
	}
}

// The enterprise enrolment: the per-tenant deployment key instead of a single-use token, the
// attestation the operating system stated, the Intune id as the MDM id, and a hardware-seeded
// idempotency key — with no device id configured anywhere.
func TestEnrolWithDeploymentKeyCarriesAttestation(t *testing.T) {
	var got protocol.EnrolmentRequest
	raw := map[string]any{}
	attestation := &protocol.DeviceAttestation{
		IntuneDeviceID: "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55",
		EntraDeviceID:  "5c2a3b1e-77d0-4e1f-9b6a-0c1d2e3f4a5b",
		SerialNumber:   "PF2X9K7Q",
	}
	d, _ := newRawDrainer(t, Config{
		AuthMode:      protocol.AuthModeDPoP,
		DeploymentKey: "sacdk_11111111-1111-4111-8111-111111111111_secret",
		TenantID:      "11111111-1111-4111-8111-111111111111",
		HardwareSeed:  "smbios:a2219e09-2c68-6d1d-a831-345a6060843c",
		Attestation:   func() *protocol.DeviceAttestation { return attestation },
		ManagedState:  string(protocol.ManagedStateManaged),
		AgentVersion:  "test",
	}, enrolResponder(t, &got, &raw, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"))

	c, err := d.enrol(context.Background(), d.hardwareIdentity())
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if got.DeploymentKey != "sacdk_11111111-1111-4111-8111-111111111111_secret" || got.EnrolmentToken != "" {
		t.Fatalf("bootstrap = token %q key %q, want the deployment key alone", got.EnrolmentToken, got.DeploymentKey)
	}
	if _, ok := raw["enrolment_token"]; ok {
		t.Fatal("the body names enrolment_token although none is configured")
	}
	if got.Attestation == nil || *got.Attestation != *attestation {
		t.Fatalf("attestation = %+v, want %+v", got.Attestation, attestation)
	}
	if got.Device.MDMID != attestation.IntuneDeviceID {
		t.Fatalf("mdm_id = %q, want the Intune device id", got.Device.MDMID)
	}
	if got.Device.ManagedState != "managed" {
		t.Fatalf("managed_state = %q", got.Device.ManagedState)
	}
	want := HardwareIdentityHash("11111111-1111-4111-8111-111111111111", "", "smbios:a2219e09-2c68-6d1d-a831-345a6060843c")
	if got.Device.HardwareIdentityHash != want {
		t.Fatalf("hardware_identity_hash = %q, want the hardware-seeded %q", got.Device.HardwareIdentityHash, want)
	}
	if c.UserRefKey != "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" {
		t.Fatalf("credential user_ref_key = %q, want the issued key kept with the credential", c.UserRefKey)
	}
	if c.DeviceID != "3f0c1a2b-0000-4000-8000-000000000001" {
		t.Fatalf("device id = %q, want the server-minted id", c.DeviceID)
	}
}

// Absent facts are omitted, not sent empty, and a configured MDM id keeps its place as the seed and
// the mdm_id, so the lab profiles' hardware identity does not change.
func TestEnrolOmitsAnEmptyAttestationAndKeepsAConfiguredMDMID(t *testing.T) {
	var got protocol.EnrolmentRequest
	raw := map[string]any{}
	d, _ := newRawDrainer(t, Config{
		AuthMode:       protocol.AuthModeDPoP,
		EnrolmentToken: "tok-123",
		TenantID:       "tenant-1",
		DeviceID:       "device-1",
		MDMID:          "sac-dev-windows",
		HardwareSeed:   "smbios:a2219e09-2c68-6d1d-a831-345a6060843c",
		Attestation:    func() *protocol.DeviceAttestation { return nil },
		AgentVersion:   "test",
	}, enrolResponder(t, &got, &raw, ""))
	if _, err := d.enrol(context.Background(), d.hardwareIdentity()); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if _, ok := raw["attestation"]; ok {
		t.Fatal("an attestation object was sent with nothing in it")
	}
	if _, ok := raw["deployment_key"]; ok {
		t.Fatal("a deployment_key was sent although none is configured")
	}
	if got.Device.MDMID != "sac-dev-windows" {
		t.Fatalf("mdm_id = %q, want the configured value", got.Device.MDMID)
	}
	if got.Device.HardwareIdentityHash != HardwareIdentityHash("tenant-1", "device-1", "sac-dev-windows") {
		t.Fatal("a configured MDM id no longer seeds the hardware identity; the lab device would enrol as a new device")
	}
}

// Without a configured device id or MDM id, two devices of one tenant must not share an idempotency
// key; the old fallback hashed "device:" + "" for every one of them.
func TestHardwareIdentityDiffersPerDeviceWithoutADeviceID(t *testing.T) {
	a := (&Drainer{cfg: Config{TenantID: "t", HardwareSeed: "smbios:a2219e09-2c68-6d1d-a831-345a6060843c"}}).hardwareIdentity()
	b := (&Drainer{cfg: Config{TenantID: "t", HardwareSeed: "smbios:5f1d2c3b-0000-4000-8000-00000000abcd"}}).hardwareIdentity()
	if a == b {
		t.Fatal("two devices with different hardware enrol under one hardware_identity_hash")
	}
}

func TestDeploymentKeyEnrolsWhenNoCredentialExists(t *testing.T) {
	var got protocol.EnrolmentRequest
	d, _ := newRawDrainer(t, Config{
		AuthMode:      protocol.AuthModeDPoP,
		DeploymentKey: "sacdk_key",
		TenantID:      "tenant-1",
		HardwareSeed:  "machine:75a3376a-ec7f-48aa-88a9-d7dad86c0bb5",
		AgentVersion:  "test",
	}, enrolResponder(t, &got, nil, ""))
	d.creds = newTestCredentialStore(t)
	if !d.ready(context.Background()) {
		t.Fatal("a configured deployment key did not enrol the device")
	}
	if got.DeploymentKey != "sacdk_key" {
		t.Fatalf("deployment key = %q", got.DeploymentKey)
	}
}

func TestNewRefusesBothBootstrapCredentials(t *testing.T) {
	_, err := New(Config{
		Endpoint:       "https://ingest.example.invalid",
		AuthMode:       protocol.AuthModeDPoP,
		EnrolmentToken: "tok",
		DeploymentKey:  "sacdk_key",
	}, nil, nil, nil, time.Now)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("New with a token and a key = %v, want a refusal", err)
	}
}

func TestEnrolmentOSUsesTheEnrolmentVocabulary(t *testing.T) {
	for goos, want := range map[string]string{"windows": "windows", "linux": "linux", "darwin": "macos"} {
		if got := enrolmentOS(goos); got != want {
			t.Errorf("enrolmentOS(%q) = %q, want %q", goos, got, want)
		}
	}
}
