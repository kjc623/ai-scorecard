package spool

import (
	"fmt"
	"sync"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

// One writer means one writer *process*; within the process the spool serialises callers.
// The race detector needs cgo, which this offline host does not have, so this test exercises
// the same hazard the detector would: many goroutines appending, peeking and reading stats
// at once, with the resulting counts asserted exactly. A missing or partial lock shows up as
// a wrong count or a map corruption rather than as a flake.
func TestConcurrentCallersAreSerialised(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir, func(c *Config) {
		c.Bounds = Bounds{MaxEntries: 0} // unbounded: every append must survive
	})

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers+4)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				e := testEntry(w*perWriter + i)
				if _, err := sp.Append(e); err != nil {
					errs <- fmt.Errorf("append: %w", err)
					return
				}
			}
		}(w)
	}
	// Readers run concurrently with the writers.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := sp.Peek(10); err != nil {
					errs <- fmt.Errorf("peek: %w", err)
					return
				}
				_ = sp.Stats()
				_ = sp.Depth()
				_ = sp.DroppedTotal()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent caller: %v", err)
	}

	want := writers * perWriter
	if got := sp.Depth(); got != want {
		t.Fatalf("Depth = %d, want %d: appends were lost or double counted", got, want)
	}
	entries, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(entries) != want {
		t.Fatalf("Peek returned %d records, want %d", len(entries), want)
	}
	seen := make(map[uint64]bool, want)
	for _, e := range entries {
		if seen[e.Seq] {
			t.Fatalf("sequence %d was assigned twice", e.Seq)
		}
		seen[e.Seq] = true
	}
	for i := 0; i < want; i++ {
		if !seen[uint64(i)] {
			t.Fatalf("sequence %d is missing: sequences must be contiguous and never reused", i)
		}
	}
	// The spool is still coherent after the concurrent phase, in a fresh process.
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sp2 := openTest(t, dir)
	if got := sp2.Depth(); got != want {
		t.Fatalf("Depth = %d after reopen, want %d", got, want)
	}
	if got := sp2.Stats(); got.DroppedTotal != 0 || got.Depth != want {
		t.Fatalf("stats after reopen: %+v", got)
	}
	var _ protocol.Store = sp2
}
