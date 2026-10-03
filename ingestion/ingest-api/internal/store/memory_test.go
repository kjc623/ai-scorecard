package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/ladder"
)

// The in-memory store is the test double the service's tests run against, so its own semantics are
// tested here against the behaviour ingest.record_event() is specified to have. The ladder suite is
// additionally run against the live stored procedure in db_integration_test.go; these cases cover the
// parts of the double a conformance run cannot reach (transaction membership, quarantine rows).

func routes(t *testing.T) RouteTable {
	t.Helper()
	r, err := LoadRouteTable(filepath.Join("..", "..", "testdata", "route-fidelity.seed.json"))
	if err != nil {
		t.Fatalf("load route table: %v", err)
	}
	return r
}

func activeMemory(t *testing.T) *Memory {
	t.Helper()
	m := NewMemory(routes(t))
	m.SetPrincipal(ladder.TenantID, ladder.DeviceID, "cred-1", PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: true,
		CredentialExpiry: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	return m
}

func accepted(t *testing.T, raw json.RawMessage) AcceptedEvent {
	t.Helper()
	var probe struct {
		EventID string `json:"event_id"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return AcceptedEvent{Index: 0, EventID: probe.EventID, Route: probe.Source, Envelope: raw}
}

func write(t *testing.T, m *Memory, raw json.RawMessage, at string) EventOutcome {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("parse %s: %v", at, err)
	}
	res, err := m.WriteBatch(context.Background(), BatchWrite{
		TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
		ReceivedAt: ts, Accepted: []AcceptedEvent{accepted(t, raw)},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	if len(res.Outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(res.Outcomes))
	}
	return res.Outcomes[0]
}

// TestAdoptionTakesTheKeyAndTheDigestTogether mirrors the defect that was fixed in
// db/schema.sql: an exact observation arriving at equal-or-worse fidelity must adopt the weak-only
// row, taking the exact key AND the content digest in the same breath, and promote merge_confidence
// off 'low'. Taking the key alone would violate submission_exact_key_implies_digest.
func TestAdoptionTakesTheKeyAndTheDigestTogether(t *testing.T) {
	m := activeMemory(t)

	// Tier S first: M0, no digest, so the submission is weak-only.
	weak := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(1), Tool: "adopt-tool", OccurredAt: "2026-10-02T16:00:00Z",
		Source: "ext.web_request", Mode: "m0", SizeBytes: 100, DedupKey: ladder.Hash('1'),
	})
	first := write(t, m, weak, "2026-10-02T16:00:01Z")
	if first.Outcome != OutcomeInserted {
		t.Fatalf("first outcome = %q, want inserted", first.Outcome)
	}

	// Tier T second, same device/tool/bucket/size and the SAME route rank: the equal-rank case that
	// used to raise instead of adopting.
	exact := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(2), Tool: "adopt-tool", OccurredAt: "2026-10-02T16:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 100,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	})
	second := write(t, m, exact, "2026-10-02T16:00:02Z")
	if second.Outcome != OutcomeMerged {
		t.Fatalf("second outcome = %q, want merged: the exact observation must adopt the weak row", second.Outcome)
	}
	if second.SubmissionID != first.SubmissionID {
		t.Errorf("adoption must keep the submission: %s vs %s", second.SubmissionID, first.SubmissionID)
	}
	subs := m.Submissions()
	if len(subs) != 1 {
		t.Fatalf("submissions = %d, want 1", len(subs))
	}
	got := subs[0]
	if got.DedupKey != ladder.Hash('b') {
		t.Errorf("dedup_key = %q, want the adopted exact key", got.DedupKey)
	}
	if got.MergeConfidence != "high" {
		t.Errorf("merge_confidence = %q, want high: the row now holds an exact key", got.MergeConfidence)
	}
	if got.ObservationCount != 2 {
		t.Errorf("observation_count = %d, want 2", got.ObservationCount)
	}
	// The adopting observation did not outrank the row, so no contested field changed. won_fields is
	// route-based (see EventOutcome.WonFields): the submission's fields come from ext.web_request,
	// which is this observation's route, so it reports true rather than claiming a change happened.
	if !second.WonFields {
		t.Error("won_fields = false, but the submission's winning route is this observation's route")
	}
}

// TestWriteBatchIsOneTransaction asserts §6's boundary: a failure anywhere in the batch leaves the
// store exactly as it was, so a device never observes a partial commit.
func TestWriteBatchIsOneTransaction(t *testing.T) {
	m := activeMemory(t)
	good := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(3), Tool: "tx-tool", OccurredAt: "2026-10-02T16:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 10,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	})
	// A second event whose route is absent from ref.route_fidelity: the write fails after the first
	// event has already been recorded, which is precisely the partial-commit case.
	unknownRoute := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(4), Tool: "tx-tool", OccurredAt: "2026-10-02T16:00:00Z",
		Source: "ext.telepathy", Mode: "m1", SizeBytes: 10,
		ContentDigest: ladder.Hash('c'), DedupKey: ladder.Hash('d'),
	})

	_, err := m.WriteBatch(context.Background(), BatchWrite{
		TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
		ReceivedAt: time.Date(2026, 10, 2, 16, 0, 1, 0, time.UTC),
		Accepted: []AcceptedEvent{
			{Index: 0, EventID: ladder.DeterministicUUID(3), Route: "ext.web_request", Envelope: good},
			{Index: 1, EventID: ladder.DeterministicUUID(4), Route: "ext.telepathy", Envelope: unknownRoute},
		},
	})
	if err == nil {
		t.Fatal("expected the unknown route to fail the write")
	}
	if m.ObservationCount() != 0 {
		t.Errorf("observations = %d, want 0: the whole batch must roll back", m.ObservationCount())
	}
	if len(m.Submissions()) != 0 {
		t.Errorf("submissions = %d, want 0", len(m.Submissions()))
	}
}

// TestQuarantineReasonsUseTheAlignedVocabulary pins the codes written into the quarantine, so the
// spelling cannot drift back to the pre-alignment vocabulary dbuilder removed from the CHECK.
func TestQuarantineReasonsUseTheAlignedVocabulary(t *testing.T) {
	m := activeMemory(t)
	detail := &protocol.BatchRejectionDetail{Pointer: "/events/0/kind", Expected: "one of prompt | usage_rollup | model_detection"}

	_, err := m.WriteBatch(context.Background(), BatchWrite{
		TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
		ReceivedAt: time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC),
		Rejected: []Rejection{
			{Index: 0, EventID: ladder.DeterministicUUID(5), Reason: protocol.ReasonUnknownKind,
				Detail: detail, Quarantine: true, Redacted: map[string]json.RawMessage{}},
			{Index: 1, EventID: ladder.DeterministicUUID(6), Reason: protocol.ReasonRevokedDevice,
				Detail: detail, Quarantine: true, Redacted: map[string]json.RawMessage{}},
			{Index: 2, EventID: ladder.DeterministicUUID(7), Reason: protocol.ReasonOversize,
				Detail: detail, Quarantine: true, Redacted: map[string]json.RawMessage{}},
			{Index: 3, EventID: ladder.DeterministicUUID(8), Reason: protocol.ReasonUnsupportedSchemaVersion,
				Detail: detail, Quarantine: true, Redacted: map[string]json.RawMessage{}},
			// tenant_mismatch has no honest quarantine row; asking for one must produce nothing.
			{Index: 4, EventID: ladder.DeterministicUUID(9), Reason: protocol.ReasonTenantMismatch,
				Detail: detail, Quarantine: true, Redacted: map[string]json.RawMessage{}},
		},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	got := map[string]bool{}
	for _, q := range m.Quarantined() {
		got[q.ReasonCode] = true
		if q.ExpiresAt.IsZero() {
			t.Errorf("quarantine row %q has no expiry", q.ReasonCode)
		}
	}
	for _, want := range []string{"unknown_kind", "revoked_device", "oversize", "unsupported_schema_version"} {
		if !got[want] {
			t.Errorf("quarantine is missing %q; it holds %v", want, got)
		}
	}
	for _, banned := range []string{"device_revoked", "batch_oversize", "schema_version_unsupported", "tenant_mismatch"} {
		if got[banned] {
			t.Errorf("quarantine carries %q, which is either the pre-alignment spelling or a code with no honest row", banned)
		}
	}
}

// TestRetentionIsMaterialised asserts expires_at is set at write time (§6: retention is resolved
// once, at write, so a later policy change does not retro-apply).
func TestRetentionIsMaterialised(t *testing.T) {
	m := activeMemory(t)
	raw := ladder.Prompt(ladder.PromptSpec{
		EventID: ladder.DeterministicUUID(10), Tool: "ttl-tool", OccurredAt: "2026-10-02T16:00:00Z",
		Source: "ext.web_request", Mode: "m1", SizeBytes: 10,
		ContentDigest: ladder.Hash('a'), DedupKey: ladder.Hash('b'),
	})
	at := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	write(t, m, raw, at.Format(time.RFC3339))
	subs := m.Submissions()
	if len(subs) != 1 {
		t.Fatalf("submissions = %d, want 1", len(subs))
	}
	// The double uses the 'standard' retention class default; the real resolution is the database's
	// ops.event_ttl_days(), which the SQL path calls and this double deliberately does not mirror.
	if want := at.AddDate(0, 0, 90); !subs[0].ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %s, want %s", subs[0].ExpiresAt, want)
	}
}

// TestPrincipalStatusCheckWritable covers the §2.3 rules in one place.
func TestPrincipalStatusCheckWritable(t *testing.T) {
	now := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	base := PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true,
		DeviceKnown: true, CredentialKnown: true, CredentialExpiry: now.Add(time.Hour),
	}
	if err := base.CheckWritable(now); err != nil {
		t.Fatalf("an active principal was refused: %v", err)
	}

	revoked := base
	revoked.CredentialRevoked = &now
	if err := revoked.CheckWritable(now); err != ErrCredentialRevoked {
		t.Errorf("revoked credential: %v", err)
	}
	expired := base
	expired.CredentialExpiry = now.Add(-time.Second)
	if err := expired.CheckWritable(now); err != ErrCredentialExpired {
		t.Errorf("expired credential: %v", err)
	}
	unknownTenant := PrincipalStatus{}
	if err := unknownTenant.CheckWritable(now); err != ErrUnknownTenant {
		t.Errorf("unknown tenant: %v", err)
	}
	suspended := base
	suspended.IngestEnabled = false
	if err := suspended.CheckWritable(now); err != ErrTenantSuspended {
		t.Errorf("suspended tenant: %v", err)
	}
	closed := base
	closed.TenantStatus = "closed"
	if err := closed.CheckWritable(now); err != ErrTenantSuspended {
		t.Errorf("closed tenant: %v", err)
	}
	revokedDevice := base
	revokedDevice.DeviceRevokedAt = &now
	if err := revokedDevice.CheckWritable(now); err != ErrDeviceRevoked {
		t.Errorf("revoked device: %v", err)
	}
}
