// Command classifier-release builds the signed classifier release that the agent packages
// install beside classifier-host. It runs at build time, never on a device:
//
//	classifier-release --rules FILE --model FILE --key FILE --version VERSION --out DIR
//
// --key names a file holding the release-signing key as a hex-encoded 32-byte Ed25519 seed. Every
// flag is required. The rules and the model are compiled before anything is signed, and the
// written release is loaded back and verified before the command reports success. On success it
// prints, one per line, pubkey=<hex public key>, key_id, version, rules_digest and model_digest;
// pubkey is the value the package passes to capture-core as SAC_CLASSIFIER_PUBKEY.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/release"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "classifier-release:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("classifier-release", flag.ContinueOnError)
	rulesPath := fs.String("rules", "", "rules document (device/classifier-host/rules/default.json)")
	modelPath := fs.String("model", "", "model artefact (device/classifier-host/rules/model.json)")
	keyPath := fs.String("key", "", "file holding the release-signing key as a hex Ed25519 seed")
	version := fs.String("version", "", "release version, reported as classifier_version")
	out := fs.String("out", "", "directory to write the release into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	for _, f := range []struct{ name, value string }{
		{"--rules", *rulesPath}, {"--model", *modelPath}, {"--key", *keyPath}, {"--version", *version}, {"--out", *out},
	} {
		if f.value == "" {
			return fmt.Errorf("%s is required", f.name)
		}
	}
	priv, err := readKey(*keyPath)
	if err != nil {
		return err
	}
	rulesRaw, err := os.ReadFile(*rulesPath)
	if err != nil {
		return err
	}
	modelRaw, err := os.ReadFile(*modelPath)
	if err != nil {
		return err
	}
	m, err := release.Write(*out, *version, rulesRaw, modelRaw, priv)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	if _, err := release.Load(*out, pub); err != nil {
		return fmt.Errorf("the written release does not load: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "pubkey=%s\nkey_id=%s\nversion=%s\nrules_digest=%s\nmodel_digest=%s\n",
		hex.EncodeToString(pub), m.KeyID, m.Version, m.RulesDigest, m.ModelDigest)
	return err
}

// readKey reads a hex-encoded Ed25519 seed. The file must exist: a release is signed only with
// the key the packages' public key belongs to.
func readKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the signing key: %w", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the signing key file must hold a 32-byte Ed25519 seed in hex")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
