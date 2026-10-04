//go:build darwin

package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shadow-ai-capture/device/protocol"
)

// macOS uses a daemon-delivered profile script plus a loader line in /etc/zshenv, so the
// environment reaches every shell without relying on a per-user dotfile (A19: exact
// locations are deployment details; the requirement is a per-machine configuration).

const (
	zshenvPath  = "/etc/zshenv"
	zshenvBegin = "# >>> sac-shim-begin"
	zshenvEnd   = "# <<< sac-shim-end"
)

// defaultManagedDir is the machine-scope directory for the shim's files. It deliberately has no
// spaces: the Node bootstrap is wired through NODE_OPTIONS, whose parser splits on whitespace and
// does not support quoting a path (so /Library/Application Support would break the require hook).
func defaultManagedDir() string { return "/var/db/shadow-ai-capture" }

// defaultProfilePath is the managed profile inside ManagedDir, sourced from /etc/zshenv.
func defaultProfilePath(dir string) string {
	return dir + "/shadow-ai-capture.sh"
}

// defaultLauncherPath is the generated launcher that sets the environment and runs Claude Code.
func defaultLauncherPath(dir string) string { return filepath.Join(dir, "claude-sac") }

// renderProfile is the POSIX shell profile on macOS.
func renderProfile(vars []envVar) string { return renderShellProfile(vars) }

// renderLauncher is the POSIX launcher on macOS.
func renderLauncher(vars []envVar) []byte { return []byte(renderPOSIXLauncher(vars)) }

// installPlatform appends a marker-guarded `source` line to /etc/zshenv via the Runner.
// Failure is degraded, not fatal: the managed files are already written, and a missing
// loader line is a coverage gap reported by Health, never a broken client.
func (p *Provider) installPlatform(ctx context.Context, rp resolvedPaths, vars []envVar) error {
	_ = vars
	if p.cfg.Runner == nil {
		p.logf("cli: no runner configured; /etc/zshenv loader line not installed")
		return nil
	}
	line := fmt.Sprintf("[ -f %s ] && . %s", shellQuote(rp.profile), shellQuote(rp.profile))
	block := zshenvBegin + "\n" + line + "\n" + zshenvEnd + "\n"
	script := "cat >> " + shellQuote(zshenvPath) + " <<'SAC_EOF'\n" + block + "SAC_EOF\n"
	if _, err := p.cfg.Runner.Run(ctx, "/bin/sh", "-c", script); err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.logf("cli: installing /etc/zshenv loader line: %v", err)
		return nil
	}
	p.step("zshenv:source-line")
	return nil
}

// uninstallPlatform removes exactly the marker-guarded block it added.
func (p *Provider) uninstallPlatform(ctx context.Context, rp resolvedPaths) {
	if p.cfg.Runner == nil {
		return
	}
	script := fmt.Sprintf("sed -i '' '/%s/,/%s/d' %s", zshenvBegin, zshenvEnd, shellQuote(zshenvPath))
	if _, err := p.cfg.Runner.Run(ctx, "/bin/sh", "-c", script); err != nil {
		p.logf("cli: removing /etc/zshenv loader line: %v", err)
	}
	p.step("zshenv:remove-line")
}
