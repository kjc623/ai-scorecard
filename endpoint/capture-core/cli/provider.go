// Package cli is `cli.shim` (docs/01-collectors.md §4.5): the CLI trust shim.
//
// It observes nothing directly. Its job is to make modes E (CLI) and G capturable
// through `proxy.tls` by writing the per-machine shell environment every CLI runtime
// inherits: a managed profile that exports the proxy variables and the CA-bundle path,
// the CA bundle itself, and (optionally) a Node bootstrap that routes Node's HTTP
// stack through the proxy. It holds no ports, takes no locks, and fails open by passing
// every command through untouched — a broken shim degrades coverage, never a client.
//
// §4.5's three checks — profile present with the expected content, CA bundle parses and
// carries the root, environment inherited by a child — are the three checks in its
// coverage row, and each has a named detail (protocol.DetailShimProfileMissing,
// DetailShimCABundleUnreadable, DetailShimNotInherited) so a coverage report can group by
// cause rather than parsing prose.
package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// Runner executes an external command. It is the seam the shim uses for the platform
// steps that are not plain file writes — the macOS /etc/zshenv loader line and the
// Windows `setx` machine-environment entries — so a test can exercise those paths with a
// fake rather than touching a real OS. It is nil-safe in the sense that Start degrades
// rather than panics when it is absent, but the managed files are still written.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// Config is the shim's policy data and its seams. Everything that is policy lives in the
// signed bundle (the CA root PEM and the proxy address); the rest is wiring.
type Config struct {
	// ManagedDir is where the shim's own files live. It is required in production; when
	// empty a per-OS default is used (see the platform files).
	ManagedDir string

	// CABundlePath is the file the shim writes the device root CA to. Empty defaults to
	// ManagedDir/ca-bundle.pem.
	CABundlePath string

	// ProfilePath is the managed profile. Empty defaults per-OS: /etc/profile.d on Linux,
	// ManagedDir/shadow-ai-capture.sh on macOS, ManagedDir/shim.cmd on Windows.
	ProfilePath string

	// EnvFile is the KEY=VALUE machine-environment file written on Linux. Empty defaults
	// to ManagedDir/env.
	EnvFile string

	// NodeBootstrapPath is the Node bootstrap script written when NodeRequire is set.
	// Empty defaults to ManagedDir/node-proxy.cjs.
	NodeBootstrapPath string

	// ProxyAddr is the host:port proxy.tls listens on (from the bundle's cli_shim.proxy_addr).
	ProxyAddr string

	// NoProxy is the NO_PROXY list; entries are comma-joined.
	NoProxy []string

	// Runtimes is the subset of {go,node,python} the shim is asked to configure. It is
	// advisory for the coverage row: the managed profile always exports the full variable
	// set, because the variables are cheap and a partial environment is the failure the
	// shim exists to prevent.
	Runtimes []string

	// NodeRequire writes the Node bootstrap and wires it via NODE_OPTIONS=--require.
	NodeRequire bool

	// RootCAPEM is the device root CA certificate in PEM form (from the bundle's
	// interception.root_ca_pem). Empty means "no root to install"; the provider never
	// reports healthy without it.
	RootCAPEM []byte

	// Runner is the external-command seam; see Runner.
	Runner Runner

	// EnvProbe, when set, reads the environment a child process inherited so Health can
	// assert the shim's environment actually reached shells' children. nil skips that
	// check — a provider without a way to read an environment must not fail health for it.
	EnvProbe func(context.Context) (map[string]string, error)

	Log   core.Logger
	Clock func() time.Time
}

// envVar is one NAME=VALUE pair, kept in a deterministic order so the profile content —
// and therefore the health check that compares against it — is stable.
type envVar struct {
	Name  string
	Value string
}

// resolvedPaths is the concrete set of files the shim writes, resolved once from Config.
type resolvedPaths struct {
	managedDir string
	cabundle   string
	profile    string
	envFile    string
	node       string
	shimCmd    string
	launcher   string
}

// Provider is `cli.shim`.
type Provider struct {
	cfg      Config
	paths    resolvedPaths
	vars     []envVar
	profile  []byte
	envFile  []byte
	launcher []byte

	mu          sync.Mutex
	started     bool
	stopped     bool
	killed      bool
	startedAt   time.Time
	lastSuccess time.Time
	counters    *core.CounterSet
	sequence    []string
}

