package ladder

import (
	"encoding/json"
	"testing"
)

// The fixture is consumed by two test packages, so a mistake in it would be reported as a product
// failure in whichever package ran first. These checks are the fixture's own guard: they assert the
// table is well-formed rather than asserting any behaviour.

func TestFixtureIsWellFormed(t *testing.T) {
	if len(Scenarios) == 0 {
		t.Fatal("the ladder fixture carries no scenarios")
	}
	seen := map[string]bool{}
	for _, sc := range Scenarios {
		if sc.Name == "" || sc.Why == "" {
			t.Errorf("scenario %q must have a name and a reason", sc.Name)
		}
		if seen[sc.Name] {
			t.Errorf("scenario %q is defined twice; the two runners would report the same name", sc.Name)
		}
		seen[sc.Name] = true

		if len(sc.Steps) == 0 {
			t.Errorf("scenario %q has no steps", sc.Name)
		}
		eventIDs := map[string]bool{}
		for i, step := range sc.Steps {
			if step.Name == "" {
				t.Errorf("scenario %q step %d has no name", sc.Name, i)
			}
			switch step.Expect.Outcome {
			case "inserted", "merged", "duplicate":
			default:
				t.Errorf("scenario %q step %d expects outcome %q, outside ingest.record_event()'s vocabulary",
					sc.Name, i, step.Expect.Outcome)
			}
			if step.Expect.SameSubmissionAs >= i {
				t.Errorf("scenario %q step %d refers forward to step %d; the expectation can only point at an earlier step",
					sc.Name, i, step.Expect.SameSubmissionAs)
			}
			if step.Expect.SameSubmissionAs < -1 {
				t.Errorf("scenario %q step %d has SameSubmissionAs %d; use -1 for 'a new submission'",
					sc.Name, i, step.Expect.SameSubmissionAs)
			}
			if step.Expect.Outcome == "duplicate" && step.Expect.SameSubmissionAs < 0 {
				t.Errorf("scenario %q step %d is a duplicate with no submission to point at", sc.Name, i)
			}
			if step.ReceiveTime().IsZero() {
				t.Errorf("scenario %q step %d has no receive time", sc.Name, i)
			}

			var fields map[string]any
			if err := json.Unmarshal(step.Envelope, &fields); err != nil {
				t.Errorf("scenario %q step %d is not a JSON object: %v", sc.Name, i, err)
				continue
			}
			for _, required := range []string{"schema_version", "event_id", "tenant_id", "device_id", "kind", "source", "collection_mode", "dedup_key", "occurred_at"} {
				if _, ok := fields[required]; !ok {
					t.Errorf("scenario %q step %d is missing %s", sc.Name, i, required)
				}
			}
			if id, _ := fields["event_id"].(string); id != "" {
				// A repeated event_id is the duplicate case, which only the replay scenario may
				// construct; anywhere else it would silently test something other than the step says.
				if eventIDs[id] && sc.Name != "idempotent-replay" {
					t.Errorf("scenario %q reuses event_id %s; only the replay scenario may", sc.Name, id)
				}
				eventIDs[id] = true
			}
		}
	}
}

// TestOnlyTheReplayScenarioReusesAnEventID states the fixture's shape as a positive assertion, so a
// scenario cannot silently become a duplicate test.
func TestOnlyTheReplayScenarioReusesAnEventID(t *testing.T) {
	for _, sc := range Scenarios {
		seen := map[string]int{}
		for _, step := range sc.Steps {
			var fields map[string]any
			if err := json.Unmarshal(step.Envelope, &fields); err != nil {
				continue
			}
			id, _ := fields["event_id"].(string)
			seen[id]++
		}
		reused := false
		for _, n := range seen {
			if n > 1 {
				reused = true
			}
		}
		if reused && sc.Name != "idempotent-replay" {
			t.Errorf("scenario %q reuses an event_id; only the replay scenario should", sc.Name)
		}
		if !reused && sc.Name == "idempotent-replay" {
			t.Errorf("the replay scenario must resend the same event_id")
		}
	}
}

func TestHashAndUUIDHelpers(t *testing.T) {
	if got := Hash('a'); got != "sha256:"+repeat("a", 64) {
		t.Errorf("Hash = %q", got)
	}
	if got := DeterministicUUID(1); got != "00000001-0000-4000-8000-000000000001" {
		t.Errorf("DeterministicUUID = %q", got)
	}
	if DeterministicUUID(1) == DeterministicUUID(2) {
		t.Error("DeterministicUUID must differ per step")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
