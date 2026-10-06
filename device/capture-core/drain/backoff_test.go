package drain

import (
	"math/rand"
	"testing"
	"time"
)

func TestBackoffBounds(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: 300 * time.Second, Rand: rand.New(rand.NewSource(1))}
	for attempt := 0; attempt < 12; attempt++ {
		d := b.Delay(attempt)
		if d < 0 {
			t.Fatalf("attempt %d: negative delay %s", attempt, d)
		}
		if d > 300*time.Second {
			t.Fatalf("attempt %d: delay %s exceeds the cap", attempt, d)
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: 8 * time.Second}
	// attempt 0 -> bound 1s; attempt 1 -> 2s; 2 -> 4s; 3 -> 8s; 4 -> 8s (capped).
	wants := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, want := range wants {
		d := b.Delay(i)
		if d < 0 || d > want {
			t.Fatalf("attempt %d: delay %s over the bound %s", i, d, want)
		}
	}
}

func TestBackoffUsesFullRangeOverManySamples(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: 8 * time.Second, Rand: rand.New(rand.NewSource(7))}
	// Over many samples at one attempt the jitter must produce both small and near-bound values,
	// otherwise it is not full jitter but a fixed or equal-jitter delay.
	seenLow, seenHigh := false, false
	for i := 0; i < 2000; i++ {
		d := b.Delay(3) // bound 8s
		if d <= time.Second {
			seenLow = true
		}
		if d >= 7*time.Second {
			seenHigh = true
		}
	}
	if !seenLow || !seenHigh {
		t.Fatalf("full jitter did not span the range (low=%v high=%v)", seenLow, seenHigh)
	}
}

func TestBackoffZeroDefaults(t *testing.T) {
	b := Backoff{}
	d := b.Delay(5)
	if d < 0 || d > 300*time.Second {
		t.Fatalf("zero Backoff produced out-of-range delay %s", d)
	}
}