// Name implements core.Provider: the collector name is the route.
func (p *Provider) Name() protocol.Route { return protocol.RouteCLIShim }

// New returns an unstarted provider.
func New(cfg Config) *Provider {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	cfg.Runtimes = normaliseRuntimes(cfg.Runtimes)

	paths := resolvePaths(cfg)
	vars := envVars(cfg, paths)

	now := cfg.Clock()
	return &Provider{
		cfg:       cfg,
		paths:     paths,
		vars:      vars,
		profile:   []byte(renderProfile(vars)),
		envFile:   []byte(renderEnvFile(vars)),
		launcher:  []byte(renderLauncher(vars)),
		startedAt: now,
		counters:  core.NewCounterSet(now),
	}
}

// nopLogger drops the two facts a shim logs (a failed Stop and a failed platform step).
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

func (p *Provider) logf(format string, args ...any) { p.cfg.Log.Printf(format, args...) }

func (p *Provider) step(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence = append(p.sequence, s)
}

// Sequence returns the side effects performed, in order. It exists so a test can assert
// the kill-switch ordering literally instead of trusting the order of statements.
func (p *Provider) Sequence() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sequence...)
}

// Counters exposes the closed counter set for the coverage row.
func (p *Provider) Counters() *core.CounterSet { return p.counters }

// Start writes the managed files and performs the platform install. It is idempotent and
// transactional: a failed Start removes everything it wrote, because a half-written
// profile is worse than none.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return nil
	}
	// A kill switch in the bundle already in force at startup must suppress the shim before it
	// writes anything. The supervisor applies the bundle before providers start, so without this
	// check the switch would be recorded and then ignored while the profile was still installed.
	if p.killed {
		p.started = true
		p.mu.Unlock()
		p.step("start:suppressed_by_kill_switch")
		return nil
	}
	p.mu.Unlock()

	// Node's NODE_OPTIONS parser splits on whitespace and does not support quoting a value, so a
	// bootstrap path with a space would be silently mangled into a failed require (and Node may
	// refuse to start). Refuse the configuration with a named cause instead of writing a profile
	// that cannot work.
	if p.cfg.NodeRequire && strings.ContainsAny(p.paths.node, " \t") {
		p.counters.Add(protocol.CounterErrors)
		return fmt.Errorf("cli: NODE_OPTIONS cannot express the Node bootstrap path %q because it contains whitespace; choose a --shim-dir without spaces or disable node_require", p.paths.node)
	}

	ok := false
	defer func() {
		if !ok {
			// Transactional Start (§4.1): roll back every file on the failure path.
			for _, f := range p.managedFiles() {
				_ = os.Remove(f)
			}
		}
	}()

	// 1. The CA bundle, mode 0600 (the only file that needs to be readable by the runtime
	// but not world-writable; it holds public certificates, but the mode is the contract).
	if len(p.cfg.RootCAPEM) > 0 {
		if err := writeFile(p.paths.cabundle, p.cfg.RootCAPEM, 0o600); err != nil {
			p.counters.Add(protocol.CounterErrors)
			p.logf("cli: writing CA bundle %s: %v", p.paths.cabundle, err)
			return fmt.Errorf("cli: writing CA bundle: %w", err)
		}
		p.counters.Add(protocol.CounterEmitted)
		p.step("write:ca-bundle")
	}

	// 2. The managed profile.
	if err := writeFile(p.paths.profile, p.profile, 0o644); err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.logf("cli: writing profile %s: %v", p.paths.profile, err)
		return fmt.Errorf("cli: writing profile: %w", err)
	}
	p.counters.Add(protocol.CounterEmitted)
	p.step("write:profile")

	// 2b. The launcher. It sets the environment and execs `claude`, so a coding agent launched
	// from Explorer/an IDE or from a shell that predates the machine-env update still gets the
	// proxy and CA. This is what makes capture not depend on environment-broadcast timing.
	if err := writeFile(p.paths.launcher, p.launcher, 0o755); err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.logf("cli: writing launcher %s: %v", p.paths.launcher, err)
		return fmt.Errorf("cli: writing launcher: %w", err)
	}
	p.counters.Add(protocol.CounterEmitted)
	p.step("write:launcher")

	// 3. The Node bootstrap, only when asked.
	if p.cfg.NodeRequire {
		if err := writeFile(p.paths.node, []byte(nodeProxyScript), 0o644); err != nil {
			p.counters.Add(protocol.CounterErrors)
			p.logf("cli: writing Node bootstrap %s: %v", p.paths.node, err)
			return fmt.Errorf("cli: writing Node bootstrap: %w", err)
		}
		p.counters.Add(protocol.CounterEmitted)
		p.step("write:node-proxy")
	}

	// 4. The platform-specific install (Linux env file, macOS loader line, Windows setx).
	if err := p.installPlatform(ctx, p.paths, p.vars); err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.logf("cli: platform install: %v", err)
		return fmt.Errorf("cli: platform install: %w", err)
	}

	p.mu.Lock()
	p.started = true
	p.stopped = false
	p.lastSuccess = time.Time{}
	p.mu.Unlock()

	ok = true
	return nil
}

