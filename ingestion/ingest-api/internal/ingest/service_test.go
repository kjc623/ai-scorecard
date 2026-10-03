package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/ladder"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

func testService(t *testing.T) (*Service, *store.Memory) {
	t.Helper()
	mem := store.NewMemory(loadRoutes(t))
	activePrincipal(mem)
	svc, err := New(loadSchema(t), mem, DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, mem
}

func principal() auth.Principal {
	return auth.Principal{TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1"}
}

func batchOf(events ...json.RawMessage) protocol.EventBatch {
	return protocol.EventBatch{
		SchemaVersion: "1.0",
		BatchID:       "batch-1",
		DeviceSentAt:  time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC),
		EventCount:    len(events),
		Events:        events,
	}
}

// validM1 returns a prompt envelope that satisfies the contract at m1.
func validM1() map[string]any {
	return map[string]any{
		"schema_version":      "1.0",
		"event_id":            ladder.DeterministicUUID(100),
		"tenant_id":           ladder.TenantID,
		"device_id":           ladder.DeviceID,
		"user_ref":            "user-1",
		"tool_fingerprint":    "tool-1",
		"direction":           "egress",
		"kind":                "prompt",
		"occurred_at":         "2026-10-02T14:00:00Z",
		"monotonic_offset_ms": 1,
		"source":              "ext.web_request",
		"collection_mode":     "m1",
		"confidence":          "high",
		"size_bytes":          10,
		"content_digest":      ladder.Hash('a'),
		"labels":              []any{},
		"classifier_version":  "2026.01.0-shadow",
		"policy_decision":     map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":           ladder.Hash('b'),
	}
}

func mustRaw(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestM0PromptCarryingContentIsRejected is the case docs/01-collectors.md §7.1 requires: at M0 the
// collector must not have read content, so a content-derived field is evidence of a defect and is
// rejected at ingest rather than ignored.
func TestM0PromptCarryingContentIsRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string // the pointer the rejection must name
	}{
		{"content_digest", func(m map[string]any) { m["content_digest"] = ladder.Hash('c') }, "/events/0/content_digest"},
		{"labels", func(m map[string]any) { m["labels"] = []any{} }, "/events/0/labels"},
		{"classifier_version", func(m map[string]any) { m["classifier_version"] = "2026.01.0-shadow" }, "/events/0/classifier_version"},
		{"confidence", func(m map[string]any) { m["confidence"] = "high" }, "/events/0/confidence"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, mem := testService(t)
			m := validM1()
			m["collection_mode"] = "m0"
			for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence"} {
				delete(m, f)
			}
			// The defect: an M0 record that carries a content-derived field.
			c.mutate(m)

			resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)),
				time.Date(2026, 10, 2, 14, 0, 1, 0, time.UTC))
			if err != nil {
				t.Fatalf("a per-event rejection must not fail the batch: %v", err)
			}
			if resp.Counts.Rejected != 1 {
				t.Fatalf("counts = %+v, want 1 rejected", resp.Counts)
			}
			got := resp.Results[0]
			if got.Outcome != protocol.OutcomeRejected {
				t.Fatalf("outcome = %q, want rejected", got.Outcome)
			}
			if got.Reason != protocol.ReasonModeViolation {
				t.Fatalf("reason = %q, want mode_violation", got.Reason)
			}
			if got.Detail == nil || got.Detail.Pointer != c.want {
				t.Errorf("detail.pointer = %v, want %s", got.Detail, c.want)
			}
			if got.Detail != nil && !strings.Contains(got.Detail.Expected, "m0") {
				t.Errorf("detail.expected = %q, want it to name the mode boundary", got.Detail.Expected)
			}
			if len(got.Detail.PresenceMap) == 0 {
				t.Error("§7 requires a field-presence map on a rejection")
			}
			if n := mem.ObservationCount(); n != 0 {
				t.Errorf("observations = %d, want 0: nothing was read and nothing may be stored", n)
			}
			// §7: the quarantine holds the diagnosis and none of the content.
			q := mem.Quarantined()
			if len(q) != 1 {
				t.Fatalf("quarantined = %d, want 1 (nothing is silently dropped)", len(q))
			}
			if q[0].ReasonCode != "mode_violation" {
				t.Errorf("quarantine reason_code = %q, want mode_violation", q[0].ReasonCode)
			}
			for _, forbidden := range []string{"content_digest", "content_excerpt", "attachments"} {
				if _, present := q[0].Redacted[forbidden]; present {
					t.Errorf("quarantined envelope still carries %s; ingest.rejected must hold no content", forbidden)
				}
			}
		})
	}
}

