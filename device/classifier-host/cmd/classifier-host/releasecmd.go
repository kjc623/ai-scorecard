package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/release"
)

// runRelease builds a signed release directory. It is a *development* tool: it reads a raw
// ed25519 private key from a file so the build host can produce a verifiable release offline.
//
// Production signing is not this: §9.5 requires rules to be "signed with the bundle", and the
// private key for that never lives on a developer's disk or in a test's temporary directory. What
// this subcommand produces is a release whose signature, digests and caps are checked by exactly
// the code that will check production releases.
func runRelease(args []string) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	dir := fs.String("dir", "", "release directory to write")
	state := fs.String("state", "shadow", "release state: shadow | enforcing | rolled_back")
	version := fs.String("version", "dev-1", "release version")
	previous := fs.String("previous", "", "previous_version, for a rolled_back release")
	keyPath := fs.String("key", "", "ed25519 private key file (hex seed); created if absent")
	rulesPath := fs.String("rules", "", "rules JSON file; omit for an empty-rules release (which is rejected)")
	noModel := fs.Bool("no-model", false, "build a rules-only release, which makes the model stage degrade with model_unavailable")
	printPub := fs.Bool("print-pubkey", false, "print the public key for --pubkey and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keyPath == "" {
		return fatalf("release: --key is required")
	}
	priv, pub, err := loadOrCreateKey(*keyPath)
	if err != nil {
		return fatalf("release: %v", err)
	}
	if *printPub {
		fmt.Println(hex.EncodeToString(pub))
		return 0
	}
	if *dir == "" {
		return fatalf("release: --dir is required")
	}
	if *rulesPath == "" {
		return fatalf("release: --rules is required")
	}
	rulesRaw, err := os.ReadFile(*rulesPath)
	if err != nil {
		return fatalf("release: %v", err)
	}
	var modelRaw []byte
	if !*noModel {
		modelRaw = model.DevArtefactJSON()
	}
	m := release.Manifest{
		Version:         *version,
		State:           release.State(*state),
		PreviousVersion: *previous,
	}
	written, err := release.Write(*dir, m, rulesRaw, modelRaw, priv)
	if err != nil {
		return fatalf("release: %v", err)
	}
	fmt.Printf("pubkey=%s\nkey_id=%s\nversion=%s\nstate=%s\nrules_digest=%s\nmodel_digest=%s\n",
		hex.EncodeToString(pub), written.KeyID, written.Version, written.State,
		written.RulesDigest, written.ModelDigest)
	return 0
}

// loadOrCreateKey reads a hex ed25519 seed from path, creating one if the file does not exist.
func loadOrCreateKey(path string) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, nil, fmt.Errorf("key file %s is not hex: %w", path, err)
		}
		if len(seed) != ed25519.SeedSize {
			return nil, nil, fmt.Errorf("key file %s holds %d bytes, want a %d-byte seed", path, len(seed), ed25519.SeedSize)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		return priv, priv.Public().(ed25519.PublicKey), nil
	}
	if !os.IsNotExist(err) {
		return nil, nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
		return nil, nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey), nil
}
