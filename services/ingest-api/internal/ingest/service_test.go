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
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

const (
	tenant = "22222222-2222-4222-8222-222222222222"
	device = "33333333-3333-4333-8333-333333333333"
)

var receivedAt = time.Date(2026, 10, 5, 12, 0, 1, 0, time.UTC)

// fakeStore records the write and answers with the configured outcomes.
type fakeStore struct {
	routes   []string
	outcome  store.Outcome
	err      error
	writes   []store.BatchWrite
	routeErr error
}

func (f *fakeStore) Routes(context.Context) ([]string, error) { return f.routes, f.routeErr }

func (f *fakeStore) WriteBatch(_ context.Context, w store.BatchWrite) ([]store.EventOutcome, error) {
	f.writes = append(f.writes, w)
	if f.err != nil {
		return nil, f.err
	}
	var out []store.EventOutcome
	for _, ev := range w.Accepted {
		o := store.EventOutcome{Index: ev.Index, EventID: ev.EventID, Outcome: f.outcome, SubmissionID: "44444444-4444-4444-8444-444444444444"}
		switch f.outcome {
		case store.OutcomeDuplicate:
			first := receivedAt.Add(-time.Hour)
			o.FirstReceivedAt = &first
		case store.OutcomeInserted:
			o.WonFields = true
		}
		out = append(out, o)
	}
	return out, nil
}

func newService(t *testing.T) (*Service, *fakeStore) {
	t.Helper()
	v, err := contract.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{routes: []string{"ext.web_request", "ext.dom", "proc.detect"}, outcome: store.OutcomeInserted}
	return New(v, st), st
}

func principal() auth.Principal {
	return auth.Principal{TenantID: tenant, DeviceID: device, CredentialID: "55555555-5555-4555-8555-555555555555"}
}

func sha(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func promptM1(eventID string) map[string]any {
	return map[string]any{
		"schema_version":      "1.0",
		"event_id":            eventID,
		"tenant_id":           tenant,
		"device_id":           device,
		"user_ref":            "user-1",
		"tool_fingerprint":    "tool-1",
		"direction":           "egress",
		"kind":                "prompt",
		"occurred_at":         "2026-10-05T12:00:00Z",
		"monotonic_offset_ms": 1,
		"source":              "ext.web_request",
		"collection_mode":     "m1",
		"confidence":          "high",
		"size_bytes":          10,
		"content_digest":      sha("a"),
		"labels":              []any{},
		"classifier_version":  "2026.01.0",
		"policy_decision":     map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":           sha("b"),
	}
}

func eventID(n int) string {
	return "11111111-1111-4111-8111-" + strings.Repeat("0", 11) + string(rune('0'+n))
}

func batchOf(t *testing.T, events ...map[string]any) protocol.EventBatch {
	t.Helper()
	b := protocol.EventBatch{SchemaVersion: "1.0", BatchID: "batch-1", DeviceSentAt: receivedAt, EventCount: len(events)}
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Events = append(b.Events, raw)
	}
	return b
}

func submit(t *testing.T, svc *Service, b protocol.EventBatch) *protocol.EventBatchResponse {
	t.Helper()
	resp, err := svc.Submit(context.Background(), principal(), b, receivedAt)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ids := make([]string, len(resp.Results))
	for i, r := range resp.Results {
		ids[i] = r.EventID
	}
	// The response must be one the device's own validator accepts.
	if err := resp.Validate(ids); err != nil {
		t.Fatalf("the device would refuse this response: %v", err)
	}
	return resp
}

func TestAcceptedEventIsWrittenAsSent(t *testing.T) {
	svc, st := newService(t)
	b := batchOf(t, promptM1(eventID(1)))
	resp := submit(t, svc, b)

	r := resp.Results[0]
	if r.Outcome != protocol.OutcomeAccepted || r.DedupTier != "T" || r.WonFields == nil || !*r.WonFields {
		t.Errorf("result = %+v", r)
	}
	if len(st.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(st.writes))
	}
	w := st.writes[0]
	if w.TenantID != tenant || w.DeviceID != device || !w.ReceivedAt.Equal(receivedAt) {
		t.Errorf("write principal = %+v", w)
	}
	if len(w.Accepted) != 1 || string(w.Accepted[0].Envelope) != string(b.Events[0]) || w.Accepted[0].Route != "ext.web_request" {
		t.Errorf("accepted = %+v; the envelope must be the bytes the device sent", w.Accepted)
	}
}

