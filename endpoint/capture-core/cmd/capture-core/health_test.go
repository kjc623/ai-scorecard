package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// The drain's degraded state and error_code must surface on the device-level health document
// through healthChannel.Snapshot — not only through drainer.Status() — so a failing drain is
// visible to the reporting layer (C23/C25).
func TestHealthSnapshotSurfacesDrainDegraded(t *testing.T) {
	d, err := drain.New(drain.Config{
		Endpoint:      "https://ingest.example.invalid",
		AuthMode:      protocol.AuthModeDPoP,
		TenantID:      "tenant-1",
		DeviceID:      "device-1",
		BackoffBase:   time.Second,
		BackoffCap:    time.Second,
		DrainInterval: time.Second,
	}, func() (protocol.Store, error) { return nil, errors.New("unused") }, nil, nil, time.Now)
	if err != nil {
		t.Fatalf("drain.New: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &service{
		cfg:     Config{DeviceID: "device-1"},
		log:     logger,
		reg:     core.NewRegistry(time.Now, nil),
		host:    &classifierHostController{},
		sink:    &lazySink{spool: &spoolHolder{}},
		result:  policy.Result{},
		drainer: d,
	}
	hc := newHealthChannel(Config{DeviceID: "device-1"}, logger, svc)

	snap := hc.Snapshot()
	if snap.Drain == nil {
		t.Fatal("the health snapshot carried no drain row")
	}
	if snap.Drain.State != protocol.StateDegraded {
		t.Fatalf("drain state = %q, want degraded", snap.Drain.State)
	}
	if snap.Drain.Detail != protocol.DetailUpstreamUnreachable {
		t.Fatalf("drain error_code = %q, want %q", snap.Drain.Detail, protocol.DetailUpstreamUnreachable)
	}
}

// fakeProvider is a registry entry whose only job is to carry a route and a health row.
type fakeProvider struct {
	route protocol.Route
	state protocol.CollectorState
}

func (p fakeProvider) Name() protocol.Route        { return p.route }
func (p fakeProvider) Start(context.Context) error { return nil }
func (p fakeProvider) Stop(context.Context) error  { return nil }
func (p fakeProvider) Health() core.Health {
	return core.Healthy(protocol.DetailNone, time.Now(), time.Now(), nil)
}
func (p fakeProvider) ApplyPolicy(policy.Bundle) error { return nil }

// docs/01 §4.3: the collector name must come from ref.collector, so the health request cannot carry
// a raw route name. This pins the mapping for the one route that differs most visibly.
func TestHealthRequestMapsRoutesToCollectorCodes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := core.NewRegistry(time.Now, nil)
	if err := reg.Add(fakeProvider{route: protocol.RouteProxyTLS}); err != nil {
		t.Fatalf("reg.Add: %v", err)
	}
	reg.StartAll(context.Background())

	svc := &service{
		cfg:    Config{DeviceID: "device-1"},
		log:    logger,
		reg:    reg,
		host:   &classifierHostController{},
		sink:   &lazySink{spool: &spoolHolder{}},
		result: policy.Result{},
	}
	hc := newHealthChannel(Config{DeviceID: "device-1"}, logger, svc)
	req := hc.healthRequest()
	if err := req.Validate(); err != nil {
		t.Fatalf("built health request is invalid: %v", err)
	}

	names := map[string]bool{}
	for _, c := range req.Collectors {
		names[c.Collector] = true
		if strings.Contains(c.Collector, ".") || strings.Contains(c.Collector, "-") {
			t.Errorf("collector %q is not a ref.collector code", c.Collector)
		}
	}
	if !names["egress_proxy"] {
		t.Errorf("proxy.tls did not map to egress_proxy: %v", names)
	}
	if !names["classifier_host"] {
		t.Errorf("classifier-host did not report a coverage row: %v", names)
	}
}
