package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/cli"
	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/capture-core/toolconfig"
	"github.com/shadow-ai-capture/device/capture-core/winproxy"
	"github.com/shadow-ai-capture/device/protocol"
)

// uninstallCleanupArg runs the uninstall cleanup. The MSI runs it, as SYSTEM, after the service has
// stopped and before the agent's files are removed, with the service's configuration files.
const uninstallCleanupArg = "--uninstall-cleanup"

// uninstallLogName is the cleanup's log, written outside the data folder so it survives that
// folder's removal.
const uninstallLogName = "ShadowAICapture-uninstall.log"

// quicRulePrefix begins the name of every firewall rule the agent adds for QUIC.
const quicRulePrefix = "ShadowAICapture QUIC "

// cleanupBudget bounds the whole cleanup, so an uninstall is never held up for long.
const cleanupBudget = 2 * time.Minute

// cleanupFacilities are the machine changes only the uninstall cleanup makes. Tests replace them.
type cleanupFacilities struct {
	// openUserSettings opens a user's Internet Settings; nil is the platform's.
	openUserSettings func(sid string) (winproxy.Registry, error)
	// deleteDeviceKey deletes the device root's private key from the platform keystore.
	deleteDeviceKey func(caDir string) error
	// removeFirewallRules removes every firewall rule whose name starts with prefix and reports how
	// many it removed; nil where the platform has no firewall the agent writes to.
	removeFirewallRules func(prefix string) (int, error)
	// logPath is the cleanup's log file.
	logPath func() string
}

var cleanupPlatform = cleanupFacilities{
	deleteDeviceKey:     tlsproxy.DeleteDeviceKey,
	removeFirewallRules: firewallRuleRemover(),
	logPath:             uninstallLogPath,
}

// errNotOnPlatform marks a step that has nothing to undo on this platform.
var errNotOnPlatform = errors.New("not on this platform")

// cleanupStep is one thing the agent changed outside its own folders, and how to undo it. run
// returns a short detail for the log, never configuration content.
type cleanupStep struct {
	name string
	run  func(ctx context.Context) (string, error)
}

// runUninstallCleanup returns the machine to how the agent found it, step by step, and writes each
// step's outcome to stdout and the uninstall log. A failed step does not stop the others, and the
// exit code is 0 whatever happens: an uninstall must never be blocked by its cleanup.
func runUninstallCleanup(args []string, stdout io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
	defer cancel()

	out := stdout
	if f, err := os.OpenFile(cleanupPlatform.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
		fmt.Fprintf(stdout, "capture-core: the uninstall log cannot be written: %v\n", err)
	} else {
		defer f.Close()
		out = io.MultiWriter(stdout, f)
	}
	logLine := func(format string, a ...any) {
		fmt.Fprintf(out, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}

	logLine("uninstall cleanup: capture-core %s", version)
	// The state directory is all the cleanup needs; a tenant file that is missing or incomplete
	// does not matter here.
	cfg, _, err := parseFlags(args)
	if strings.TrimSpace(cfg.StateDir) == "" && err != nil {
		logLine("configuration: %v", err)
	}
	failed := 0
	for _, st := range cleanupSteps(cfg.StateDir) {
		detail, err := st.run(ctx)
		switch {
		case errors.Is(err, errNotOnPlatform):
			logLine("%s: skipped, %v", st.name, err)
		case err != nil:
			failed++
			logLine("%s: failed: %v", st.name, err)
		case detail != "":
			logLine("%s: ok, %s", st.name, detail)
		default:
			logLine("%s: ok", st.name)
		}
	}
	logLine("uninstall cleanup: done, %d step(s) failed", failed)
	return 0
}

// cleanupSteps is the cleanup over the state directory at stateDir, in order: the tools'
// configuration, OLLAMA_HOST, the users' PAC, the device root and its key, the QUIC firewall rules
// and the CLI shim's machine environment.
func cleanupSteps(stateDir string) []cleanupStep {
	dir, dirErr := state.Open(stateDir)
	needsState := func(run func(ctx context.Context) (string, error)) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			if dirErr != nil {
				return "", dirErr
			}
			return run(ctx)
		}
	}
	removeTool := func(w func() toolconfig.Writer) func(context.Context) (string, error) {
		return needsState(func(context.Context) (string, error) { return "", w().Remove() })
	}
	logf := func(string, ...any) {}

	return []cleanupStep{
		{string(protocol.CollectorToolConfigClaudeCode), removeTool(func() toolconfig.Writer {
			return toolconfig.NewClaudeCodeWriter(dir, platform.claudeCodeSettings, nil)
		})},
		{string(protocol.CollectorToolConfigCodex), removeTool(func() toolconfig.Writer {
			return toolconfig.NewCodexFiles(
				toolconfig.NewCodexWriter(dir, platform.codexRequirements, nil),
				toolconfig.NewCodexConfigWriter(dir, platform.codexConfig, nil),
			)
		})},
		{string(protocol.CollectorToolConfigCopilot), removeTool(func() toolconfig.Writer {
			return toolconfig.NewCopilotWriter(dir, nil)
		})},
		{string(protocol.CollectorToolConfigCursor), removeTool(func() toolconfig.Writer {
			return toolconfig.NewCursorWriter(dir, platform.cursorHooks, nil)
		})},
		{"ollama_host", needsState(func(context.Context) (string, error) {
			if platform.machineEnv == nil {
				return "", errNotOnPlatform
			}
			return "", toolconfig.NewOllama(dir, platform.machineEnv).Restore()
		})},
		{"desktop_proxy_pac", needsState(func(context.Context) (string, error) {
			n, err := winproxy.RestoreRecorded(dir.Path(winproxy.StateFile), cleanupPlatform.openUserSettings)
			return fmt.Sprintf("%d user(s) restored", n), err
		})},
		{"trust_root", needsState(func(ctx context.Context) (string, error) {
			return "", tlsproxy.RetireDeviceRoot(ctx, dir.Path(state.DeviceCADir), platform.trustStore(logf))
		})},
		{"device_root_key", needsState(func(context.Context) (string, error) {
			return "", cleanupPlatform.deleteDeviceKey(dir.Path(state.DeviceCADir))
		})},
		{"quic_firewall_rules", func(context.Context) (string, error) {
			if cleanupPlatform.removeFirewallRules == nil {
				return "", errNotOnPlatform
			}
			n, err := cleanupPlatform.removeFirewallRules(quicRulePrefix)
			return fmt.Sprintf("%d rule(s) removed", n), err
		}},
		{"cli_shim_environment", func(ctx context.Context) (string, error) {
			shim := cli.New(cli.Config{
				ManagedDir:  platform.shimDir,
				ProfilePath: platform.shimProfile,
				Runner:      platform.shimRunner,
			})
			return "", shim.Stop(ctx)
		}},
	}
}
