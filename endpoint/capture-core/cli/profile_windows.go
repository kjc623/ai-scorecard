//go:build windows

package cli

import (
	"context"
	"path/filepath"

	"github.com/shadow-ai-capture/device/protocol"
)

// defaultManagedDir is the machine-scope directory for the shim's files.
func defaultManagedDir() string { return `C:\ProgramData\shadow-ai-capture` }

// defaultProfilePath is the batch profile inside ManagedDir; the machine environment is
// what Windows shells actually inherit, set via `setx /M` below (§14.1).
func defaultProfilePath(dir string) string { return filepath.Join(dir, "shim.cmd") }

// renderProfile is a batch profile on Windows.
func renderProfile(vars []envVar) string { return renderCmd(vars) }

// installPlatform sets each variable in the machine environment via `setx /M`. A failure
// here is degraded, not fatal: the managed files are written, and the missing machine
// environment is a coverage gap, never a broken client.
func (p *Provider) installPlatform(ctx context.Context, rp resolvedPaths, vars []envVar) error {
	_ = rp
	if p.cfg.Runner == nil {
		p.logf("cli: no runner configured; machine environment not set")
		return nil
	}
	for _, v := range vars {
		if _, err := p.cfg.Runner.Run(ctx, "setx", "/M", v.Name, v.Value); err != nil {
			p.counters.Add(protocol.CounterErrors)
			p.logf("cli: setx %s failed: %v", v.Name, err)
			continue
		}
		p.step("setx:" + v.Name)
	}
	return nil
}

// uninstallPlatform removes the machine-environment entries the provider set. `REG DELETE`
// on a missing value is a clean no-op, and removal is best-effort — the shutdown column
// must not deadlock on the OS environment.
func (p *Provider) uninstallPlatform(ctx context.Context, rp resolvedPaths) {
	if p.cfg.Runner == nil {
		return
	}
	for _, name := range windowsEnvNames {
		_, _ = p.cfg.Runner.Run(ctx, "reg", "delete",
			`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`,
			"/v", name, "/f")
	}
	p.step("reg:delete-env")
}

// windowsEnvNames is every variable the shim can set on Windows.
var windowsEnvNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	"NODE_OPTIONS",
}
