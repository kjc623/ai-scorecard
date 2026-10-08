// Command capture-core is the Shadow AI Capture endpoint agent: one binary per platform that runs
// the collectors (the local TLS proxy, the loopback broker and the CLI trust shim), the policy
// engine, the spool, the classifier host as a child process, and the device-to-cloud drain.
//
// It runs in one of four ways:
//
//   - as the service: started by the Windows Service Control Manager, launchd or systemd with
//     --config-file arguments, until stopped;
//   - as the browser's native-messaging host: a browser starts it with the extension's origin
//     (chrome-extension://<id>/) as the first argument, and it relays frames between the browser
//     and the running service;
//   - as the user-session helper: the service starts it with --user-helper in each signed-in
//     session (Windows), and it shows the notifications the service asks for;
//   - as a diagnostic: --print-config or --version.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// version is the agent version, set at release build time with -ldflags "-X main.version=...".
var version = "dev"

// userHelperArg is the only argument the service starts the user-session helper with.
const userHelperArg = "--user-helper"

func main() {
	if len(os.Args) > 1 && isBrowserOrigin(os.Args[1]) {
		if err := runRelay(os.Stdin, os.Stdout, dialNative); err != nil {
			fmt.Fprintf(os.Stderr, "capture-core: native-messaging relay: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == userHelperArg {
		if err := runUserHelper(); err != nil {
			fmt.Fprintf(os.Stderr, "capture-core: user-session helper: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "capture-core: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, mode, err := parseFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		usage(os.Stdout)
		return nil
	}
	if err != nil {
		return err
	}
	if mode.showVersion {
		fmt.Printf("capture-core %s\n", version)
		return nil
	}
	if mode.printConfig {
		return printConfig(cfg, os.Stdout)
	}
	if handled, err := runPlatformService(cfg); handled {
		return err
	}
	return runService(cfg, newLogger(cfg.LogLevel, os.Stderr))
}

// isBrowserOrigin reports whether arg is the caller origin Chrome and Edge pass a native-messaging
// host as its first argument.
func isBrowserOrigin(arg string) bool {
	return strings.HasPrefix(arg, "chrome-extension://")
}

// newLogger is the agent's structured JSON logger.
func newLogger(level string, w io.Writer) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}
