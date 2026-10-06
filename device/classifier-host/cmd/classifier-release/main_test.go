package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/release"
)

func writeSeed(t *testing.T, dir string) (string, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "classifier.key.hex")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

func shipped(name string) string { return filepath.Join(testrig.ModuleRoot(), "rules", name) }

func TestBuildsAReleaseOfTheShippedRulesAndModel(t *testing.T) {
	tmp := t.TempDir()
	key, pub := writeSeed(t, tmp)
	out := filepath.Join(tmp, "classifier")
	var stdout bytes.Buffer
	err := run([]string{"--rules", shipped("default.json"), "--model", shipped("model.json"), "--key", key, "--version", "2026.10.1", "--out", out}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "pubkey="+hex.EncodeToString(pub)+"\n") || !strings.Contains(stdout.String(), "version=2026.10.1\n") {
		t.Errorf("output:\n%s", stdout.String())
	}
	rel, err := release.Load(out, pub)
	if err != nil || rel.Version != "2026.10.1" {
		t.Fatalf("the release does not load: %v", err)
	}
	entries, _ := os.ReadDir(out)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "manifest.json,model.json,rules.json" {
		t.Errorf("the release holds %v", names)
	}
	if rules, _ := os.ReadFile(filepath.Join(out, release.RulesFile)); !bytes.Equal(rules, testrig.Rules(t)) {
		t.Error("the release's rules are not the shipped rules byte for byte")
	}
}

func TestFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	key, _ := writeSeed(t, tmp)
	badKey := filepath.Join(tmp, "bad.key")
	badRules := filepath.Join(tmp, "bad-rules.json")
	for path, body := range map[string]string{badKey: "not a seed", badRules: `{"version":"v","rules":[]}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	full := map[string]string{"--rules": shipped("default.json"), "--model": shipped("model.json"), "--key": key, "--version": "1", "--out": filepath.Join(tmp, "out")}
	args := func(change map[string]string) []string {
		var out []string
		for flag, value := range full {
			if v, ok := change[flag]; ok {
				value = v
			}
			if value != "" {
				out = append(out, flag, value)
			}
		}
		return out
	}
	cases := map[string]map[string]string{
		"no key":           {"--key": ""},
		"missing key file": {"--key": filepath.Join(tmp, "absent.key")},
		"malformed key":    {"--key": badKey},
		"no rules":         {"--rules": ""},
		"rules that fail":  {"--rules": badRules},
		"no model":         {"--model": ""},
		"no version":       {"--version": ""},
		"no output":        {"--out": ""},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(args(change), &bytes.Buffer{}); err == nil {
				t.Fatal("a release was built")
			}
			if _, err := os.Stat(filepath.Join(tmp, "out")); !os.IsNotExist(err) {
				t.Fatalf("a refused build wrote output: %v", err)
			}
			if _, err := os.Stat(filepath.Join(tmp, "absent.key")); !os.IsNotExist(err) {
				t.Fatal("a signing key was created")
			}
		})
	}
}
