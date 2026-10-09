package toolconfig

import (
	"context"
	"os"
	"strconv"
	"strings"
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
	// otel is whether the tool exports OpenTelemetry the agent points at its receiver. A tool
	// without has hooks only, and its OTel switch does nothing.
	otel bool
	// managedOnly is whether the tool has a setting that lets only managed hooks run.
	managedOnly bool
	// otelOnly is whether the agent declares no hooks for the tool, so its hooks switch does nothing.
	otelOnly bool
	// cliFingerprint is the catalog fingerprint of the tool's CLI when that is an app of its own,
	// whose prompt logging follows its own collection mode.
	cliFingerprint string
	// versionApp is the catalog app whose installed version is checked against minVersion, the
	// first release that honours every setting the agent writes for the tool. Empty checks none.
	versionApp, minVersion string
}

// recentEvents is how long a tool that runs with the agent's configuration in place may send no
// event before its row is degraded with no_recent_events.
const recentEvents = 24 * time.Hour

// overridable is a Writer whose managed file a user's own configuration can override.
type overridable interface {
	// Overridden reports whether a user's configuration overrides the agent's keys for d.
	Overridden(d Desired) bool
}

// Config is a provider's seams.
type Config struct {
	// Token returns the OTLP receiver's bearer token.
	Token func() string
	// Scope names the device and the user a tool's collection mode is resolved for. nil resolves
	// with neither, so the tenant default applies on those axes.
	Scope func() core.ScopeQuery
	// Executable returns the running capture-core's path, which the tool's hooks run. nil is
	// os.Executable.
	Executable func() (string, error)
	// InstalledVersion returns the version of a catalog app the inventory's last scan found, ok
	// false when it found none. nil checks no version.
	InstalledVersion func(appKey string) (version string, ok bool)
	// LastEvent returns when the tool (its endpoint.tools key) last sent the agent an OTel record
	// or a hook, zero when it has not since the service started. LastRunning returns when a
	// process of a catalog app was last seen running: now while one runs, zero when none was
	// seen. Without both, no_recent_events is not reported.
	LastEvent   func(tool string) time.Time
	LastRunning func(appKey string) time.Time
	Log         core.Logger
	Clock       func() time.Time
	// Watcher, when set, watches the tool's managed configuration while the provider runs and has
	// it applied again when something else changes it.
	Watcher *Watcher
}

// Provider is one tool's tool_config_<tool> collector: while the bundle switches the tool's OTel
// export or its hooks on, the tool's managed configuration carries the agent's keys for what is on.
// Start applies them, Stop removes them, and a bundle that changes what is on, the collection mode,
// the receiver's address, the token or managed-only hooks applies them again.
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
	unsupported bool // the installed version is older than the tool's minimum, so nothing was written
	writeErr    error
	startedAt   time.Time
	lastSuccess time.Time
	// inPlaceSince is when the configuration was last written after a run of failed, skipped or
	// no applies, or after a change made outside the agent; zero while it is not in place.
	inPlaceSince time.Time
	counters     *core.CounterSet
	// tampered: a comparison found the applied configuration changed by something other than the
	// agent. It holds until a health report has carried it and a later comparison is clean.
	tampered       bool
	tamperReported bool
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
	if cfg.Executable == nil {
		cfg.Executable = os.Executable
	}
	now := cfg.Clock()
	return &Provider{tool: t, w: w, cfg: cfg, startedAt: now, counters: core.NewCounterSet(now)}
}

// Name implements core.Provider.
func (p *Provider) Name() protocol.Collector { return p.tool.collector }

// Enabled implements core.Toggled: either of the tool's OTel and hooks switches, each effective
// while its collector is on.
func (p *Provider) Enabled(b *policy.Bundle) bool {
	return p.otelOn(b) || p.hooksOn(b)
}

// hooksOn is the tool's hooks switch, for a tool the agent declares hooks for.
func (p *Provider) hooksOn(b *policy.Bundle) bool {
	return !p.tool.otelOnly && hooksOn(b, p.tool.key)
}

// otelOn is the tool's OTel switch, for a tool with an OTel export.
func (p *Provider) otelOn(b *policy.Bundle) bool {
	return p.tool.otel && otelOn(b, p.tool.key)
}

func otelOn(b *policy.Bundle, tool string) bool {
	return b != nil && b.Endpoint.OTel.Enabled && b.Endpoint.Tools[tool].OTel
}

