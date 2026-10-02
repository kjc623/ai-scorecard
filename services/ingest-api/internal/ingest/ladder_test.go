package ingest

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/auth"
	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/dedup"
	"github.com/shadow-ai-capture/ingest-api/internal/ladder"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// The ladder suite. Every scenario in internal/ladder runs twice: here against the in-memory store,
// and in internal/store/db_integration_test.go against the real ingest.record_event() over
// PostgreSQL 17. The table lives in a normal package precisely so the two runs cannot drift.

func loadRoutes(t *testing.T) store.RouteTable {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "route-fidelity.seed.json")
	routes, err := store.LoadRouteTable(path)
	if err != nil {
		t.Fatalf("load route table: %v", err)
	}
	return routes
}

func loadSchema(t *testing.T) *contract.Schema {
	t.Helper()
	// Discover walks up from the test's working directory to contracts/event-envelope.schema.json,
	// so the test does not encode the package's depth in the tree.
	path, err := contract.Discover(".")
	if err != nil {
		t.Fatalf("discover contract schema: %v", err)
	}
	schema, err := contract.Load(path)
	if err != nil {
		t.Fatalf("load contract schema: %v", err)
	}
	return schema
}

func activePrincipal(mem *store.Memory) {
	mem.SetPrincipal(ladder.TenantID, ladder.DeviceID, "cred-1", store.PrincipalStatus{
		TenantKnown: true, TenantStatus: "active", IngestEnabled: true, TenantRegion: "eu-west",
		DeviceKnown: true, CredentialKnown: true,
		CredentialExpiry: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	})
}

func envelopeFacts(t *testing.T, raw json.RawMessage) (eventID, route, source string) {
	t.Helper()
	env, err := contract.DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("decode fixture envelope: %v", err)
	}
	eventID, _ = env.EventID()
	route, _ = env.Source()
	return eventID, route, route
}

// TestLadderAgainstTheStore is the §4 ladder conformance test. It runs every scenario through the
// store's single write entry point, which is the layer that owns the merge decision, and asserts
// ingest.record_event()'s own vocabulary: inserted / merged / duplicate.
func TestLadderAgainstTheStore(t *testing.T) {
	schema := loadSchema(t)
	routes := loadRoutes(t)

	for _, sc := range ladder.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			mem := store.NewMemory(routes)
			activePrincipal(mem)
			subIDs := make([]string, len(sc.Steps))

			for i, step := range sc.Steps {
				step := step
				t.Run(step.Name, func(t *testing.T) {
					// The fixture itself must be a valid device submission, or the scenario is
					// testing the validator rather than the ladder.
					env, err := contract.DecodeEnvelope(step.Envelope)
					if err != nil {
						t.Fatalf("fixture envelope did not decode: %v", err)
					}
					if v, err := schema.ValidateEnvelope(env); err != nil {
						t.Fatalf("fixture envelope failed the generated/schema check: %v", err)
					} else if v != nil {
						t.Fatalf("fixture envelope violates the contract at %s: %s", v.Pointer, v.Expected)
					}

					eventID, route, _ := envelopeFacts(t, step.Envelope)
					res, err := mem.WriteBatch(context.Background(), store.BatchWrite{
						TenantID:     ladder.TenantID,
						DeviceID:     ladder.DeviceID,
						CredentialID: "cred-1",
						ReceivedAt:   step.ReceiveTime(),
						Accepted: []store.AcceptedEvent{{
							Index: 0, EventID: eventID, Route: route, Envelope: step.Envelope,
						}},
					})
					if err != nil {
						t.Fatalf("WriteBatch: %v", err)
					}
					if len(res.Outcomes) != 1 {
						t.Fatalf("expected 1 outcome, got %d", len(res.Outcomes))
					}
					got := res.Outcomes[0]
					if string(got.Outcome) != step.Expect.Outcome {
						t.Errorf("outcome = %q, want %q", got.Outcome, step.Expect.Outcome)
					}
					if got.SubmissionID == "" {
						t.Errorf("outcome %q carried no submission_id", got.Outcome)
					}
					if step.Expect.SameSubmissionAs >= 0 {
						want := subIDs[step.Expect.SameSubmissionAs]
						if got.SubmissionID != want {
							t.Errorf("submission_id = %s, want the submission of step %d (%s)",
								got.SubmissionID, step.Expect.SameSubmissionAs, want)
						}
					}
					if step.Expect.WonFields != nil && got.WonFields != *step.Expect.WonFields {
						t.Errorf("won_fields = %v, want %v", got.WonFields, *step.Expect.WonFields)
					}
					subIDs[i] = got.SubmissionID
				})
			}
		})
	}
}

