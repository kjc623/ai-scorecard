package main

import (
	"context"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/protocol"
)

// The shared discovery emitter refuses (counting an error) until the device is enrolled, then is
// built once over the issued device id.
func TestDiscoveryEmitterWaitsForEnrolment(t *testing.T) {
	cloud := startFakeCloud(t, nil)
	svc, err := newService(context.Background(), testConfig(t, cloud, nil), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if svc.discovery == nil {
		t.Fatal("the service has no discovery emitter")
	}
	counters := core.NewCounterSet(time.Now())
	rec := discovery.Record{Type: protocol.DiscoveryTypeAppInstalled, Basis: protocol.DetectionBasisInstalledScan,
		Route: protocol.RouteInvScan, AppKey: "cursor", UserRef: discovery.UnattributedUserRef, OccurredAt: time.Now()}
	if err := svc.discovery.Emit(context.Background(), counters, rec); err == nil {
		t.Fatal("a discovery record was emitted before enrolment")
	}
	if got := counters.Cumulative()[protocol.CounterErrors]; got != 1 {
		t.Fatalf("errors = %d, want 1", got)
	}

	if err := (identityResolver{svc}).Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	first, err := svc.discovery.emitter()
	if err != nil {
		t.Fatalf("emitter after enrolment: %v", err)
	}
	if again, _ := svc.discovery.emitter(); again != first {
		t.Fatal("the emitter was built twice")
	}
}
