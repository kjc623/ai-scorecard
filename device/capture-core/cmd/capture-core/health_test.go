package main

import (
	"testing"
	"time"
)

// The report after the first follows within a minute whatever cadence the control plane asks for,
// so the state collectors are in while the agent starts does not stand for a whole interval; every
// later report keeps the cadence.
func TestTheSecondHealthReportFollowsTheFirstWithinAMinute(t *testing.T) {
	for _, c := range []struct {
		first    bool
		interval time.Duration
		want     time.Duration
	}{
		{true, 900 * time.Second, healthInterval},
		{true, minHealthInterval, minHealthInterval},
		{false, 900 * time.Second, 900 * time.Second},
		{false, minHealthInterval, minHealthInterval},
	} {
		if got := nextReport(c.first, c.interval); got != c.want {
			t.Errorf("nextReport(%t, %s) = %s, want %s", c.first, c.interval, got, c.want)
		}
	}
}