// TestLadderSameEventTwoRoutesThroughTheService asserts the wire-level contract of §5.3 for the
// two-route case: one logical submission, two accepted results, both carrying its id, and only the
// higher-fidelity observation reporting that it won the contested fields.
func TestLadderSameEventTwoRoutesThroughTheService(t *testing.T) {
	schema := loadSchema(t)
	routes := loadRoutes(t)
	mem := store.NewMemory(routes)
	activePrincipal(mem)
	svc, err := New(schema, mem, DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sc := ladder.Scenarios[0]
	if sc.Name != "same-event-two-routes" {
		t.Fatalf("fixture order changed: scenario 0 is %q", sc.Name)
	}

	batch := protocol.EventBatch{
		SchemaVersion: "1.0",
		BatchID:       "batch-two-routes",
		DeviceSentAt:  time.Date(2026, 10, 2, 14, 0, 3, 0, time.UTC),
		EventCount:    2,
		Events:        []json.RawMessage{sc.Steps[0].Envelope, sc.Steps[1].Envelope},
	}
	resp, err := svc.Submit(context.Background(), auth.Principal{
		TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
	}, batch, time.Date(2026, 10, 2, 14, 0, 4, 0, time.UTC))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if resp.Counts.Accepted != 2 || resp.Counts.Duplicate != 0 || resp.Counts.Rejected != 0 {
		t.Fatalf("counts = %+v, want 2 accepted", resp.Counts)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	a, b := resp.Results[0], resp.Results[1]
	if a.SubmissionID == "" || a.SubmissionID != b.SubmissionID {
		t.Errorf("both observations must report one submission: %q vs %q", a.SubmissionID, b.SubmissionID)
	}
	if a.DedupTier != "T" || b.DedupTier != "T" {
		t.Errorf("dedup_tier = %q/%q, want T/T (both carry a canonical content digest)", a.DedupTier, b.DedupTier)
	}
	if a.WonFields == nil || !*a.WonFields {
		t.Errorf("the first observation created the submission, so won_fields must be true")
	}
	if b.WonFields == nil || !*b.WonFields {
		t.Errorf("ext.page_context (rank 10) beats ext.web_request (rank 40), so the later observation wins the fields")
	}
	// I2a: two observations, one submission. Nothing discarded.
	if n := mem.ObservationCount(); n != 2 {
		t.Errorf("observations stored = %d, want 2 (append-only, one row per observation)", n)
	}
	subs := mem.Submissions()
	if len(subs) != 1 {
		t.Fatalf("submissions = %d, want 1", len(subs))
	}
	if subs[0].ObservationCount != 2 {
		t.Errorf("observation_count = %d, want 2", subs[0].ObservationCount)
	}
	if subs[0].WinningSource != "ext.page_context" {
		t.Errorf("winning_source = %q, want ext.page_context", subs[0].WinningSource)
	}
	if len(subs[0].ObservedRoutes) != 2 {
		t.Errorf("observed_routes = %v, want both routes recorded", subs[0].ObservedRoutes)
	}
}

// TestLadderUnreconcilableIsStoredNotMerged is the §4.5 rule stated as an assertion: where identity
// is not proven, both observations are stored and counted, and the pair is left visible.
func TestLadderUnreconcilableIsStoredNotMerged(t *testing.T) {
	schema := loadSchema(t)
	mem := store.NewMemory(loadRoutes(t))
	activePrincipal(mem)
	svc, err := New(schema, mem, DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var sc *ladder.Scenario
	for i := range ladder.Scenarios {
		if ladder.Scenarios[i].Name == "unreconcilable-tier-t-then-tier-s" {
			sc = &ladder.Scenarios[i]
		}
	}
	if sc == nil {
		t.Fatal("scenario unreconcilable-tier-t-then-tier-s is missing from the fixture")
	}

	for i, step := range sc.Steps {
		_, err := svc.Submit(context.Background(), auth.Principal{
			TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
		}, protocol.EventBatch{
			SchemaVersion: "1.0",
			BatchID:       "batch-unrec-" + string(rune('a'+i)),
			EventCount:    1,
			Events:        []json.RawMessage{step.Envelope},
		}, step.ReceiveTime())
		if err != nil {
			t.Fatalf("Submit %s: %v", step.Name, err)
		}
	}

	if n := mem.ObservationCount(); n != 2 {
		t.Errorf("observations = %d, want 2", n)
	}
	subs := mem.Submissions()
	if len(subs) != 2 {
		t.Fatalf("submissions = %d, want 2: a Tier-S observation has no text to compare, so it must not merge", len(subs))
	}
}

// TestLadderIdempotentReplayThroughTheService asserts §5.3's duplicate result: the submission is
// named and the *first* receipt is reported, because a retry is not a second receipt.
func TestLadderIdempotentReplayThroughTheService(t *testing.T) {
	schema := loadSchema(t)
	mem := store.NewMemory(loadRoutes(t))
	activePrincipal(mem)
	svc, err := New(schema, mem, DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var sc *ladder.Scenario
	for i := range ladder.Scenarios {
		if ladder.Scenarios[i].Name == "idempotent-replay" {
			sc = &ladder.Scenarios[i]
		}
	}
	if sc == nil {
		t.Fatal("scenario idempotent-replay is missing from the fixture")
	}

	first, err := svc.Submit(context.Background(), auth.Principal{
		TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
	}, protocol.EventBatch{
		SchemaVersion: "1.0", BatchID: "batch-replay-1", EventCount: 1,
		Events: []json.RawMessage{sc.Steps[0].Envelope},
	}, sc.Steps[0].ReceiveTime())
	if err != nil {
		t.Fatalf("Submit first: %v", err)
	}

	second, err := svc.Submit(context.Background(), auth.Principal{
		TenantID: ladder.TenantID, DeviceID: ladder.DeviceID, CredentialID: "cred-1",
	}, protocol.EventBatch{
		SchemaVersion: "1.0", BatchID: "batch-replay-2", EventCount: 1,
		Events: []json.RawMessage{sc.Steps[1].Envelope},
	}, sc.Steps[1].ReceiveTime())
	if err != nil {
		t.Fatalf("Submit replay: %v", err)
	}

	if first.Counts.Accepted != 1 {
		t.Fatalf("first submission counts = %+v, want 1 accepted", first.Counts)
	}
	if second.Counts.Duplicate != 1 {
		t.Fatalf("replay counts = %+v, want 1 duplicate (counted and reported, never silently dropped)", second.Counts)
	}
	got := second.Results[0]
	if got.Outcome != protocol.OutcomeDuplicate {
		t.Fatalf("replay outcome = %q, want duplicate", got.Outcome)
	}
	if got.SubmissionID != first.Results[0].SubmissionID {
		t.Errorf("replay reported submission %s, want the original %s", got.SubmissionID, first.Results[0].SubmissionID)
	}
	if got.FirstReceivedAt == nil {
		t.Fatal("a duplicate must report first_received_at")
	}
	want := sc.Steps[0].ReceiveTime()
	if !got.FirstReceivedAt.Equal(want) {
		t.Errorf("first_received_at = %s, want the first receipt %s (a retry does not re-stamp)",
			got.FirstReceivedAt, want)
	}
	if n := mem.ObservationCount(); n != 1 {
		t.Errorf("observations = %d, want 1: a replay is not a second observation", n)
	}
}

// TestBucketBoundaryArithmetic pins §4.3's floor arithmetic, including the edges a naive
// division would get wrong.
func TestBucketBoundaryArithmetic(t *testing.T) {
	cases := []struct {
		at   string
		want string
	}{
		{"2026-10-02T14:30:00Z", "2026-10-02T14:30:00Z"},
		{"2026-10-02T14:34:59Z", "2026-10-02T14:30:00Z"},
		{"2026-10-02T14:35:00Z", "2026-10-02T14:35:00Z"},
		{"2026-10-02T14:35:01Z", "2026-10-02T14:35:00Z"},
		{"2026-10-02T14:39:59.999Z", "2026-10-02T14:35:00Z"},
		{"2026-10-02T00:00:00Z", "2026-10-02T00:00:00Z"},
		{"1970-01-01T00:00:00Z", "1970-01-01T00:00:00Z"},
		{"1969-12-31T23:59:59Z", "1969-12-31T23:55:00Z"}, // floor, not truncation
	}
	for _, c := range cases {
		at, err := time.Parse(time.RFC3339, c.at)
		if err != nil {
			t.Fatalf("parse %s: %v", c.at, err)
		}
		want, _ := time.Parse(time.RFC3339, c.want)
		if got := dedup.BucketStart(at); !got.Equal(want) {
			t.Errorf("BucketStart(%s) = %s, want %s", c.at, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}
