package main

import (
	"errors"
	"io"
	"log/slog"
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