func TestDuplicateReportsTheFirstReceipt(t *testing.T) {
	svc, st := newService(t)
	st.outcome = store.OutcomeDuplicate
	r := submit(t, svc, batchOf(t, promptM1(eventID(1)))).Results[0]
	if r.Outcome != protocol.OutcomeDuplicate || r.FirstReceivedAt == nil || r.DedupTier != "" || r.WonFields != nil {
		t.Errorf("result = %+v", r)
	}
}

func TestPerEventRejections(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(m map[string]any)
		reason     protocol.ReasonCode
		pointer    string
		quarantine bool
	}{
		{"content at m0", func(m map[string]any) {
			m["collection_mode"] = "m0"
			delete(m, "labels")
			delete(m, "classifier_version")
			delete(m, "confidence")
		}, protocol.ReasonModeViolation, "/events/1/content_digest", true},
		{"unknown kind", func(m map[string]any) { m["kind"] = "process_list" }, protocol.ReasonUnknownKind, "/events/1/kind", true},
		{"old schema version", func(m map[string]any) { m["schema_version"] = "0.9" }, protocol.ReasonUnsupportedSchemaVersion, "/events/1/schema_version", true},
		{"undeclared field", func(m map[string]any) { m["prompt_text"] = "secret" }, protocol.ReasonSchemaViolation, "/events/1/prompt_text", true},
		{"another tenant", func(m map[string]any) { m["tenant_id"] = "99999999-9999-4999-8999-999999999999" }, protocol.ReasonTenantMismatch, "/events/1/tenant_id", false},
		{"another device", func(m map[string]any) { m["device_id"] = "99999999-9999-4999-8999-999999999999" }, protocol.ReasonSchemaViolation, "/events/1/device_id", true},
		{"route not ranked here", func(m map[string]any) { m["source"] = "cli.shim" }, protocol.ReasonSchemaViolation, "/events/1/source", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, st := newService(t)
			bad := promptM1(eventID(2))
			c.mutate(bad)
			resp := submit(t, svc, batchOf(t, promptM1(eventID(1)), bad))

			if resp.Counts.Accepted != 1 || resp.Counts.Rejected != 1 {
				t.Fatalf("counts = %+v; a rejection must not cost the rest of the batch", resp.Counts)
			}
			r := resp.Results[1]
			if r.Reason != c.reason || r.Detail == nil || r.Detail.Pointer != c.pointer {
				t.Fatalf("result = %+v, detail = %+v; want %s at %s", r, r.Detail, c.reason, c.pointer)
			}
			if len(r.Detail.PresenceMap) == 0 {
				t.Error("a rejection carries the presence map")
			}
			w := st.writes[0]
			if got := len(w.Rejected) == 1; got != c.quarantine {
				t.Fatalf("quarantined = %d, want %v", len(w.Rejected), c.quarantine)
			}
			if c.quarantine {
				for _, gone := range []string{"content_digest", "content_excerpt", "attachments", "prompt_text"} {
					if _, ok := w.Rejected[0].Redacted[gone]; ok {
						t.Errorf("the quarantined envelope carries %s", gone)
					}
				}
			}
		})
	}
}

func TestDedupTierFollowsTheVariant(t *testing.T) {
	m0 := promptM1(eventID(1))
	m0["collection_mode"] = "m0"
	for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence"} {
		delete(m0, f)
	}
	rollup := promptM1(eventID(2))
	for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence", "size_bytes", "policy_decision"} {
		delete(rollup, f)
	}
	rollup["kind"], rollup["direction"], rollup["source"] = "usage_rollup", "none", "proc.detect"
	rollup["window_start"], rollup["window_end"], rollup["submission_count"], rollup["bytes_total"] = "2026-10-05T11:55:00Z", "2026-10-05T12:00:00Z", 2, 20
	discovery := promptM1(eventID(4))
	for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence", "size_bytes", "policy_decision"} {
		delete(discovery, f)
	}
	discovery["kind"], discovery["direction"], discovery["source"] = "discovery", "none", "proc.detect"
	discovery["discovery_type"], discovery["detection_basis"] = "app_running", "process_event"
	activity := promptM1(eventID(5))
	for _, f := range []string{"content_digest", "labels", "classifier_version", "confidence", "policy_decision"} {
		delete(activity, f)
	}
	activity["kind"], activity["direction"], activity["source"] = "agent_activity", "none", "proc.detect"
	activity["activity_type"], activity["tool_name"], activity["outcome"] = "tool_call", "Bash", "success"

	svc, _ := newService(t)
	resp := submit(t, svc, batchOf(t, m0, rollup, promptM1(eventID(3)), discovery, activity))
	for i, want := range []string{"S", "R", "T", "V", "A"} {
		if got := resp.Results[i].DedupTier; got != want {
			t.Errorf("result %d dedup_tier = %q, want %q", i, got, want)
		}
	}
}

