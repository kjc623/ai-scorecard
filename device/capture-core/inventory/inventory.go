// Package inventory is the inventory_scanner collector: while the bundle switches it on, it scans
// what is installed on the device at start and then every endpoint.inventory.interval_minutes,
// matches what it finds against the app catalog, and emits each match as a discovery record on
// route inv.scan through the discovery emitter, which de-duplicates per day and keeps the budget.
package inventory

import (
	"context"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// defaultInterval is the rescan interval until a bundle states one.
const defaultInterval = 360 * time.Minute

// Scanner is one kind of thing a scan looks for. Scan returns the catalog matches it found, and one
// error per place it could not read; it never starts a program it finds.
type Scanner interface {
	Scan(ctx context.Context, b *policy.Bundle) ([]discovery.Record, []error)
}

// CountedScanner is a Scanner that also counts each file it checks as observed; the provider scans
// it through ScanCounted with its own counters.
type CountedScanner interface {
	Scanner
	ScanCounted(ctx context.Context, b *policy.Bundle, counters *core.CounterSet) ([]discovery.Record, []error)
}

// Emitter is the part of the discovery emitter the provider uses.
type Emitter interface {
	Emit(ctx context.Context, collector *core.CounterSet, r discovery.Record) error
}

// Config is the provider's wiring.
type Config struct {
	// Scanners is the scan's pass, in order. Empty where the platform has no scanner, and the
	// provider then reports absent.
	Scanners []Scanner
	Emitter  Emitter
	// Bundles returns the bundle in force, whose catalog the scanners match against.
	Bundles func() *policy.Bundle
	Log     core.Logger
	Clock   func() time.Time
	// After waits between scans; time.After when nil.
	After func(time.Duration) <-chan time.Time
}

// Provider is the inventory_scanner collector.
type Provider struct {
	cfg      Config
	counters *core.CounterSet

	// life serialises Start and Stop.
	life sync.Mutex

	mu       sync.Mutex
	running  bool
	interval time.Duration
	scanned  bool // a scan finished since Start
	partial  bool // the last scan could not read everything
	// found is the last finished scan's catalog apps, each with the lowest version found ("" when
	// no record of it carried one).
	found       map[string]string
	since       time.Time
	lastSuccess time.Time
	cancel      context.CancelFunc
	done        chan struct{}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// New returns the provider.
func New(cfg Config) *Provider {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Bundles == nil {
		cfg.Bundles = func() *policy.Bundle { return nil }
	}
	now := cfg.Clock()
	return &Provider{cfg: cfg, counters: core.NewCounterSet(now), since: now, interval: defaultInterval}
}

// Name implements core.Provider.
func (p *Provider) Name() protocol.Collector { return protocol.CollectorInventoryScanner }

// Enabled implements core.Toggled.
func (p *Provider) Enabled(b *policy.Bundle) bool { return b != nil && b.Endpoint.Inventory.Enabled }

// Counters exposes the provider's counter set.
func (p *Provider) Counters() *core.CounterSet { return p.counters }

// ApplyPolicy records the rescan interval. The wait already under way keeps its length; the next
// one uses the new interval.
func (p *Provider) ApplyPolicy(b policy.Bundle) error {
	if m := b.Endpoint.Inventory.IntervalMinutes; m > 0 {
		p.mu.Lock()
		p.interval = time.Duration(m) * time.Minute
		p.mu.Unlock()
	}
	return nil
}

// Start runs the first scan, then rescans on the interval until Stop.
func (p *Provider) Start(ctx context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil
	}
	p.running = true
	p.scanned, p.partial = false, false
	p.since = p.cfg.Clock()
	p.mu.Unlock()
	if len(p.cfg.Scanners) == 0 {
		return nil
	}

	p.scan(ctx)
	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	p.cancel, p.done = cancel, done
	p.mu.Unlock()
	go p.loop(loopCtx, done)
	return nil
}

// Stop ends the rescans, waiting for a scan under way to finish its current read.
func (p *Provider) Stop(context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	p.mu.Lock()
	p.running = false
	p.found = nil
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}

func (p *Provider) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		p.mu.Lock()
		wait := p.interval
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-p.cfg.After(wait):
		}
		p.scan(ctx)
	}
}

// scan runs one pass of every scanner and emits what they matched.
func (p *Provider) scan(ctx context.Context) {
	b := p.cfg.Bundles()
	now := p.cfg.Clock()
	failed := 0
	found := map[string]string{}
	for _, s := range p.cfg.Scanners {
		var recs []discovery.Record
		var errs []error
		if c, ok := s.(CountedScanner); ok {
			recs, errs = c.ScanCounted(ctx, b, p.counters)
		} else {
			recs, errs = s.Scan(ctx, b)
		}
		for _, err := range errs {
			p.counters.Add(protocol.CounterErrors)
			p.cfg.Log.Printf("inventory: %v", err)
		}
		failed += len(errs)
		for _, r := range recs {
			if v, seen := found[r.AppKey]; !seen || lowerVersion(r.Version, v) {
				found[r.AppKey] = r.Version
			}
			r.Route = protocol.RouteInvScan
			if r.OccurredAt.IsZero() {
				r.OccurredAt = now
			}
			if err := p.cfg.Emitter.Emit(ctx, p.counters, r); err != nil {
				p.cfg.Log.Printf("inventory: %v", err)
			}
		}
	}
	complete := failed == 0 && ctx.Err() == nil
	p.mu.Lock()
	p.scanned, p.partial = true, !complete
	if ctx.Err() == nil && p.running {
		p.found = found
	}
	if complete {
		p.lastSuccess = now
	}
	p.mu.Unlock()
}

// Installed reports whether the last scan found the catalog app appKey, and its version: the
// lowest found when it is installed more than once, "" when no record carried one. It reports
// nothing before the first scan and while the scanner is switched off.
func (p *Provider) Installed(appKey string) (version string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	version, ok = p.found[appKey]
	return version, ok
}

// lowerVersion reports whether candidate is a lower release than current. A version that is not a
// release number is never lower, so a known version replaces an unknown one.
func lowerVersion(candidate, current string) bool {
	if !releaseName.MatchString(candidate) {
		return false
	}
	return !releaseName.MatchString(current) || compareRelease(candidate, current) < 0
}

// Health implements core.Provider: healthy after a complete scan, degraded with
// enumeration_partial until one has run or when the last could not read everything.
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	running, scanned, partial, since, last := p.running, p.scanned, p.partial, p.since, p.lastSuccess
	p.mu.Unlock()
	switch {
	case len(p.cfg.Scanners) == 0:
		return core.Absent(protocol.DetailToolVersionUnsupported, since, p.counters)
	case !running:
		return core.Absent(protocol.DetailNone, since, p.counters)
	case !scanned || partial:
		return core.Degraded(protocol.DetailEnumerationPartial, since, last, p.counters)
	}
	return core.Healthy(protocol.DetailNone, since, last, p.counters)
}