// Stop removes the profile, the loader line and every managed file it created, and leaves
// the trust store alone. It is idempotent and never fails visibly: a removal error is
// logged and counted, and interference is evidence, not a reason to block shutdown.
func (p *Provider) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	p.removeFiles(ctx)
	return nil
}

// removeFiles deletes the managed files and runs the platform uninstall. It is shared by
// Stop and the kill switch, so "stop" and "kill" cannot drift apart on what they remove.
func (p *Provider) removeFiles(ctx context.Context) {
	for _, f := range p.managedFiles() {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			p.counters.Add(protocol.CounterErrors)
			p.logf("cli: removing %s: %v", f, err)
		}
	}
	p.uninstallPlatform(ctx, p.paths)
	p.step("remove:files")
}

// managedFiles is every file the shim could have written, for removal and rollback.
func (p *Provider) managedFiles() []string {
	return []string{
		p.paths.profile,
		p.paths.cabundle,
		p.paths.envFile,
		p.paths.node,
		p.paths.shimCmd,
		p.paths.launcher,
	}
}

// ApplyPolicy implements core.Provider: a diff, never a restart. The one policy change
// that affects the shim is the kill switch: mode=disable for either cli.shim or
// proxy.tls removes the shim's files and reports absent detail=killed, because the proxy
// it feeds has stopped enforcing.
func (p *Provider) ApplyPolicy(b policy.Bundle) error {
	if !p.killSwitchActive(b) {
		return nil
	}
	p.mu.Lock()
	already := p.killed
	p.killed = true
	p.mu.Unlock()
	if already {
		return nil
	}
	p.step("kill_switch:remove_files")
	p.removeFiles(context.Background())
	p.step("health:absent:" + string(protocol.DetailKilled))
	return nil
}

// killSwitchActive reports whether b carries a kill switch for cli.shim or proxy.tls that is in
// force now. A future-dated switch does not fire early.
func (p *Provider) killSwitchActive(b policy.Bundle) bool {
	for _, r := range []protocol.Route{protocol.RouteCLIShim, protocol.RouteProxyTLS} {
		if ks, ok := b.KillSwitchFor(r); ok && ks.Mode == policy.KillDisable {
			if ks.EffectiveAt.IsZero() || !ks.EffectiveAt.After(p.cfg.Clock()) {
				return true
			}
		}
	}
	return false
}

// Health implements core.Provider. Healthy requires all three of §4.5's checks to pass:
// the profile exists with the expected content, the CA bundle parses and carries the
// root, and — only when an EnvProbe is configured — a child inherited the environment.
// It is never healthy without a root CA; a nil EnvProbe skips the inherited check.
func (p *Provider) Health() core.Health {
	p.mu.Lock()
	started, stopped, killed := p.started, p.stopped, p.killed
	lastSuccess := p.lastSuccess
	envProbe := p.cfg.EnvProbe
	p.mu.Unlock()

	switch {
	case !started || stopped:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, p.startedAt, lastSuccess)
	case killed:
		return p.counters.Snapshot(protocol.StateAbsent, protocol.DetailKilled, p.startedAt, lastSuccess)
	}

	// One cycle: the observed counter counts the check pass, not the file writes.
	p.counters.Add(protocol.CounterObserved)

	// Check 1: profile present with the expected content.
	if len(p.profile) == 0 || !fileMatches(p.paths.profile, p.profile) {
		p.counters.Add(protocol.CounterDropped)
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailShimProfileMissing, p.startedAt, lastSuccess)
	}
	// Check 2: CA bundle parses and contains the root.
	if !p.bundleOK() {
		p.counters.Add(protocol.CounterDropped)
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailShimCABundleUnreadable, p.startedAt, lastSuccess)
	}
	// Check 3: environment inherited (skipped when no probe is configured).
	if envProbe != nil && !inheritedOK(envProbe, p.cfg.ProxyAddr, p.paths.cabundle) {
		p.counters.Add(protocol.CounterDropped)
		return p.counters.Snapshot(protocol.StateDegraded, protocol.DetailShimNotInherited, p.startedAt, lastSuccess)
	}

	now := p.cfg.Clock()
	p.mu.Lock()
	p.lastSuccess = now
	p.mu.Unlock()
	return p.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, p.startedAt, now)
}