func TestBatchShapeErrors(t *testing.T) {
	svc, st := newService(t)
	good := batchOf(t, promptM1(eventID(1)))
	cases := map[string]struct {
		mutate func(b *protocol.EventBatch)
		status int
		code   protocol.ReasonCode
	}{
		"no batch_id":         {func(b *protocol.EventBatch) { b.BatchID = "" }, 400, protocol.ReasonSchemaViolation},
		"unsupported version": {func(b *protocol.EventBatch) { b.SchemaVersion = "2.0" }, 400, protocol.ReasonUnsupportedSchemaVersion},
		"count mismatch":      {func(b *protocol.EventBatch) { b.EventCount = 2 }, 400, protocol.ReasonSchemaViolation},
		"no events":           {func(b *protocol.EventBatch) { b.Events, b.EventCount = nil, 0 }, 400, protocol.ReasonOversize},
		"oversized envelope": {func(b *protocol.EventBatch) {
			b.Events = []json.RawMessage{json.RawMessage(`"` + strings.Repeat("x", protocol.MaxEnvelopeBytes) + `"`)}
		}, 413, protocol.ReasonOversize},
		"element not an object": {func(b *protocol.EventBatch) { b.Events = []json.RawMessage{json.RawMessage(`[1]`)} }, 400, protocol.ReasonSchemaViolation},
		"no event_id": {func(b *protocol.EventBatch) {
			m := promptM1("")
			delete(m, "event_id")
			raw, _ := json.Marshal(m)
			b.Events = []json.RawMessage{raw}
		}, 400, protocol.ReasonSchemaViolation},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b := good
			b.Events = append([]json.RawMessage(nil), good.Events...)
			c.mutate(&b)
			_, err := svc.Submit(context.Background(), principal(), b, receivedAt)
			var e *Error
			if !errors.As(err, &e) || e.Status != c.status || e.Code != c.code {
				t.Fatalf("err = %v, want %d %s", err, c.status, c.code)
			}
		})
	}
	if len(st.writes) != 0 {
		t.Errorf("a batch-level failure wrote %d batches", len(st.writes))
	}
}

func TestWriteFailuresAreRetryableAndDoNotLeakTheirText(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   protocol.ReasonCode
	}{
		"revoked inside the transaction": {store.ErrCredentialRevoked, 401, protocol.ReasonRevokedDevice},
		"ingest disabled":                {store.ErrTenantSuspended, 403, protocol.ReasonUnknownTenant},
		"database failure":               {errors.New(`invalid input syntax for type uuid: "sha256:aaa"`), 503, protocol.ReasonSchemaViolation},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			svc, st := newService(t)
			st.err = c.err
			_, err := svc.Submit(context.Background(), principal(), batchOf(t, promptM1(eventID(1))), receivedAt)
			var e *Error
			if !errors.As(err, &e) || e.Status != c.status || e.Code != c.code {
				t.Fatalf("err = %v, want %d %s", err, c.status, c.code)
			}
			if strings.Contains(e.Message, "sha256") || strings.Contains(e.Message, "syntax") {
				t.Errorf("the device-facing message carries the internal failure: %q", e.Message)
			}
		})
	}
}

func TestRouteTableFailureIsRetryable(t *testing.T) {
	svc, st := newService(t)
	st.routeErr = errors.New("connection refused")
	_, err := svc.Submit(context.Background(), principal(), batchOf(t, promptM1(eventID(1))), receivedAt)
	var e *Error
	if !errors.As(err, &e) || e.Status != 503 {
		t.Fatalf("err = %v, want a retryable 503", err)
	}
	st.routeErr = nil
	submit(t, svc, batchOf(t, promptM1(eventID(1))))
}
