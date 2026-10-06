package rollup

import (
	"testing"
	"time"
)

func TestCoverageWindowForEndsAtNextMidnight(t *testing.T) {
	now := at(t, "2026-10-04T21:07:42Z")
	w, err := CoverageWindowFor(now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.From.Format(time.RFC3339), "2026-09-28T00:00:00Z"; got != want {
		t.Errorf("From = %s, want %s", got, want)
	}
	// The window includes the day that contains now, so today's coverage is written as it happens.
	if got, want := w.To.Format(time.RFC3339), "2026-10-05T00:00:00Z"; got != want {
		t.Errorf("To = %s, want %s", got, want)
	}
}

func TestCoverageWindowForRejectsZeroLookback(t *testing.T) {
	if _, err := CoverageWindowFor(time.Now(), 0); err == nil {
		t.Fatal("a zero-day coverage window was accepted")
	}
}
