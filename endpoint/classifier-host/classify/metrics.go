package classify

import (
	"sort"
	"sync"
	"time"
)

// StageStats is one stage's rolling distribution. §9.4: "The device records the rolling p95 per
// stage on the health channel, so Q4 is answered from production telemetry per device class
// rather than one benchmark."
type StageStats struct {
	Stage string
	Count int
	P50   time.Duration
	P95   time.Duration
	Max   time.Duration
}

// Metrics is a fixed-window per-stage latency recorder. It is deliberately allocation-free on
// the hot path (a ring buffer per stage) because it runs on the interactive path it measures.
type Metrics struct {
	mu     sync.Mutex
	window int
	idx    map[string]int
	full   map[string]bool
	buf    map[string][]time.Duration
}

// NewMetrics returns a recorder keeping the last window samples per stage. A window of zero or
// less means the default of 256.
func NewMetrics(window int) *Metrics {
	if window <= 0 {
		window = 256
	}
	return &Metrics{window: window, idx: map[string]int{}, full: map[string]bool{}, buf: map[string][]time.Duration{}}
}

// Observe records one stage duration.
func (m *Metrics) Observe(stage string, d time.Duration) {
	if m == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buf[stage]
	if !ok {
		b = make([]time.Duration, m.window)
		m.buf[stage] = b
	}
	i := m.idx[stage]
	b[i] = d
	i++
	if i >= m.window {
		i = 0
		m.full[stage] = true
	}
	m.idx[stage] = i
}

// Stats returns the rolling distribution for one stage.
func (m *Metrics) Stats(stage string) StageStats {
	if m == nil {
		return StageStats{Stage: stage}
	}
	m.mu.Lock()
	b := m.buf[stage]
	n := m.idx[stage]
	if m.full[stage] {
		n = m.window
	}
	sample := make([]time.Duration, 0, n)
	sample = append(sample, b[:n]...)
	m.mu.Unlock()

	st := StageStats{Stage: stage, Count: len(sample)}
	if len(sample) == 0 {
		return st
	}
	sort.Slice(sample, func(i, j int) bool { return sample[i] < sample[j] })
	st.P50 = sample[percentileIndex(len(sample), 50)]
	st.P95 = sample[percentileIndex(len(sample), 95)]
	st.Max = sample[len(sample)-1]
	return st
}

// Snapshot returns every stage's stats, keyed by stage name.
func (m *Metrics) Snapshot() map[string]StageStats {
	out := map[string]StageStats{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	stages := make([]string, 0, len(m.buf))
	for s := range m.buf {
		stages = append(stages, s)
	}
	m.mu.Unlock()
	for _, s := range stages {
		out[s] = m.Stats(s)
	}
	return out
}

// percentileIndex is the nearest-rank index for a percentile over a sorted sample: the smallest
// value at or above the requested share. It is the definition §9.4's p95 implies and it needs no
// interpolation, so it behaves the same on both targets.
func percentileIndex(n, p int) int {
	if n <= 0 {
		return 0
	}
	rank := (p*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return rank - 1
}
