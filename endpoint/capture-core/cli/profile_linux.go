//go:build linux

package cli

import (
	"context"
	"path/filepath"

	"github.com/shadow-ai-capture/device/protocol"
)

// defaultManagedDir is the Linux machine-scope directory for the shim's files.
func defaultManagedDir() string { return "/etc/shadow-ai-capture" }

// defaultProfilePath is the /etc/profile.d fragment every login shell sources (§14.3).
func defaultProfilePath(string) string { return "/etc/profile.d/shadow-ai-capture.sh" }

// defaultLauncherPath is the generated launcher that sets the environment and runs Claude Code.
func defaultLauncherPath(dir string) string { return filepath.Join(dir, "claude-sac") }

// renderProfile is the POSIX shell profile on Linux.
func renderProfile(vars []envVar) string { return renderShellProfile(vars) }

// renderLauncher is the POSIX launcher on Linux.
func renderLauncher(vars []envVar) []byte { return []byte(renderPOSIXLauncher(vars)) }

// installPlatform writes the machine-environment file (KEY=VALUE), for systemd units and
// anything else that reads a machine environment rather than a login shell.
func (p *Provider) installPlatform(ctx context.Context, rp resolvedPaths, vars []envVar) error {
	_ = ctx
	if err := writeFile(rp.envFile, p.envFile, 0o644); err != nil {
		return err
	}
	p.counters.Add(protocol.CounterEmitted)
	p.step("write:env-file")
	return nil
}

// uninstallPlatform has nothing to do beyond the managed files (removed by the caller).
func (p *Provider) uninstallPlatform(ctx context.Context, rp resolvedPaths) {}
