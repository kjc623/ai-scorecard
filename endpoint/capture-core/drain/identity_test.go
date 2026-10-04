package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/protocol"
)

// captureLogger records every Printf call so a test can assert that a disagreement was logged
// rather than silently swallowed.
type captureLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *captureLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, fmt.Sprintf(format, args...))
}

func (l *captureLogger) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func memPayload(s *memStore, seq uint64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.entries[seq]; m != nil {
		return m.entry.Payload
	}
	return nil
}

// issueDPoP writes a 200 /v1/enrol response minting the given identity.
func issueDPoP(t *testing.T, tenantID, deviceID string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		resp := protocol.EnrolmentResponse{
			SchemaVersion: protocol.EnrolmentSchemaVersion,
			DeviceID:      deviceID,
			TenantID:      tenantID,
			Region:        "eu",
			Credential:    protocol.IssuedCredential{Mode: protocol.AuthModeDPoP, JWK: &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}},
			ServerTime:    time.Now().UTC(),
		}
		raw, _ := json.Marshal(resp)
		writeJSON(t, w, http.StatusOK, raw)
	}
}

// TestEnrolAdoptsIssuedIdentity proves the fix: an enrol response that mints a device_id/tenant_id
// different from the flags makes subsequent envelopes carry the issued values, not the flags.
func TestEnrolAdoptsIssuedIdentity(t *testing.T) {
	sink := newMemStore()
	pipe, err := core.NewPipeline(sink, time.Now, func() string { return "evt-issued" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	pipe.Normalizer = dedup.IdentityNFC{}

	d, _ := newRawDrainer(t, Config{
		AuthMode:       protocol.AuthModeDPoP,
		EnrolmentToken: "tok-123",
		TenantID:       "flag-tenant",
		DeviceID:       "flag-device",
		AgentVersion:   "test",
		MDMID:          "mdm-9",
	}, issueDPoP(t, "issued-tenant", "issued-device"))
	// The service wires OnEnrolled to core.Pipeline.SetIdentity; here it is the same seam.
	d.cfg.OnEnrolled = func(c *credential.Credential) {
		pipe.SetIdentity(core.Identity{TenantID: c.TenantID, DeviceID: c.DeviceID, UserRef: "flag-user"})
	}

	c, err := d.enrol(context.Background(), HardwareIdentityHash("flag-tenant", "flag-device", "mdm-9"))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if err := d.setCredential(c); err != nil {
		t.Fatalf("setCredential: %v", err)
	}

	// The next envelope minted after enrolment must carry the issued identity.
	size := int64(10)
	out, err := pipe.Process(context.Background(), core.Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       size,
		Decision:        &protocol.Decision{RuleID: "r", Action: protocol.ActionLogged},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !out.Emitted {
		t.Fatalf("observation not emitted: %+v", out)
	}
	var env struct {
		TenantID string `json:"tenant_id"`
		DeviceID string `json:"device_id"`
		UserRef  string `json:"user_ref"`
	}
	if err := json.Unmarshal(memPayload(sink, out.Seq), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.TenantID != "issued-tenant" || env.DeviceID != "issued-device" {
		t.Fatalf("envelope identity = (%q, %q), want issued (%q, %q)", env.TenantID, env.DeviceID, "issued-tenant", "issued-device")
	}
	if env.UserRef != "flag-user" {
		t.Fatalf("envelope user_ref = %q, want %q (the server does not issue a user_ref)", env.UserRef, "flag-user")
	}
}

// TestEnrolDisagreementIsLogged proves a disagreement between the issued identity and the flags is
// surfaced, not silently swallowed.
func TestEnrolDisagreementIsLogged(t *testing.T) {
	log := &captureLogger{}
	d, _ := newRawDrainer(t, Config{
		AuthMode:       protocol.AuthModeDPoP,
		EnrolmentToken: "tok-123",
		TenantID:       "flag-tenant",
		DeviceID:       "flag-device",
		AgentVersion:   "test",
		MDMID:          "mdm-9",
	}, issueDPoP(t, "issued-tenant", "issued-device"))
	d.log = log

	c, err := d.enrol(context.Background(), HardwareIdentityHash("flag-tenant", "flag-device", "mdm-9"))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if err := d.setCredential(c); err != nil {
		t.Fatalf("setCredential: %v", err)
	}

	if !log.contains("issued tenant_id") || !log.contains("issued device_id") {
		t.Fatalf("the tenant_id/device_id disagreement was not logged; got %v", log.msgs)
	}
	if !log.contains("flag-tenant") || !log.contains("flag-device") {
		t.Fatalf("the disagreement log did not name the flag values; got %v", log.msgs)
	}
}

// TestAdoptIssuedIdentityAgreementIsSilent guards the opposite case: when the issued identity
// matches the flags, adoption still happens but nothing is logged about a disagreement (there is
// none).
func TestAdoptIssuedIdentityAgreementIsSilent(t *testing.T) {
	log := &captureLogger{}
	d, _ := newRawDrainer(t, Config{
		AuthMode:       protocol.AuthModeDPoP,
		EnrolmentToken: "tok-123",
		TenantID:       "tenant-1",
		DeviceID:       "device-1",
		AgentVersion:   "test",
	}, issueDPoP(t, "tenant-1", "device-1"))
	d.log = log
	var adoptedTenant, adoptedDevice string
	d.cfg.OnEnrolled = func(c *credential.Credential) {
		adoptedTenant, adoptedDevice = c.TenantID, c.DeviceID
	}

	c, err := d.enrol(context.Background(), HardwareIdentityHash("tenant-1", "device-1", ""))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if err := d.setCredential(c); err != nil {
		t.Fatalf("setCredential: %v", err)
	}
	if adoptedTenant != "tenant-1" || adoptedDevice != "device-1" {
		t.Fatalf("adopted identity = (%q, %q), want the issued (%q, %q)", adoptedTenant, adoptedDevice, "tenant-1", "device-1")
	}
	for _, m := range log.msgs {
		if strings.Contains(m, "differs from") {
			t.Fatalf("an agreement was logged as a disagreement: %q", m)
		}
	}
}

// TestStaleIdentityQuarantined proves a record minted under a different identity than the current
// credential is quarantined with a named reason rather than delivered (and rejected) or silently
// dropped.
func TestStaleIdentityQuarantined(t *testing.T) {
	store := newMemStore()
	e := testEnvelope(t, "evt-stale")
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	m["tenant_id"] = "other-tenant"
	m["device_id"] = "other-device"
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	e.Payload = raw
	e.SizeBytes = int64(len(raw))
	if _, err := store.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	d, _ := newTestDrainer(t, func() (protocol.Store, error) { return store, nil }, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a stale-identity entry must not be sent to the peer")
	})
	res, err := d.Drain(context.Background(), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Delivered != 0 {
		t.Fatalf("Delivered = %d, want 0 (a stale-identity record must not be delivered)", res.Delivered)
	}
	if res.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1 (the stale record is quarantined as rejected)", res.Rejected)
	}
	if store.state(1) != protocol.SpoolRejected {
		t.Fatalf("state = %s, want rejected", store.state(1))
	}
	if store.reason(1) != string(reasonStaleIdentity) {
		t.Fatalf("reason = %q, want %q", store.reason(1), reasonStaleIdentity)
	}
}