func hooksOn(b *policy.Bundle, tool string) bool {
	return b != nil && b.Endpoint.Hooks.Enabled && b.Endpoint.Tools[tool].Hooks
}

// Counters exposes the provider's counter set.
func (p *Provider) Counters() *core.CounterSet { return p.counters }

// desired is what b asks the tool to be configured with. Managed-only hooks apply only while the
// agent's own hooks are declared, for a tool that has the setting. An executable path that cannot be read leaves HookCommand empty,
// which the writer refuses.
func (p *Provider) desired(b *policy.Bundle) Desired {
	var d Desired
	if p.otelOn(b) {
		var q core.ScopeQuery
		if p.cfg.Scope != nil {
			q = p.cfg.Scope()
		}
		logs := func(fingerprint string) bool {
			q.ToolFingerprint = fingerprint
			return core.ModeRank(core.Resolve(b, q).Mode) >= core.ModeRank(protocol.ModeM1)
		}
		d.OTel = true
		d.HTTPListen = b.Endpoint.OTel.HTTPListen
		d.Token = p.cfg.Token()
		d.LogPrompts = logs(p.tool.fingerprint)
		if p.tool.cliFingerprint != "" {
			d.LogCLIPrompts = logs(p.tool.cliFingerprint)
		}
	}
	if p.hooksOn(b) {
		d.Hooks = true
		d.ManagedOnly = p.tool.managedOnly && b.Endpoint.Hooks.ManagedOnly
		if exe, err := p.cfg.Executable(); err == nil {
			d.HookCommand = exe
		}
	}
	return d
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
	p.inPlaceSince = time.Time{}
	p.startedAt = p.cfg.Clock()
	want, have := p.want, p.haveWant
	p.mu.Unlock()
	if p.tool.supported && have {
		_ = p.apply(want)
	}
	if p.tool.supported && p.cfg.Watcher != nil {
		p.cfg.Watcher.add(p)
	}
	return nil
}

// Stop removes the agent's keys and restores what they replaced. It runs also when the provider
// never started, so keys a crashed run left behind are removed.
func (p *Provider) Stop(context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	if p.cfg.Watcher != nil {
		p.cfg.Watcher.remove(p)
	}
	p.mu.Lock()
	p.running = false
	p.attempted = false
	p.tampered, p.tamperReported = false, false
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
	done := p.attempted && p.applied == d && p.installed && !p.unsupported && p.writeErr == nil
	p.mu.Unlock()
	if !running || !p.tool.supported || done {
		return nil
	}
	return p.apply(d)
}

// apply writes d when the tool is installed at a version that honours it, and records the outcome.
func (p *Provider) apply(d Desired) error {
	installed := p.w.Installed()
	unsupported := false
	var err error
	if installed {
		var version string
		if version, unsupported = p.belowMinimum(); unsupported {
			p.cfg.Log.Printf("toolconfig: %s: version %s is older than %s, the first that honours the agent's settings; nothing was written",
				p.tool.key, version, p.tool.minVersion)
		} else {
			err = p.w.Apply(d)
		}
	}
	now := p.cfg.Clock()
	p.mu.Lock()
	p.attempted = true
	p.applied = d
	p.installed = installed
	p.unsupported = unsupported
	p.writeErr = err
	if installed && !unsupported && err == nil {
		p.lastSuccess = now
		if p.inPlaceSince.IsZero() {
			p.inPlaceSince = now
		}
	} else {
		p.inPlaceSince = time.Time{}
	}
	p.mu.Unlock()
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("toolconfig: %s: could not write %s: %v", p.tool.key, p.w.Path(), err)
	}
	return err
}

