package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/etwsession"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/procmon"
	"github.com/shadow-ai-capture/device/protocol"
)

// testProcessEvents is a process event source a test writes into.
type testProcessEvents struct{ events chan etwsession.Event }

func (s *testProcessEvents) Events() <-chan etwsession.Event { return s.events }
func (s *testProcessEvents) Close()                          {}

// The service runs the process monitor while the bundle switches it on, and a catalog app's start
// reaches the edge as an app_running discovery from the issued device on route proc.detect. The
// process cannot be opened, so the record is unattributed.
func TestProcessMonitorReportsARunningApp(t *testing.T) {
	src := &testProcessEvents{events: make(chan etwsession.Event, 4)}
	saved := platform.processEvents
	platform.processEvents = func() (procmon.Source, error) { return src, nil }
	t.Cleanup(func() { platform.processEvents = saved })

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b := &policy.Bundle{
		Version:       "5",
		EffectiveAt:   time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		TenantDefault: protocol.ModeM0,
		Catalog: []policy.CatalogApp{{AppKey: "cursor", Category: "ide", Signals: []policy.CatalogSignal{
			{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "Cursor.exe"},
		}}},
	}
	b.Endpoint.Processes.Enabled = true
	b.Endpoint.DiscoveryDailyBudget = 200
	raw, err := policy.Sign("policy-key-1", priv, b)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, raw)
	svc, err := newService(context.Background(), testConfig(t, cloud, pub), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Stop(context.Background()) }()
	if h, ok := svc.reg.HealthFor(protocol.CollectorProcessDetector); !ok || h.State != protocol.StateHealthy {
		t.Fatalf("process_detector health = %+v, want healthy", h)
	}

	src.events <- etwsession.Event{
		Provider: procmon.KernelProcess.GUID,
		ID:       1,
		Time:     time.Now(),
		Properties: map[string]string{
			"ProcessID":       "4000000000",
			"ParentProcessID": "4",
			"ImageName":       `\Device\HarddiskVolume3\Program Files\cursor\Cursor.exe`,
		},
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(cloud.receivedEvents()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	events := cloud.receivedEvents()
	if len(events) != 1 {
		t.Fatalf("the edge received %d events, want 1", len(events))
	}
	var env struct {
		DeviceID        string `json:"device_id"`
		Kind            string `json:"kind"`
		Source          string `json:"source"`
		ToolFingerprint string `json:"tool_fingerprint"`
		DiscoveryType   string `json:"discovery_type"`
		DetectionBasis  string `json:"detection_basis"`
		UserRef         string `json:"user_ref"`
	}
	if err := json.Unmarshal(events[0], &env); err != nil {
		t.Fatal(err)
	}
	if env.DeviceID != cloudDevice || env.Kind != "discovery" || env.Source != "proc.detect" ||
		env.ToolFingerprint != "app:cursor" || env.DiscoveryType != "app_running" ||
		env.DetectionBasis != "process_event" || env.UserRef != unattributedUserRef {
		t.Fatalf("delivered envelope = %s", events[0])
	}
}
