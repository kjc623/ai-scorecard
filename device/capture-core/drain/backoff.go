package drain

import (
	"math/rand"
	"time"
)

// Backoff is exponential backoff with full jitter: the bound doubles per attempt, is capped, and
// the delay is uniform in [0, bound]. Full jitter is what spreads a fleet's flush after an outage.
type Backoff struct {
	Base time.Duration
	Cap  time.Duration
	Rand *rand.Rand
}

// Delay returns the delay for the next attempt, where attempt is zero-based (the first retry is
// attempt 0).
func (b Backoff) Delay(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	cap := b.Cap
	if cap <= 0 {
		cap = 300 * time.Second
	}
	if attempt < 0 {
		attempt = 0
	}
	bound := base
	for i := 0; i < attempt && bound < cap; i++ {
		bound *= 2
		if bound > cap || bound <= 0 {
			bound = cap
			break
		}
	}
	r := b.Rand
	if r == nil {
		r = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if bound <= 0 {
		return 0
	}
	// Int63n panics on n<=0; bound is a positive duration here.
	return time.Duration(r.Int63n(int64(bound) + 1))
}