// bundleOK reports whether the on-disk CA bundle parses and contains the device root. A
// missing root is a capability gap, not a parse success: the provider must not report
// healthy on the strength of an empty bundle.
func (p *Provider) bundleOK() bool {
	if len(p.cfg.RootCAPEM) == 0 {
		return false
	}
	data, err := os.ReadFile(p.paths.cabundle)
	if err != nil {
		return false
	}
	return bundleContainsRoot(data, p.cfg.RootCAPEM)
}

// inheritedOK asserts a child process inherited the shim's environment: one of the
// CA-bundle variables must point at the shim's bundle, and the proxy variable must match
// when a proxy is configured.
func inheritedOK(probe func(context.Context) (map[string]string, error), proxyAddr, cabundle string) bool {
	env, err := probe(context.Background())
	if err != nil {
		return false
	}
	caOK := false
	for _, k := range caBundleVars {
		if v, ok := env[k]; ok && v == cabundle {
			caOK = true
			break
		}
	}
	if !caOK {
		return false
	}
	if proxyAddr != "" {
		if v, ok := env["HTTPS_PROXY"]; !ok || v != "http://"+proxyAddr {
			return false
		}
	}
	return true
}

// caBundleVars is the set of variables a runtime reads for the CA bundle. Any one of them
// pointing at the shim's bundle proves the environment reached the child.
var caBundleVars = []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}

func resolvePaths(cfg Config) resolvedPaths {
	dir := cfg.ManagedDir
	if dir == "" {
		dir = defaultManagedDir()
	}
	rp := resolvedPaths{managedDir: dir}
	rp.cabundle = cfg.CABundlePath
	if rp.cabundle == "" {
		rp.cabundle = filepath.Join(dir, "ca-bundle.pem")
	}
	rp.profile = cfg.ProfilePath
	if rp.profile == "" {
		rp.profile = defaultProfilePath(dir)
	}
	rp.envFile = cfg.EnvFile
	if rp.envFile == "" {
		rp.envFile = filepath.Join(dir, "env")
	}
	rp.node = cfg.NodeBootstrapPath
	if rp.node == "" {
		rp.node = filepath.Join(dir, "node-proxy.cjs")
	}
	rp.shimCmd = filepath.Join(dir, "shim.cmd")
	rp.launcher = defaultLauncherPath(dir)
	return rp
}

