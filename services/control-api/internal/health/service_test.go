package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/internal/store/storetest"
)

func validReport(at time.Time) protocol.HealthRequest {
	return protocol.HealthRequest{
		SchemaVersion:  protocol.HealthSchemaVersion,
		ReportedAt:     at,
		AgentVersion:   "test-agent/1",
		Hostname:       "LAPTOP-7",
		ManagedState:   "managed",
		CollectionMode: "m2",
		Spool:          protocol.SpoolHealth{DepthEvents: 7, DroppedTotal: 2},
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

// seed adds the tenant and device a health report authenticates as.
func seed(st *storetest.Memory) {
	st.AddTenant(store.Tenant{TenantID: "tenant-1", Status: "active", IngestEnabled: true, DeviceIdentity: protocol.DeviceIdentityClear})
	st.AddDevice(store.Device{TenantID: "tenant-1", DeviceID: "device-1", OS: "windows"})
}

// The health channel upserts one row per collector and stamps device activity in the same
// transaction; both facts must land.
func TestReportWritesCollectorStateAndDeviceActivity(t *testing.T) {
	st := storetest.New()
	seed(st)
	svc, err := New(st, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return at }

	resp, err := svc.Report(context.Background(), "tenant-1", "device-1", validReport(at))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if resp.NextReportAfterS != 900 {
		t.Errorf("next_report_after_s = %d, want 900", resp.NextReportAfterS)
	}
	// The response restates the tenant's identity setting so the device learns it.
	if resp.DeviceIdentity != protocol.DeviceIdentityClear {
		t.Errorf("device_identity = %q, want clear", resp.DeviceIdentity)
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
	// The device-level fields have no column and travel in the row's detail document.
	if len(row.Detail) == 0 {
		t.Error("device-level detail was not recorded")
	}
	// The device row carries the reported version, managed state and clear hostname.
	dev, err := st.Device(context.Background(), "tenant-1", "device-1")
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if dev.Hostname != "LAPTOP-7" {
		t.Errorf("device hostname = %q, want LAPTOP-7", dev.Hostname)
	}
	if dev.AgentVersion != "test-agent/1" {
		t.Errorf("device agent_version = %q, want test-agent/1", dev.AgentVersion)
	}
	if dev.ManagedState != "managed" {
		t.Errorf("device managed_state = %q, want managed", dev.ManagedState)
	}
}

// The per-tool collectors' causes are in the shared vocabulary, so a report carrying them passes
// validation and each row keeps its cause.
func TestReportStoresToolCollectorCauses(t *testing.T) {
	st := storetest.New()
	st.SetCollectors("egress_proxy", "tool_config_claude_code", "tool_config_codex", "tool_config_copilot",
		"tool_config_cursor", "otel_receiver", "hook_relay")
	seed(st)
	svc, err := New(st, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	req := validReport(at)
	rows := map[string]struct {
		state  protocol.CollectorState
		detail protocol.Detail
	}{
		"tool_config_claude_code": {protocol.StateDegraded, protocol.DetailConfigTampered},
		"tool_config_codex":       {protocol.StateDegraded, protocol.DetailNoRecentEvents},
		"tool_config_copilot":     {protocol.StateDegraded, protocol.DetailToolVersionUnsupported},
		"tool_config_cursor":      {protocol.StateAbsent, protocol.DetailDisabledByPolicy},
		"otel_receiver":           {protocol.StateDegraded, protocol.DetailConfigWriteFailed},
		"hook_relay":              {protocol.StateAbsent, protocol.DetailToolNotInstalled},
	}
	for collector, r := range rows {
		req.Collectors = append(req.Collectors, protocol.HealthReport{Collector: collector, State: r.state, Detail: r.detail})
	}
	if _, err := svc.Report(context.Background(), "tenant-1", "device-1", req); err != nil {
		t.Fatalf("Report: %v", err)
	}
	for collector, r := range rows {
		row, ok := st.CollectorStateAt("tenant-1", "device-1", collector)
		if !ok || row.State != string(r.state) || row.ErrorCode != string(r.detail) {
			t.Errorf("%s: row = %+v (%v), want %s with %s", collector, row, ok, r.state, r.detail)
		}
	}
}

func TestReportRefusesUnknownCollector(t *testing.T) {
	st := storetest.New()
	st.SetCollectors("egress_proxy")
	seed(st)
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

// An out-of-order report must not overwrite a newer row.
func TestReportDoesNotOverwriteNewerState(t *testing.T) {
	st := storetest.New()
	seed(st)
	svc, _ := New(st, Config{})
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }
	if _, err := svc.Report(context.Background(), "tenant-1", "device-1", validReport(base)); err != nil {
		t.Fatalf("first report: %v", err)
	}

	// A later report flips the state to degraded.
	svc.now = func() time.Time { return base.Add(time.Hour) }
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
	svc.now = func() time.Time { return base.Add(30 * time.Minute) }
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

func TestReportGatesClearHostnameOnTenantSetting(t *testing.T) {
	st := storetest.New()
	st.AddTenant(store.Tenant{TenantID: "tenant-1", Status: "active", IngestEnabled: true, DeviceIdentity: protocol.DeviceIdentityHashed})
	st.AddDevice(store.Device{TenantID: "tenant-1", DeviceID: "device-1", OS: "windows"})
	svc, _ := New(st, Config{})

	resp, err := svc.Report(context.Background(), "tenant-1", "device-1", validReport(time.Now()))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if resp.DeviceIdentity != protocol.DeviceIdentityHashed {
		t.Fatalf("device_identity = %q, want hashed", resp.DeviceIdentity)
	}
	dev, err := st.Device(context.Background(), "tenant-1", "device-1")
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	// The server is authoritative: a stale device that still sends a clear hostname to a 'hashed'
	// tenant must not have it stored.
	if dev.Hostname != "" {
		t.Errorf("a hashed tenant stored a clear hostname %q", dev.Hostname)
	}
}

func TestReportRefusesUnvalidatedShape(t *testing.T) {
	svc, _ := New(storetest.New(), Config{})
	req := validReport(time.Now())
	req.SchemaVersion = "9.9"
	_, err := svc.Report(context.Background(), "tenant-1", "device-1", req)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("error = %v, want 400", err)
	}
}
