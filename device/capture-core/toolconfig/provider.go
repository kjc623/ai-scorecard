package toolconfig

import (
	"context"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// tool is what a provider knows about the tool it configures.
type tool struct {
	key         string // endpoint.tools key
	collector   protocol.Collector
	fingerprint string
	// supported is whether the agent writes the tool's configuration on this platform.
	supported bool
	// hooks is whether the provider declares the tool's hooks, switched by the tool's hooks entry
	// while the hook relay is on, rather than pointing its OTel export at the receiver. Its Desired
	// is empty: the hook command depends on nothing in the bundle.
	hooks bool
}

// Config is a provider's seams.
type Config struct {
	// Token returns the OTLP receiver's bearer token.
	Token func() string
	// Scope names the device and the user a tool's collection mode is resolved for. nil resolves
	// with neither, so the tenant default applies on those axes.
	Scope func() core.ScopeQuery
	Log   core.Logger
	Clock func() time.Time
}

// Provider is one tool's tool_config_<tool> collector: while the bundle switches the tool's OTel
// export (or, for a hooks tool, its hooks) on, the tool's managed configuration carries the agent's
// keys. Start applies them, Stop removes them, and a bundle that changes the collection mode, the
// receiver's address or the token applies them again.
type Provider struct {
	tool tool
	w    Writer
	cfg  Config

	// life serialises Start, Stop and ApplyPolicy.
	life sync.Mutex

	mu          sync.Mutex
	running     bool
	want        Desired
	haveWant    bool
	attempted   bool // an apply ran since Start; applied, installed and writeErr describe it
	applied     Desired
	installed   bool
	writeErr    error
	startedAt   time.Time
	lastSuccess time.Time
	counters    *core.CounterSet
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// NewClaudeCode returns the tool_config_claude_code collector over w.
func NewClaudeCode(w Writer, cfg Config) *Provider { return newProvider(claudeCodeTool, w, cfg) }

func newProvider(t tool, w Writer, cfg Config) *Provider {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	if cfg.Token == nil {
		cfg.Token = func() string { return "" }
	}
	now := cfg.Clock()
	return &Provider{tool: t, w: w, cfg: cfg, startedAt: now, counters: core.NewCounterSet(now)}
}

// Name implements core.Provider.
func (p *Provider) Name() protocol.Collector { return p.tool.collector }

// Enabled implements core.Toggled: the tool's OTel switch, effective while the receiver is on, or
// for a hooks tool its hooks switch, effective while the hook relay is on.
func (p *Provider) Enabled(b *policy.Bundle) bool {
	if b == nil {
		return false
	}
	if p.tool.hooks {
		return b.Endpoint.Hooks.Enabled && b.Endpoint.Tools[p.tool.key].Hooks
	}
	return b.Endpoint.OTel.Enabled && b.Endpoint.Tools[p.tool.key].OTel
}

// Counters exposes the provider's counter set.
func (p *Provider) Counters() *core.CounterSet { return p.counters }

// desired is what b asks the tool to be configured with.
func (p *Provider) desired(b *policy.Bundle) Desired {
	if p.tool.hooks {
		return Desired{}
	}
	var q core.ScopeQuery
	if p.cfg.Scope != nil {
		q = p.cfg.Scope()
	}
	q.ToolFingerprint = p.tool.fingerprint
	mode := core.Resolve(b, q).Mode
	return Desired{
		HTTPListen: b.Endpoint.OTel.HTTPListen,
		Token:      p.cfg.Token(),
		LogPrompts: core.ModeRank(mode) >= core.ModeRank(protocol.ModeM1),
	}
}

// Start applies the configuration the last bundle asked for. A failed write leaves the provider
// running and degraded; the next bundle tries again.
func (p *Provider) Start(context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil
	}
	p.running = true
	p.attempted = false
	p.startedAt = p.cfg.Clock()
	want, have := p.want, p.haveWant
	p.mu.Unlock()
	if p.tool.supported && have {
		_ = p.apply(want)
	}
	return nil
}

// Stop removes the agent's keys and restores what they replaced. It runs also when the provider
// never started, so keys a crashed run left behind are removed.
func (p *Provider) Stop(context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	p.mu.Lock()
	p.running = false
	p.attempted = false
	p.mu.Unlock()
	if !p.tool.supported {
		return nil
	}
	if err := p.w.Remove(); err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("toolconfig: %s: could not remove the agent's settings from %s: %v", p.tool.key, p.w.Path(), err)
		return err
	}
	return nil
}

// ApplyPolicy records what b asks for and, while running and switched on, applies it when it
// differs from what was applied, or when the last apply did not complete.
func (p *Provider) ApplyPolicy(b policy.Bundle) error {
	if !p.Enabled(&b) {
		return nil
	}
	d := p.desired(&b)
	p.life.Lock()
	defer p.life.Unlock()
	p.mu.Lock()
	p.want, p.haveWant = d, true
	running := p.running
	done := p.attempted && p.applied == d && p.installed && p.writeErr == nil
	p.mu.Unlock()
	if !running || !p.tool.supported || done {
		return nil
	}
	return p.apply(d)
}

// apply writes d when the tool is installed and records the outcome.
func (p *Provider) apply(d Desired) error {
	installed := p.w.Installed()
	var err error
	if installed {
		err = p.w.Apply(d)
	}
	now := p.cfg.Clock()
	p.mu.Lock()
	p.attempted = true
	p.applied = d
	p.installed = installed
	p.writeErr = err
	if installed && err == nil {
		p.lastSuccess = now
	}
	p.mu.Unlock()
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("toolconfig: %s: could not write %s: %v", p.tool.key, p.w.Path(), err)
	}
	return err
}

// Health implements core.Provider: healthy only while the file on disk holds the agent's keys.
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	running, attempted, installed, writeErr := p.running, p.attempted, p.installed, p.writeErr
	applied, since, last := p.applied, p.startedAt, p.lastSuccess
	p.mu.Unlock()
	switch {
	case !p.tool.supported:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailToolVersionUnsupported, since, last)
	case !running || !attempted:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, since, last)
	case !installed:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailToolNotInstalled, since, last)
	case writeErr != nil:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailConfigWriteFailed, since, last)
	}
	if ok, err := p.w.Holds(applied); err != nil || !ok {
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailConfigWriteFailed, since, last)
	}
	return p.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, since, last)
}