// TestM1AndAboveRequireClassifierOutput covers the other half of the mode boundary: §7's
// "labels missing at M1+".
func TestM1AndAboveRequireClassifierOutput(t *testing.T) {
	for _, missing := range []string{"content_digest", "labels", "classifier_version", "confidence"} {
		t.Run("missing_"+missing, func(t *testing.T) {
			svc, _ := testService(t)
			m := validM1()
			delete(m, missing)
			resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), time.Now())
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if resp.Results[0].Reason != protocol.ReasonModeViolation {
				t.Fatalf("reason = %q, want mode_violation", resp.Results[0].Reason)
			}
		})
	}
}

func TestExcerptBoundaryBetweenM2AndM3(t *testing.T) {
	t.Run("m2 requires an excerpt", func(t *testing.T) {
		svc, _ := testService(t)
		m := validM1()
		m["collection_mode"] = "m2"
		resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), time.Now())
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if resp.Results[0].Reason != protocol.ReasonModeViolation {
			t.Fatalf("reason = %q, want mode_violation", resp.Results[0].Reason)
		}
	})
	t.Run("m3 forbids an excerpt, because content moves only on a grant", func(t *testing.T) {
		svc, _ := testService(t)
		m := validM1()
		m["collection_mode"] = "m3"
		m["content_excerpt"] = map[string]any{"kind": "match_span", "text": "x"}
		resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), time.Now())
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if resp.Results[0].Reason != protocol.ReasonModeViolation {
			t.Fatalf("reason = %q, want mode_violation", resp.Results[0].Reason)
		}
		if p := resp.Results[0].Detail.Pointer; p != "/events/0/content_excerpt" {
			t.Errorf("pointer = %q, want /events/0/content_excerpt", p)
		}
	})
}

// TestReasonCodeVocabulary walks §7's closed set through the checks that produce each code, so a
// code cannot be removed from the closed set without a test failing.
func TestReasonCodeVocabulary(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 5, 0, time.UTC)

	t.Run("unknown_kind", func(t *testing.T) {
		svc, _ := testService(t)
		m := validM1()
		m["kind"] = "process_telemetry" // the R7 case: no such kind exists
		resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), now)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if resp.Results[0].Reason != protocol.ReasonUnknownKind {
			t.Fatalf("reason = %q, want unknown_kind", resp.Results[0].Reason)
		}
	})

	t.Run("unsupported_schema_version", func(t *testing.T) {
		svc, _ := testService(t)
		m := validM1()
		m["schema_version"] = "2.0"
		resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), now)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		got := resp.Results[0]
		if got.Reason != protocol.ReasonUnsupportedSchemaVersion {
			t.Fatalf("reason = %q, want unsupported_schema_version", got.Reason)
		}
		if got.Detail == nil || len(got.Detail.Supported) != 1 || got.Detail.Supported[0] != "1.0" {
			t.Errorf("detail.supported = %v, want the advertised set [1.0]", got.Detail)
		}
	})

	t.Run("tenant_mismatch", func(t *testing.T) {
		svc, mem := testService(t)
		m := validM1()
		m["tenant_id"] = "99999999-9999-4999-8999-999999999999"
		resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), now)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if resp.Results[0].Reason != protocol.ReasonTenantMismatch {
			t.Fatalf("reason = %q, want tenant_mismatch", resp.Results[0].Reason)
		}
		// A body claiming another tenant must never reach ingest.rejected under this tenant's row.
		if q := mem.Quarantined(); len(q) != 0 {
			t.Errorf("quarantined = %d, want 0: there is no honest quarantine code for a foreign tenant claim", len(q))
		}
	})

	t.Run("schema_violation with a pointer", func(t *testing.T) {
		svc, _ := testService(t)
		m := validM1()
		m["dedup_key"] = "not-a-sha256"
		resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), now)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		got := resp.Results[0]
		if got.Reason != protocol.ReasonSchemaViolation {
			t.Fatalf("reason = %q, want schema_violation", got.Reason)
		}
		if got.Detail == nil || got.Detail.Pointer != "/events/0/dedup_key" {
			t.Errorf("pointer = %v, want /events/0/dedup_key", got.Detail)
		}
	})

	t.Run("schema_violation for a field the contract does not declare", func(t *testing.T) {
		svc, _ := testService(t)
		m := validM1()
		m["prompt_text"] = "the contract is additionalProperties:false"
		resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), now)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		got := resp.Results[0]
		if got.Reason != protocol.ReasonSchemaViolation {
			t.Fatalf("reason = %q, want schema_violation", got.Reason)
		}
		if got.Detail == nil || got.Detail.Pointer != "/events/0/prompt_text" {
			t.Errorf("pointer = %v, want /events/0/prompt_text", got.Detail)
		}
	})

	t.Run("oversize for a batch outside 1-500", func(t *testing.T) {
		svc, _ := testService(t)
		b := batchOf()
		b.EventCount = 0
		_, err := svc.Submit(context.Background(), principal(), b, now)
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("err = %v, want a batch-level *Error", err)
		}
		if apiErr.Code != protocol.ReasonOversize || apiErr.Status != 400 {
			t.Fatalf("code/status = %s/%d, want oversize/400", apiErr.Code, apiErr.Status)
		}
	})

	t.Run("oversize for a single envelope over the cap", func(t *testing.T) {
		svc, _ := testService(t)
		big := make([]byte, protocol.MaxEnvelopeBytes+1)
		for i := range big {
			big[i] = ' '
		}
		big[0], big[len(big)-1] = '{', '}'
		_, err := svc.Submit(context.Background(), principal(), batchOf(big), now)
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("err = %v, want a batch-level *Error", err)
		}
		if apiErr.Code != protocol.ReasonOversize || apiErr.Status != 413 {
			t.Fatalf("code/status = %s/%d, want oversize/413 (§5.3 lists the per-event cap under 413)", apiErr.Code, apiErr.Status)
		}
	})

	t.Run("schema_violation when event_count disagrees with events", func(t *testing.T) {
		svc, _ := testService(t)
		b := batchOf(mustRaw(t, validM1()))
		b.EventCount = 3
		_, err := svc.Submit(context.Background(), principal(), b, now)
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != protocol.ReasonSchemaViolation || apiErr.Status != 400 {
			t.Fatalf("err = %v, want schema_violation/400", err)
		}
	})
}

