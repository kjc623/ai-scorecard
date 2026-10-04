//go:build !linux && !darwin && !windows

package cli

import (
	"context"

	"github.com/shadow-ai-capture/device/protocol"
)

// A platform outside the three supported ones gets the Linux shape as a conservative
// fallback: a POSIX profile fragment plus a machine-environment file. Such a platform is
// not a shipped target, so the default locations are best-effort rather than verified.

func defaultManagedDir() string { return "/etc/shadow-ai-capture" }

func defaultProfilePath(string) string { return "/etc/profile.d/shadow-ai-capture.sh" }

func renderProfile(vars []envVar) string { return renderShellProfile(vars) }

func (p *Provider) installPlatform(ctx context.Context, rp resolvedPaths, vars []envVar) error {
	_ = ctx
	_ = vars
	if err := writeFile(rp.envFile, p.envFile, 0o644); err != nil {
		return err
	}
	p.counters.Add(protocol.CounterEmitted)
	p.step("write:env-file")
	return nil
}

func (p *Provider) uninstallPlatform(ctx context.Context, rp resolvedPaths) {}