// checkDrift compares the tool's configuration with what was last applied. When the agent's keys
// are no longer in place it applies them again and marks the row tampered; a failed re-apply is
// tried again at the next comparison. A clean comparison after a health report has carried the
// tamper clears it. A failed apply that no outside change caused, or a release too old to be
// written for, waits for the next bundle.
func (p *Provider) checkDrift() {
	p.life.Lock()
	defer p.life.Unlock()
	p.mu.Lock()
	running, attempted, installed, unsupported, writeErr := p.running, p.attempted, p.installed, p.unsupported, p.writeErr
	applied, tampered := p.applied, p.tampered
	p.mu.Unlock()
	if !p.tool.supported || !running || !attempted || !installed || unsupported || (writeErr != nil && !tampered) {
		return
	}
	if writeErr == nil {
		if ok, err := p.w.Holds(applied); err == nil && ok {
			p.mu.Lock()
			if p.tamperReported {
				p.tampered, p.tamperReported = false, false
			}
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		p.tampered, p.tamperReported = true, false
		p.inPlaceSince = time.Time{}
		p.mu.Unlock()
	}
	p.cfg.Log.Printf("toolconfig: %s: %s was changed outside the agent; applying the agent's settings again", p.tool.key, p.w.Path())
	_ = p.apply(applied)
}

// belowMinimum reports whether the inventory's last scan found the tool at a version older than its
// minimum, and that version. An unknown version is not below it.
func (p *Provider) belowMinimum() (string, bool) {
	if p.tool.versionApp == "" || p.cfg.InstalledVersion == nil {
		return "", false
	}
	version, ok := p.cfg.InstalledVersion(p.tool.versionApp)
	return version, ok && olderRelease(version, p.tool.minVersion)
}

// olderRelease reports whether version is a lower release than minimum, comparing the leading
// dotted numbers (2.1.49 is older than 2.1.223, and 2.1.295.0 is not older than 2.1.295). A
// version that does not start with a number is unknown, and not older.
func olderRelease(version, minimum string) bool {
	v, ok := releaseNumbers(version)
	m, _ := releaseNumbers(minimum)
	if !ok {
		return false
	}
	for i := 0; i < max(len(v), len(m)); i++ {
		var a, b int
		if i < len(v) {
			a = v[i]
		}
		if i < len(m) {
			b = m[i]
		}
		if a != b {
			return a < b
		}
	}
	return false
}

// releaseNumbers reads the dotted numbers a version starts with, stopping at the first part that
// is not a number (the 52 of 1.7.52-beta is read).
func releaseNumbers(v string) ([]int, bool) {
	var out []int
	for _, part := range strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".") {
		digits := part[:len(part)-len(strings.TrimLeft(part, "0123456789"))]
		n, err := strconv.Atoi(digits)
		if err != nil {
			break
		}
		out = append(out, n)
		if len(digits) < len(part) {
			break
		}
	}
	return out, len(out) > 0
}

// quiet reports whether a process of the tool was seen running within the last day while the
// agent's configuration had been in place all that day, and the tool sent no event in it.
func (p *Provider) quiet(inPlaceSince time.Time) bool {
	if p.cfg.LastEvent == nil || p.cfg.LastRunning == nil || inPlaceSince.IsZero() {
		return false
	}
	dayAgo := p.cfg.Clock().Add(-recentEvents)
	if inPlaceSince.After(dayAgo) || p.cfg.LastEvent(p.tool.key).After(dayAgo) {
		return false
	}
	for app, tool := range toolByApp {
		if tool == p.tool.key && p.cfg.LastRunning(app).After(dayAgo) {
			return true
		}
	}
	return false
}

// Health implements core.Provider: tampered while a change made outside the agent is being
// reported, else healthy only while the installed version honours the agent's settings, the file
// on disk holds the agent's keys, no user's configuration overrides them, every installed part of
// the tool has a machine-wide configuration the agent can write (else degraded with
// tool_version_unsupported), and the tool, when it ran in the last day, sent an event in it.
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	running, attempted, installed, unsupported, writeErr := p.running, p.attempted, p.installed, p.unsupported, p.writeErr
	applied, since, last, tampered, inPlaceSince := p.applied, p.startedAt, p.lastSuccess, p.tampered, p.inPlaceSince
	p.mu.Unlock()
	switch {
	case !p.tool.supported:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailToolVersionUnsupported, since, last)
	case !running || !attempted:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, since, last)
	case !installed:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailToolNotInstalled, since, last)
	case tampered:
		p.mu.Lock()
		p.tamperReported = p.tampered
		p.mu.Unlock()
		return p.counters.Snapshot(protocol.StateTampered, protocol.DetailConfigTampered, since, last)
	case unsupported:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailToolVersionUnsupported, since, last)
	case writeErr != nil:
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailConfigWriteFailed, since, last)
	}
	if ok, err := p.w.Holds(applied); err != nil || !ok {
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailConfigWriteFailed, since, last)
	}
	if o, ok := p.w.(overridable); ok && o.Overridden(applied) {
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailConfigTampered, since, last)
	}
	if pw, ok := p.w.(partialWriter); ok && pw.Unenforced() {
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailToolVersionUnsupported, since, last)
	}
	if p.quiet(inPlaceSince) {
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailNoRecentEvents, since, last)
	}
	return p.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, since, last)
}