// TestQuarantineMappingIsTotal is the invariant the Lead asked to be asserted: every code in §7's
// closed set either maps to a quarantine code, or is one of the two documented cases where no
// honest counterpart exists in the live ingest.rejected CHECK.
func TestQuarantineMappingIsTotal(t *testing.T) {
	documentedExceptions := map[protocol.ReasonCode]string{
		protocol.ReasonTenantMismatch: "the live CHECK has no code for a body disagreeing with the authenticated principal",
		protocol.ReasonDuplicateBatch: "batch-level by construction: there is no per-event envelope to quarantine",
	}
	for _, code := range protocol.AllReasonCodes {
		mapped, ok := store.QuarantineReason(code)
		if !ok {
			if _, expected := documentedExceptions[code]; !expected {
				t.Errorf("wire code %q has no quarantine mapping and is not a documented exception", code)
			}
			continue
		}
		if mapped == "" {
			t.Errorf("wire code %q mapped to an empty quarantine code", code)
		}
		if _, expected := documentedExceptions[code]; expected {
			t.Errorf("wire code %q is listed as a documented exception but now maps to %q; update the report", code, mapped)
		}
	}
	if len(protocol.AllReasonCodes) != 10 {
		t.Errorf("§7's closed set has %d codes; the closed-set test must be revisited when it changes", len(protocol.AllReasonCodes))
	}
}

// TestBatchWithRejectionsStillWritesTheAcceptedEvents is I2/I3 together: a validation failure is a
// per-event outcome, it never aborts the batch, and it is never silently dropped.
func TestBatchWithRejectionsStillWritesTheAcceptedEvents(t *testing.T) {
	svc, mem := testService(t)
	good := mustRaw(t, validM1())
	bad := validM1()
	bad["kind"] = "process_telemetry"
	bad["event_id"] = ladder.DeterministicUUID(101)

	b := batchOf(good, mustRaw(t, bad))
	resp, err := svc.Submit(context.Background(), principal(), b, time.Date(2026, 10, 2, 14, 0, 9, 0, time.UTC))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.Counts.Accepted != 1 || resp.Counts.Rejected != 1 {
		t.Fatalf("counts = %+v, want 1 accepted and 1 rejected", resp.Counts)
	}
	if resp.Results[0].Outcome != protocol.OutcomeAccepted || resp.Results[1].Outcome != protocol.OutcomeRejected {
		t.Fatalf("results must be in request order, got %q then %q", resp.Results[0].Outcome, resp.Results[1].Outcome)
	}
	if len(mem.Quarantined()) != 1 {
		t.Errorf("the rejected event must have a quarantine row")
	}
	if mem.ObservationCount() != 1 {
		t.Errorf("observations = %d, want 1", mem.ObservationCount())
	}
}

// TestRouteMustExistInReferenceTable covers the pre-transaction check that keeps an unknown route
// from reaching ingest.record_event(), which raises rather than guessing a rank.
func TestRouteMustExistInReferenceTable(t *testing.T) {
	routes := loadRoutes(t)
	delete(routes, "ext.dom") // as if ref.route_fidelity had lost the row
	mem := store.NewMemory(routes)
	activePrincipal(mem)
	svc, err := New(loadSchema(t), mem, DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := validM1()
	m["source"] = "ext.dom"
	resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m)), time.Now())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.Results[0].Reason != protocol.ReasonSchemaViolation {
		t.Fatalf("reason = %q, want schema_violation", resp.Results[0].Reason)
	}
	if !strings.Contains(resp.Results[0].Detail.Expected, "ref.route_fidelity") {
		t.Errorf("expected = %q, want it to name the reference table", resp.Results[0].Detail.Expected)
	}
}

