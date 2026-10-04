package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

func validReport(at time.Time) protocol.HealthRequest {
	return protocol.HealthRequest{
		SchemaVersion: protocol.HealthSchemaVersion,
		ReportedAt:    at,
		AgentVersion:  "test-agent/1",
		Spool:         protocol.SpoolHealth{DepthEvents: 7, DroppedTotal: 2},
		Collectors: []protocol.HealthReport{
			{
				Collector: "egress_proxy",
				State:     protocol.StateHealthy,
				Counters:  map[protocol.Counter]uint64{protocol.CounterObserved: 11},
				Version:   "9.9.9",
			},
		},
	}
}

// docs/02 §5.4: the health channel upserts one row per collector and stamps device activity in the
// same transaction. Both facts must land, and the closed vocabulary must be enforced.
func TestReportWritesCollectorStateAndDeviceActivity(t *testing.T) {
	st := store.NewMemory()
	svc, err := New(st, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return at })

	resp, err := svc.Report(context.Background(), "tenant-1", "device-1", validReport(at))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if resp.NextReportAfterS != 900 {
		t.Errorf("next_report_after_s = %d, want 900", resp.NextReportAfterS)
	}
	row, ok := st.CollectorStateAt("tenant-1", "device-1", "egress_proxy")
	if !ok {
		t.Fatal("collector_state row was not written")
	}
	if row.State != "healthy" || row.Version != "9.9.9" {
		t.Errorf("row = %+v", row)
	}
	if row.SpoolDepth == nil || *row.SpoolDepth != 7 || row.SpoolDroppedTotal != 2 {
		t.Errorf("spool depth/dropped not recorded: %+v", row)
	}
	seen, ok := st.DeviceLastSeenAt("tenant-1", "device-1")
	if !ok || !seen.Equal(at) {
		t.Errorf("last_seen_at = %v (%v), want %v", seen, ok, at)
	}
	// The device-level fields have no column and travel in the row's detail document (docs/02 §9).
	if len(row.Detail) == 0 {
		t.Error("device-level detail was not recorded")
	}
}

func TestReportRefusesUnknownCollector(t *testing.T) {
	st := store.NewMemory()
	st.SetCollectors("egress_proxy")
	svc, _ := New(st, Config{})
	req := validReport(time.Now())
	req.Collectors[0].Collector = "not_a_collector"
	_, err := svc.Report(context.Background(), "tenant-1", "device-1", req)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an *apierr.Error", err)
	}
	if apiErr.Status != 400 {
		t.Errorf("status = %d, want 400", apiErr.Status)
	}
	if _, ok := st.CollectorStateAt("tenant-1", "device-1", "not_a_collector"); ok {
		t.Error("an unknown collector was stored")
	}
}

// The stale-report guard: an out-of-order report must not overwrite a newer row (docs/02 §5.4).
func TestReportDoesNotOverwriteNewerState(t *testing.T) {
	st := store.NewMemory()
	svc, _ := New(st, Config{})
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return base })
	if _, err := svc.Report(context.Background(), "tenant-1", "device-1", validReport(base)); err != nil {
		t.Fatalf("first report: %v", err)
	}

	// A later report flips the state to degraded.
	svc.SetClock(func() time.Time { return base.Add(time.Hour) })
	later := validReport(base.Add(time.Hour))
	later.Collectors[0].State = protocol.StateDegraded
	if _, err := svc.Report(context.Background(), "tenant-1", "device-1", later); err != nil {
		t.Fatalf("later report: %v", err)
	}
	row, _ := st.CollectorStateAt("tenant-1", "device-1", "egress_proxy")
	if row.State != "degraded" {
		t.Fatalf("state = %q, want degraded", row.State)
	}

	// A stale report (older timestamp) must be ignored.
	svc.SetClock(func() time.Time { return base.Add(30 * time.Minute) })
	stale := validReport(base.Add(30 * time.Minute))
	stale.Collectors[0].State = protocol.StateTampered
	if _, err := svc.Report(context.Background(), "tenant-1", "device-1", stale); err != nil {
		t.Fatalf("stale report: %v", err)
	}
	row, _ = st.CollectorStateAt("tenant-1", "device-1", "egress_proxy")
	if row.State != "degraded" {
		t.Fatalf("a stale report overwrote state: got %q", row.State)
	}
}

func TestReportRefusesUnvalidatedShape(t *testing.T) {
	svc, _ := New(store.NewMemory(), Config{})
	req := validReport(time.Now())
	req.SchemaVersion = "9.9"
	_, err := svc.Report(context.Background(), "tenant-1", "device-1", req)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("error = %v, want 400", err)
	}
}
