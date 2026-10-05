package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// docs/02-ingest-and-transport.md §5.4: a health report is a keyed upsert of current state, not an
// event. These tests pin the two properties the wire type must hold: the server can tell a valid
// report from an invalid one, and the counters map is the closed seven even when the device omits
// some.

func TestHealthRequestValidate(t *testing.T) {
	good := HealthRequest{
		SchemaVersion: HealthSchemaVersion,
		ReportedAt:    time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC),
		Spool:         SpoolHealth{DepthEvents: 4, DroppedTotal: 1},
		Collectors: []HealthReport{
			NewHealthReport("device-1", "egress_proxy", "1.2.3", time.Now()),
		},
	}
	good.Collectors[0].State = StateHealthy
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed report was refused: %v", err)
	}

	cases := map[string]func(*HealthRequest){
		"schema version":  func(r *HealthRequest) { r.SchemaVersion = "2.0" },
		"no reported_at":  func(r *HealthRequest) { r.ReportedAt = time.Time{} },
		"no collectors":   func(r *HealthRequest) { r.Collectors = nil },
		"negative depth":  func(r *HealthRequest) { r.Spool.DepthEvents = -1 },
		"unknown state":   func(r *HealthRequest) { r.Collectors[0].State = "ok" },
		"unknown detail":  func(r *HealthRequest) { r.Collectors[0].Detail = "made_up_cause" },
		"unknown counter": func(r *HealthRequest) { r.Collectors[0].Counters[Counter("made_up")] = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := good
			r.Collectors = append([]HealthReport(nil), good.Collectors...)
			r.Collectors[0].Counters = map[Counter]uint64{}
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("report with %s was accepted", name)
			}
		})
	}
}

// The closed counter set is the mechanism that stops a provider inventing a high-cardinality metric
// (docs/01-collectors.md §4.3, A15). A report that carries all seven is the shape; a report that
// omits one is not confused with one that is genuinely zero because the reader fills the set.
func TestNewHealthReportCarriesTheClosedCounters(t *testing.T) {
	rep := NewHealthReport("d", "egress_proxy", "1", time.Now())
	if len(rep.Counters) != len(AllCounters) {
		t.Fatalf("counters = %d, want the closed %d", len(rep.Counters), len(AllCounters))
	}
	for _, c := range AllCounters {
		if _, ok := rep.Counters[c]; !ok {
			t.Errorf("counter %q is missing", c)
		}
	}
}

func TestHealthRequestJSONRoundTrip(t *testing.T) {
	in := HealthRequest{
		SchemaVersion: HealthSchemaVersion,
		ReportedAt:    time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC),
		AgentVersion:  "1.2.3",
		Spool:         SpoolHealth{DepthEvents: 4},
		Collectors: []HealthReport{{
			Collector: "process_detector",
			State:     StateDegraded,
			Detail:    DetailEnumerationPartial,
			Counters:  map[Counter]uint64{CounterObserved: 3},
		}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "tenant_id") || strings.Contains(string(raw), "device_id\":\"\"") {
		t.Fatalf("a health request must not carry a tenant or an empty device id: %s", raw)
	}
	var out HealthRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("round-tripped report is invalid: %v", err)
	}
	if out.Collectors[0].Detail != DetailEnumerationPartial {
		t.Fatalf("detail = %q", out.Collectors[0].Detail)
	}
}
