// Command classifier-host is the classifier host of docs/01-collectors.md §9 and the parent of
// the §10 parser child. One source builds for native (the resident host) and for GOOS=js
// GOARCH=wasm (the extension's in-page copy), and the `classify` subcommand runs the same fixed
// corpus through both, which is what §9.1's equivalence property is measured over.
//
// Subcommands:
//
//	classify    run a corpus once and write the canonical, timing-free verdict records
//	measure     run a corpus N times and write §9.4's per-stage latency percentiles
//	release     build a signed release directory (development tool; the signing key is a file)
//	serve       serve the local request/response channel (native: stdio, unix socket or loopback)
//	parse-child the parser child itself, spawned by parser/isolation; reads one framed document
//	version     print the build's target and version
//
// Every subcommand that loads a release takes `--release <dir> --pubkey <hex>`; there is no
// default key and no unsigned path, because a release the device cannot verify is a release the
// device must not run (§9.5).
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/classifier-host/docparse"
	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
)

// Version is the host binary's version. It is the classifier_version of last resort when no
// release is loaded, so no response is ever unattributable.
const Version = "0.1.0-dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	var code int
	switch args[0] {
	case "classify":
		code = runClassify(args[1:])
	case "measure":
		code = runMeasure(args[1:])
	case "release":
		code = runRelease(args[1:])
	case "serve":
		code = runServe(args[1:])
	case "parse-child":
		code = runParseChild(args[1:])
	case "version":
		fmt.Printf("classifier-host %s %s/%s\n", Version, runtime.GOOS, runtime.GOARCH)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "classifier-host: unknown subcommand %q\n\n", args[0])
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `classifier-host — rules -> validators -> model classification (docs/01-collectors.md §9)

  classify    --release DIR --pubkey HEX --corpus FILE [--out FILE]
  measure     --release DIR --pubkey HEX --corpus FILE [--iterations N] [--out FILE]
  release     --dir DIR --state shadow|enforcing|rolled_back --version V --key FILE
              [--rules FILE] [--no-model] [--previous V] [--print-pubkey]
  serve       --release DIR --pubkey HEX [--transport stdio|unix|tcp] [--addr VALUE]
  parse-child
  version

The classify and measure subcommands exist so the same corpus can be run through the native and
the js/wasm build and the outputs diffed: §9.1's byte-identical-labels property is asserted by
device/classifier-host/equivalence_test.go, not by convention.
`)
}

func fatalf(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "classifier-host: "+format+"\n", args...)
	return 1
}

// targetBudget is §9.4's ladder for the target this binary was built for. One source, two
// targets, two published columns — the budget is the only thing the target selects.
func targetBudget() classify.Budget {
	if runtime.GOARCH == "wasm" {
		return classify.WASMBudget()
	}
	return classify.NativeBudget()
}

// rig bundles the pieces a classification needs.
type rig struct {
	store *release.Store
	host  *classify.Host
}

// loadRig verifies and loads the release, wires the parser child for this target, and builds the
// host.
func loadRig(releaseDir, pubkeyHex string) (*rig, error) {
	if releaseDir == "" || pubkeyHex == "" {
		return nil, fmt.Errorf("--release and --pubkey are both required: a release must be verified before it is run")
	}
	pubBytes, err := hex.DecodeString(strings.TrimSpace(pubkeyHex))
	if err != nil {
		return nil, fmt.Errorf("--pubkey is not hex: %w", err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("--pubkey is %d bytes, want an ed25519 public key of %d", len(pubBytes), ed25519.PublicKeySize)
	}
	store := release.NewStore()
	if _, err := store.Apply(releaseDir, release.NewTrust(ed25519.PublicKey(pubBytes)), rules.DefaultCaps(), model.DefaultCaps()); err != nil {
		return nil, err
	}
	host, err := classify.New(classify.Options{
		Store:       store,
		Parser:      newParserRunner(),
		Budget:      targetBudget(),
		Metrics:     classify.NewMetrics(512),
		Version:     Version,
		Enforcement: classify.DefaultEnforcement(),
	})
	if err != nil {
		return nil, err
	}
	return &rig{store: store, host: host}, nil
}

// newParserRunner is defined per target: parser/isolation on native, nil on js/wasm where §9.1's
// table says document parsing is unavailable.
var _ = docparse.CauseNone

func osName() string   { return runtime.GOOS }
func archName() string { return runtime.GOARCH }