// envVars is the shim's environment, in deterministic order. The proxy variables point at
// the proxy, the CA variables point at the bundle, and NODE_OPTIONS loads the bootstrap
// when NodeRequire is set.
//
// Both cases of the proxy variables are set because toolchains disagree on case, and
// NODE_USE_ENV_PROXY is set so that on Node 24+ the built-in fetch (which the Anthropic SDK a
// coding agent uses) honours them; on older Node, Claude Code reads HTTPS_PROXY itself and the
// https.Agent bootstrap covers the http stack. NO_PROXY always includes loopback so the proxy is
// never asked to reach itself.
func envVars(cfg Config, rp resolvedPaths) []envVar {
	vars := []envVar{}
	if cfg.ProxyAddr != "" {
		proxy := "http://" + cfg.ProxyAddr
		vars = append(vars,
			envVar{Name: "HTTP_PROXY", Value: proxy},
			envVar{Name: "HTTPS_PROXY", Value: proxy},
			envVar{Name: "http_proxy", Value: proxy},
			envVar{Name: "https_proxy", Value: proxy},
		)
	}
	noProxyList := []string{"localhost", "127.0.0.1", "::1"}
	seen := map[string]bool{}
	for _, p := range noProxyList {
		seen[p] = true
	}
	for _, p := range cfg.NoProxy {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			noProxyList = append(noProxyList, p)
		}
	}
	noProxy := strings.Join(noProxyList, ",")
	vars = append(vars,
		envVar{Name: "NO_PROXY", Value: noProxy},
		envVar{Name: "no_proxy", Value: noProxy},
		envVar{Name: "NODE_EXTRA_CA_CERTS", Value: rp.cabundle},
		envVar{Name: "SSL_CERT_FILE", Value: rp.cabundle},
		envVar{Name: "REQUESTS_CA_BUNDLE", Value: rp.cabundle},
		envVar{Name: "CURL_CA_BUNDLE", Value: rp.cabundle},
		envVar{Name: "NODE_USE_ENV_PROXY", Value: "1"},
	)
	if cfg.NodeRequire {
		vars = append(vars, envVar{Name: "NODE_OPTIONS", Value: "--require " + rp.node})
	}
	return vars
}

// renderShellProfile renders the POSIX profile: one `export NAME=value` line per variable.
func renderShellProfile(vars []envVar) string {
	var b strings.Builder
	b.WriteString("# Managed by Shadow AI Capture. Do not edit.\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "export %s=%s\n", v.Name, shellQuote(v.Value))
	}
	return b.String()
}

// renderEnvFile renders the Linux machine-environment file: one KEY=VALUE per line.
func renderEnvFile(vars []envVar) string {
	var b strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&b, "%s=%s\n", v.Name, v.Value)
	}
	return b.String()
}

// renderCmd renders the Windows batch profile: one `set "NAME=value"` per line.
func renderCmd(vars []envVar) string {
	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("REM Managed by Shadow AI Capture. Do not edit.\r\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "set \"%s=%s\"\r\n", v.Name, v.Value)
	}
	return b.String()
}

// renderPOSIXLauncher renders a launcher that sets the shim environment and execs `claude`.
// It exists so capture does not depend on the machine environment having been read by the
// shell that starts the agent (a new login, a service, or an IDE launch).
func renderPOSIXLauncher(vars []envVar) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Generated by Shadow AI Capture cli.shim: run Claude Code through proxy.tls.\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "export %s=%s\n", v.Name, shellQuote(v.Value))
	}
	b.WriteString("exec claude \"$@\"\n")
	return b.String()
}

// renderCmdLauncher renders the Windows launcher (`claude-sac.cmd`).
func renderCmdLauncher(vars []envVar) string {
	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("REM Generated by Shadow AI Capture cli.shim: run Claude Code through proxy.tls.\r\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "set \"%s=%s\"\r\n", v.Name, v.Value)
	}
	b.WriteString("claude %*\r\n")
	return b.String()
}

// shellQuote quotes a value for a POSIX shell: single quotes with embedded quotes escaped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeFile creates parent directories and writes the file with the given mode.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

// fileMatches reports whether the file at path has exactly the expected content.
func fileMatches(path string, expected []byte) bool {
	got, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Equal(got, expected)
}

// bundleContainsRoot reports whether the PEM bundle parses and contains the root PEM.
func bundleContainsRoot(bundle, rootPEM []byte) bool {
	bundleCerts, err := parsePEMCerts(bundle)
	if err != nil {
		return false
	}
	rootCerts, err := parsePEMCerts(rootPEM)
	if err != nil {
		return false
	}
	for _, rc := range rootCerts {
		found := false
		for _, c := range bundleCerts {
			if c.Equal(rc) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// parsePEMCerts parses every CERTIFICATE block in a PEM document.
func parsePEMCerts(p []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := p
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("cli: no certificates in PEM")
	}
	return certs, nil
}

// normaliseRuntimes filters the runtime subset to the closed set {go,node,python} and
// defaults an empty slice to all three.
func normaliseRuntimes(in []string) []string {
	if len(in) == 0 {
		return []string{"go", "node", "python"}
	}
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	for _, r := range in {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "go", "node", "python":
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	if len(out) == 0 {
		return []string{"go", "node", "python"}
	}
	return out
}

// Ensure the provider satisfies the contract it claims to.
var _ core.Provider = (*Provider)(nil)
