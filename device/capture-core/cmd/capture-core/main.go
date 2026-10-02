// Command capture-core is the endpoint agent (docs/01-collectors.md §3.1): one static binary per
// platform that hosts the providers, the policy engine, the spool and the native-messaging host.
//
// It wires components; it does not contain policy. §4.1's provider contract, §11.2's mode gate,
// §13.2's bundle verification and §3.5's ordering all live in the packages it imports, and the
// binary's own job is to resolve configuration, build the graph, drive the supervisor and expose
// the two edges the rest of the system speaks: the classifier host's local socket and the browser's
// native-messaging channel.
//
// Subcommands (all flags on the root, because there is exactly one binary):
//
//	run                  the service (default): load policy, open the spool, start providers in §3.5 order
//	--print-config       resolve the bundle and print what the agent resolved, then exit
//	--native-host        the native-messaging host: Chromium's 4-byte length-prefixed JSON on stdin/stdout
//	--native-frames DIR  feed the golden frames in DIR through the real framing and dispatch, then exit
//	--selftest           the end-to-end evidence run: service + frames + health + shutdown, exit non-zero on failure
//	--version            version and build information
//
// Deployment is described in cmd/capture-core/README.md. Nothing here installs a service, writes a
// system proxy or touches the OS trust store: those are platform facilities behind the interfaces
// in proxy/tlsproxy, and they are NOT VERIFIED on the host this was built on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is the agent version. It is reported in the health channel and in --version, and it is
// deliberately not tied to any policy version: the bundle carries its own version (§11.3).
const version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "capture-core: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, mode, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}
	if mode.showVersion {
		fmt.Printf("capture-core %s\n", version)
		fmt.Printf("protocol framing version %d; native messaging framing: 4-byte little-endian length prefix (Chromium)\n", protocolVersion())
		return nil
	}

	logger := newLogger(cfg, mode)

	switch {
	case mode.printConfig:
		return printConfig(cfg, slogLogger{logger})
	case mode.selftest:
		return runSelftest(cfg, logger)
	case mode.nativeFrames != "":
		return runNativeFrames(cfg, logger, mode.nativeFrames)
	case mode.nativeHost:
		// The native-messaging host is its own process in Chromium's model, but it shares the
		// binary and the same service graph: the browser's messages go through the same pipeline
		// as a proxy's observations.
		return runNativeHost(cfg, logger)
	default:
		return runService(cfg, logger)
	}
}

// runMode is what the flags asked for. Exactly one of these is true; parseFlags enforces it.
type runMode struct {
	showVersion  bool
	printConfig  bool
	selftest     bool
	nativeHost   bool
	nativeFrames string
}

