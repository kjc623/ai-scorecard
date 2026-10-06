package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEnrolmentRoundTrip(t *testing.T) {
	req := EnrolmentRequest{
		SchemaVersion: EnrolmentSchemaVersion,
		DeploymentKey: "sac_dk_example",
		CSR:           "-----BEGIN CERTIFICATE REQUEST-----\nMIIB...\n-----END CERTIFICATE REQUEST-----",
		Device: DeviceInfo{
			OS:                   "windows",
			OSVersion:            "11.0.26100",
			AgentVersion:         "1.4.2",
			HardwareIdentityHash: "sha256:9f2c",
		},
		Attestation: &DeviceAttestation{IntuneDeviceID: "intune-abc"},
	}
	assertJSONRoundTrip(t, req)

	rotation := EnrolmentRequest{SchemaVersion: EnrolmentSchemaVersion, CSR: req.CSR, Device: req.Device}
	b, err := json.Marshal(rotation)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := decodeKeys(t, b)["deployment_key"]; present {
		t.Fatalf("a rotation request serialised an empty deployment_key: %s", b)
	}

	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	resp := EnrolmentResponse{
		SchemaVersion: EnrolmentSchemaVersion,
		DeviceID:      "d-1",
		TenantID:      "t-1",
		Region:        "eastus",
		Reenrolled:    true,
		Credential: IssuedCredential{
			CertPEM:  "-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----",
			ChainPEM: []string{"-----BEGIN CERTIFICATE-----\nCA...\n-----END CERTIFICATE-----"},
			NotAfter: now.Add(90 * 24 * time.Hour),
		},
		PolicyETag: "bundle-7",
		ServerTime: now,
	}
	assertJSONRoundTrip(t, resp)
}

func assertJSONRoundTrip[T any](t *testing.T, v T) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var back T
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	if !reflect.DeepEqual(v, back) {
		t.Fatalf("%T did not round-trip:\n before: %#v\n after:  %#v\n json: %s", v, v, back, b)
	}
}

func decodeKeys(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	return m
}
