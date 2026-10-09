package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/etwsession"
	"github.com/shadow-ai-capture/device/capture-core/flowmon"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// testFlowEvents is a DNS and connect event source a test writes into.
type testFlowEvents struct{ events chan etwsession.Event }

func (s *testFlowEvents) Events() <-chan etwsession.Event { return s.events }
func (s *testFlowEvents) Close()                          {}

// The service runs the flow monitor while the bundle switches it on, and a connect to an address
// a catalog inference domain resolved to reaches the edge as an inference_connection from the
// issued device on route net.flow.
func TestFlowMonitorReportsAnInferenceConnection(t *testing.T) {
	src := &testFlowEvents{events: make(chan etwsession.Event, 4)}
	saved := platform.flowEvents
	platform.flowEvents = func() (flowmon.Source, error) { return src, nil }
	t.Cleanup(func() { platform.flowEvents = saved })

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b := &policy.Bundle{
		Version:       "5",
		EffectiveAt:   time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		TenantDefault: protocol.ModeM0,
		Catalog: []policy.CatalogApp{{AppKey: "openai_api", Category: "inference_api", Signals: []policy.CatalogSignal{
			{Platform: "any", Kind: policy.SignalInferenceDomain, Value: "api.openai.com"},
		}}},
	}
	b.Endpoint.Flows.Enabled = true
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
	if h, ok := svc.reg.HealthFor(protocol.CollectorFlowMonitor); !ok || h.State != protocol.StateHealthy {
		t.Fatalf("flow_monitor health = %+v, want healthy", h)
	}

	// The test process itself connects, so the process lookup answers wherever it runs.
	pid := uint32(os.Getpid())
	now := time.Now()
	src.events <- etwsession.Event{
		Provider: flowmon.DNSClient.GUID, ID: 3008, PID: pid, Time: now,
		Properties: map[string]string{"QueryName": "api.openai.com", "QueryStatus": "0", "QueryResults": "::ffff:162.159.140.245;"},
	}
	src.events <- etwsession.Event{
		Provider: flowmon.KernelNetwork.GUID, ID: 12, Time: now,
		Properties: map[string]string{"PID": strconv.FormatUint(uint64(pid), 10), "daddr": "162.159.140.245", "dport": "443"},
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
		DestinationHost string `json:"destination_host"`
	}
	if err := json.Unmarshal(events[0], &env); err != nil {
		t.Fatal(err)
	}
	if env.DeviceID != cloudDevice || env.Kind != "discovery" || env.Source != "net.flow" ||
		env.ToolFingerprint != "app:openai_api" || env.DiscoveryType != "inference_connection" ||
		env.DetectionBasis != "flow_metadata" || env.DestinationHost != "api.openai.com" {
		t.Fatalf("delivered envelope = %s", events[0])
	}
}