func parseFlags(args []string) (Config, runMode, error) {
	cfg := defaultConfig()
	var mode runMode

	fs := flag.NewFlagSet("capture-core", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: capture-core [run|--native-host|--selftest|--native-frames DIR|--print-config|--version] [flags]\n\n")
		fs.PrintDefaults()
	}

	// Identity and storage.
	fs.StringVar(&cfg.SpoolDir, "spool-dir", cfg.SpoolDir, "spool directory (the agent's only durable store; §12)")
	fs.StringVar(&cfg.SpoolKey, "spool-key", cfg.SpoolKey, "path to the spool key file; must be OUTSIDE the spool directory (a key beside its ciphertext is not encryption at rest)")
	fs.StringVar(&cfg.SpoolBoundsProfile, "spool-bounds", cfg.SpoolBoundsProfile, "spool bound profile: default | dev")
	fs.StringVar(&cfg.TenantID, "tenant-id", cfg.TenantID, "tenant id stamped on every envelope (from enrolment, §13.1)")
	fs.StringVar(&cfg.DeviceID, "device-id", cfg.DeviceID, "device id stamped on every envelope")
	fs.StringVar(&cfg.UserRef, "user-ref", cfg.UserRef, "pseudonymous subject reference (never a name or e-mail)")
	fs.StringVar(&cfg.Population, "population", cfg.Population, "user population for scope resolution (may be empty)")
	fs.StringVar(&cfg.Retention, "retention", cfg.Retention, "device-side retention for spooled observations (e.g. 720h)")

	// Policy.
	fs.StringVar(&cfg.BundlePath, "bundle", cfg.BundlePath, "path to the signed policy bundle (omit for M0: metadata only, §13.3 rule 5)")
	fs.StringVar(&cfg.PolicyKey, "policy-key", cfg.PolicyKey, "hex-encoded Ed25519 public key the bundle must verify under")
	fs.StringVar(&cfg.PolicyKeyID, "policy-key-id", cfg.PolicyKeyID, "key id the bundle must name")

	// Classifier host (§3.4).
	fs.StringVar(&cfg.ClassifierAddress, "classifier-address", cfg.ClassifierAddress, "classifier host address as transport:path, e.g. unix:/run/sac/classifier.sock or pipe:\\\\.\\pipe\\sac-classifier; empty means rules-only")
	fs.DurationVar(&cfg.ClassifierBudget, "classifier-budget", cfg.ClassifierBudget, "budget for one classification")

	// Providers.
	fs.BoolVar(&cfg.EnableTLS, "proxy-tls", cfg.EnableTLS, "run proxy.tls (the egress interceptor)")
	fs.StringVar(&cfg.TLSListen, "proxy-tls-listen", cfg.TLSListen, "proxy.tls listen address; a test or a local run uses 127.0.0.1:0")
	fs.StringVar(&cfg.TLSCanary, "proxy-tls-canary", cfg.TLSCanary, "canary host:port for the §5.2 end-to-end probe; empty reports degraded, never healthy")
	fs.BoolVar(&cfg.EnableLoopback, "proxy-loopback", cfg.EnableLoopback, "run proxy.loopback using the bundle's port map")
	fs.BoolVar(&cfg.EnableProcDetect, "proc-detect", cfg.EnableProcDetect, "run proc.detect (needs an enumerator; without one the route is reported as a named gap)")
	fs.DurationVar(&cfg.DrainDeadline, "drain-deadline", cfg.DrainDeadline, "bounded spool drain at shutdown (§3.5 step 3)")

	// Health channel.
	fs.StringVar(&cfg.HealthFile, "health-file", cfg.HealthFile, "append the health channel to this file as JSON lines (empty disables the writer)")
	fs.DurationVar(&cfg.HealthInterval, "health-interval", cfg.HealthInterval, "health channel interval")

	// Attachment transport (native messaging, §3.4).
	fs.Int64Var(&cfg.AttachmentCap, "attachment-cap", cfg.AttachmentCap, "policy cap for one attachment manifest, checked BEFORE any byte moves")

	// Modes.
	fs.BoolVar(&mode.showVersion, "version", false, "print version and exit")
	fs.BoolVar(&mode.printConfig, "print-config", false, "resolve the bundle and print the effective configuration, then exit")
	fs.BoolVar(&mode.selftest, "selftest", false, "run the end-to-end self test (service, golden frames, health, shutdown) and exit non-zero on failure")
	fs.BoolVar(&mode.nativeHost, "native-host", false, "run the native-messaging host on stdin/stdout")
	fs.StringVar(&mode.nativeFrames, "native-frames", "", "directory of golden frame case files to run through the real native-messaging framing, then exit")
	fs.StringVar(&cfg.WorkDir, "work-dir", cfg.WorkDir, "work directory for --selftest (created and wiped per run)")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format: json | text")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: debug | info | warn | error")
	fs.BoolVar(&cfg.DryRun, "dry-run", cfg.DryRun, "resolve and validate everything, print the plan, and start nothing")

	if err := fs.Parse(args); err != nil {
		return cfg, mode, err
	}
	if fs.NArg() > 0 {
		return cfg, mode, fmt.Errorf("unexpected argument %q; the only positional form is `run`, which is the default", fs.Arg(0))
	}

	selected := 0
	for _, on := range []bool{mode.showVersion, mode.printConfig, mode.selftest, mode.nativeHost, mode.nativeFrames != ""} {
		if on {
			selected++
		}
	}
	if selected > 1 {
		return cfg, mode, errors.New("choose one of --version, --print-config, --selftest, --native-host, --native-frames")
	}
	if err := cfg.validate(mode); err != nil {
		return cfg, mode, err
	}
	return cfg, mode, nil
}

// defaultConfig is what the flags start from. Every default is either inert (a loopback address the
// OS assigns) or explicitly a gap; nothing here decides policy.
func defaultConfig() Config {
	return Config{
		SpoolBoundsProfile: "default",
		Retention:          "720h",
		PolicyKeyID:        "policy-key-1",
		ClassifierBudget:   2 * time.Second,
		EnableTLS:          true,
		TLSListen:          "127.0.0.1:0",
		EnableLoopback:     true,
		DrainDeadline:      30 * time.Second,
		HealthInterval:     30 * time.Second,
		AttachmentCap:      64 << 20, // protocol.MaxAttachmentBytes: the transport ceiling, overridable by policy
		WorkDir:            ".selftest",
		LogFormat:          "json",
		LogLevel:           "info",
	}
}

func newLogger(cfg Config, mode runMode) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	// The native-messaging host must not write anything but frames to stdout: Chromium reads that
	// stream as a message channel, and a log line in it is a protocol violation.
	if mode.nativeHost {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// signalContext returns a context cancelled by SIGINT/SIGTERM, which is what the service manager
// sends on stop.
func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

func protocolVersion() byte { return protocolFramingVersion() }
