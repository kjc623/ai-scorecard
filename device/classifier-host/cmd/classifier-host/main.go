// Command classifier-host classifies content for capture-core, which starts it as a child process
// and exchanges length-prefixed frames with it over the child's stdin and stdout:
//
//	classifier-host serve --release DIR --pubkey HEX [--transport stdio]
//
// serve loads the signed release in DIR, refuses to start unless it verifies under the Ed25519
// public key HEX, and answers until stdin closes. Documents are parsed in a further child of this
// same executable, started as `classifier-host parse-child` for each document. Diagnostics go to
// stderr as JSON lines.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/classifier-host/parser"
	"github.com/shadow-ai-capture/device/classifier-host/parser/isolation"
	"github.com/shadow-ai-capture/device/classifier-host/release"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: classifier-host serve --release DIR --pubkey HEX [--transport stdio] [--parser-timeout DURATION] | parse-child")
		return 2
	}
	switch args[0] {
	case "serve":
		log := slog.New(slog.NewJSONHandler(stderr, nil))
		if err := serve(args[1:], stdin, stdout, log); err != nil {
			log.Error("classifier-host stopped", "error", err)
			return 1
		}
		return 0
	case "parse-child":
		return parser.Execute(stdin, stdout, parser.DefaultLimits())
	default:
		fmt.Fprintf(stderr, "classifier-host: unknown command %q\n", args[0])
		return 2
	}
}

func serve(args []string, stdin io.Reader, stdout io.Writer, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	releaseDir := fs.String("release", "", "signed classifier release directory")
	pubkeyHex := fs.String("pubkey", "", "hex Ed25519 public key the release must verify under")
	transport := fs.String("transport", "stdio", "the only transport is stdio")
	parserTimeout := fs.Duration("parser-timeout", isolation.DefaultLimits().Timeout, "per-document parser timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *releaseDir == "" || *pubkeyHex == "" {
		return errors.New("--release and --pubkey are both required")
	}
	if *transport != "stdio" {
		return fmt.Errorf("transport %q is not supported; the classifier serves its parent over stdio", *transport)
	}
	pub, err := hex.DecodeString(strings.TrimSpace(*pubkeyHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("--pubkey must be a %d-byte Ed25519 public key in hex", ed25519.PublicKeySize)
	}
	rel, err := release.Load(*releaseDir, ed25519.PublicKey(pub))
	if err != nil {
		return err
	}
	child, err := isolation.DefaultCommand()
	if err != nil {
		return err
	}
	limits := isolation.DefaultLimits()
	limits.Timeout = *parserTimeout
	host, err := classify.New(classify.Options{Release: rel, Parser: isolation.New(child, limits)})
	if err != nil {
		return err
	}
	log.Info("classifier-host serving", "release", rel.Version, "rules", rel.Rules.Version(), "model", rel.Model.Version())
	return classify.NewServer(host).ServeConn(stdio{stdin, stdout})
}

// stdio joins the process's stdin and stdout into the connection the server reads and writes.
type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error { return nil }
