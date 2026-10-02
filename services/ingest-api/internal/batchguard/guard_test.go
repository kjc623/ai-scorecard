package batchguard

import (
	"testing"
	"time"
)

// TestCheckAndMarkAreSeparate is the behaviour the ingest handler depends on: a batch refused for
// its shape must not be recorded, so the device can fix it and re-send under the same batch_id.
func TestCheckAndMarkAreSeparate(t *testing.T) {
	g := New(time.Hour)
	const tenant, device, batch = "t", "d", "b"

	if g.Check(tenant, device, batch) {
		t.Fatal("a batch that was never marked reported as seen")
	}
	if g.Check(tenant, device, batch) {
		t.Fatal("Check must not record the batch; only Mark does")
	}
	g.Mark(tenant, device, batch)
	if !g.Check(tenant, device, batch) {
		t.Fatal("a marked batch must report as seen")
	}
	// A different batch from the same device is unaffected.
	if g.Check(tenant, device, "other") {
		t.Error("a different batch_id reported as seen")
	}
	// A different device is unaffected, so one device cannot suppress another's batch id.
	if g.Check(tenant, "other-device", batch) {
		t.Error("another device's batch_id reported as seen")
	}
	// A different tenant is unaffected (C32: tenant is the leading dimension).
	if g.Check("other-tenant", device, batch) {
		t.Error("another tenant's batch_id reported as seen")
	}
}

func TestWindowExpiry(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := New(10 * time.Minute)
	g.SetNow(func() time.Time { return now })
	g.Mark("t", "d", "b")
	if !g.Check("t", "d", "b") {
		t.Fatal("inside the window the batch must report as seen")
	}
	now = now.Add(10*time.Minute + time.Second)
	if g.Check("t", "d", "b") {
		t.Error("outside the window the batch must be accepted again")
	}
}

// TestDisabledGuardNeverBlocks documents the visible-off switch: a non-positive window disables the
// guard rather than silently keeping a zero-length one.
func TestDisabledGuardNeverBlocks(t *testing.T) {
	g := New(0)
	g.Mark("t", "d", "b")
	if g.Check("t", "d", "b") {
		t.Error("a disabled guard must not report a batch as seen")
	}
}

// TestSweepBoundsTheMap pushes past the sweep threshold so the opportunistic cleanup is exercised
// rather than merely present.
func TestSweepBoundsTheMap(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := New(time.Minute)
	g.SetNow(func() time.Time { return now })

	for i := 0; i < 1500; i++ {
		g.Mark("t", "d", string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	now = now.Add(2 * time.Minute)
	g.Mark("t", "d", "fresh") // triggers the sweep

	g.mu.Lock()
	size := len(g.seen)
	g.mu.Unlock()
	if size > 1024 {
		t.Errorf("the guard map holds %d entries after a sweep; the sweep is not bounding it", size)
	}
}