// TestRevokedCredentialInsideTheTransaction covers §2.3: the credential is re-checked before
// commit, and a revocation between admission and commit writes nothing at all.
func TestRevokedCredentialInsideTheTransaction(t *testing.T) {
	svc, mem := testService(t)
	revokedAt := time.Date(2026, 10, 2, 14, 0, 4, 0, time.UTC)
	mem.Revoke(ladder.TenantID, ladder.DeviceID, "cred-1", revokedAt)

	_, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, validM1())),
		time.Date(2026, 10, 2, 14, 0, 5, 0, time.UTC))
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a batch-level *Error", err)
	}
	if apiErr.Code != protocol.ReasonRevokedDevice || apiErr.Status != 401 {
		t.Fatalf("code/status = %s/%d, want revoked_device/401", apiErr.Code, apiErr.Status)
	}
	if mem.ObservationCount() != 0 {
		t.Errorf("observations = %d, want 0: a revoked batch writes nothing", mem.ObservationCount())
	}
	if n := len(mem.Submissions()); n != 0 {
		t.Errorf("submissions = %d, want 0", n)
	}
}

// TestDedupTierIsRecomputedNotSent covers §4.5's "the tier is not a wire field": the server
// recomputes it and reports it, and the report differs by kind and by attachment readability.
func TestDedupTierIsRecomputedNotSent(t *testing.T) {
	svc, _ := testService(t)
	now := time.Now()

	m1 := validM1()
	m1["event_id"] = ladder.DeterministicUUID(200)
	resp, err := svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m1)), now)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := resp.Results[0].DedupTier; got != "T" {
		t.Errorf("dedup_tier = %q, want T", got)
	}

	// An attachment whose bytes the route could not read makes it Tier T-B, which the wire field
	// reports as T (the distinction is internal: §5.3 says "T (tier T) or S").
	tb := validM1()
	tb["event_id"] = ladder.DeterministicUUID(201)
	tb["attachments"] = []any{map[string]any{"name": "contract.pdf", "size_bytes": 10}}
	resp, err = svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, tb)), now)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := resp.Results[0].DedupTier; got != "T" {
		t.Errorf("dedup_tier = %q, want T for a Tier T-B observation", got)
	}

	m0 := validM1()
	m0["event_id"] = ladder.DeterministicUUID(202)
	m0["collection_mode"] = "m0"
	for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence"} {
		delete(m0, f)
	}
	resp, err = svc.Submit(context.Background(), principal(), batchOf(mustRaw(t, m0)), now)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := resp.Results[0].DedupTier; got != "S" {
		t.Errorf("dedup_tier = %q, want S for an M0 observation", got)
	}
}

// TestBatchLevelIdentityFailureIsNotAPartialWrite asserts the §6 transaction boundary: a batch
// whose credential is refused leaves the store exactly as it was.
func TestBatchLevelIdentityFailureIsNotAPartialWrite(t *testing.T) {
	svc, mem := testService(t)
	// The tenant and device are known but the presented credential is not: 401, not 403.
	mem.SetPrincipal(ladder.TenantID, ladder.DeviceID, "", store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: false,
	})
	p := auth.Principal{TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "unknown-cred"}
	_, err := svc.Submit(context.Background(), p, batchOf(mustRaw(t, validM1())), time.Now())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Fatalf("err = %v, want 401 for an unknown credential on a known device", err)
	}
	if mem.ObservationCount() != 0 || len(mem.Submissions()) != 0 || len(mem.Quarantined()) != 0 {
		t.Error("nothing may be written when the principal is not authenticated")
	}
}

// TestUnknownTenantIsRefusedWith403 covers the other half: a principal whose tenant this deployment
// does not know is a 403, and §7's code for "unknown or inactive" is what the device sees.
func TestUnknownTenantIsRefusedWith403(t *testing.T) {
	svc, mem := testService(t)
	p := auth.Principal{
		TenantID: "88888888-8888-4888-8888-888888888888", DeviceID: ladder.DeviceID, CredentialID: "cred-1",
	}
	_, err := svc.Submit(context.Background(), p, batchOf(mustRaw(t, validM1())), time.Now())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 403 || apiErr.Code != protocol.ReasonUnknownTenant {
		t.Fatalf("err = %v, want 403 unknown_tenant", err)
	}
	if mem.ObservationCount() != 0 {
		t.Error("nothing may be written for an unknown tenant")
	}
}
