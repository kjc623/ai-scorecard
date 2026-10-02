// Package testrig builds signed releases, stores and hosts for tests in this module.
//
// It is a test-only helper (nothing in the product imports it) and it generates its own ed25519
// keys per call, so no private key is ever committed: the release *format*, the signature check
// and the digest check are the production ones, exercised through package release's own Write and
// Load rather than through a test-only shortcut.
package testrig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
)

func defaultRulesCaps() rules.Caps { return rules.DefaultCaps() }

// ModuleRoot is the device/classifier-host directory, located from this file rather than from the
// process's working directory so every package's tests find testdata/ the same way.
func ModuleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testrig: cannot locate the module root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// Testdata returns a path inside the module's testdata directory.
func Testdata(name string) string {
	return filepath.Join(ModuleRoot(), "testdata", name)
}

// DevRules is the development rule set the corpus is built against.
func DevRules(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(Testdata("dev-rules.json"))
	if err != nil {
		t.Fatalf("testrig: reading dev rules: %v", err)
	}
	return b
}

// Key generates a release-signing key pair.
func Key(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("testrig: generating a key: %v", err)
	}
	return priv, pub
}

// PubHex is the --pubkey form of a public key.
func PubHex(pub ed25519.PublicKey) string { return hex.EncodeToString(pub) }

// ReleaseOptions control what a fixture release contains.
type ReleaseOptions struct {
	Rules    []byte // nil means the development rule set
	Model    []byte // nil means model.DevArtefactJSON()
	NoModel  bool   // a rules-only release
	Previous string
	Version  string
	State    release.State
}

// WriteRelease builds a signed release directory under dir and returns its manifest.
func WriteRelease(t *testing.T, dir string, priv ed25519.PrivateKey, opts ReleaseOptions) release.Manifest {
	t.Helper()
	rulesRaw := opts.Rules
	if rulesRaw == nil {
		rulesRaw = DevRules(t)
	}
	var modelRaw []byte
	if !opts.NoModel {
		modelRaw = opts.Model
		if modelRaw == nil {
			modelRaw = model.DevArtefactJSON()
		}
	}
	version := opts.Version
	if version == "" {
		version = "test-1"
	}
	state := opts.State
	if state == "" {
		state = release.StateEnforcing
	}
	m, err := release.Write(dir, release.Manifest{
		Version:         version,
		State:           state,
		PreviousVersion: opts.Previous,
	}, rulesRaw, modelRaw, priv)
	if err != nil {
		t.Fatalf("testrig: writing the release: %v", err)
	}
	return m
}

// Store builds a store with one applied release and returns it with the release directory and the
// public key's hex form.
func Store(t *testing.T, state release.State, opts ReleaseOptions) (*release.Store, string, string) {
	t.Helper()
	priv, pub := Key(t)
	dir := filepath.Join(t.TempDir(), "release")
	opts.State = state
	WriteRelease(t, dir, priv, opts)
	store := release.NewStore()
	if _, err := store.Apply(dir, release.NewTrust(pub), defaultRulesCaps(), model.DefaultCaps()); err != nil {
		t.Fatalf("testrig: applying the release: %v", err)
	}
	return store, dir, PubHex(pub)
}

// Host builds a host over a store, with options a caller can adjust.
func Host(t *testing.T, store *release.Store, adjust ...func(*classify.Options)) *classify.Host {
	t.Helper()
	opts := classify.Options{
		Store:       store,
		Budget:      classify.NativeBudget(),
		Metrics:     classify.NewMetrics(64),
		Version:     "test-host",
		Enforcement: classify.DefaultEnforcement(),
	}
	for _, f := range adjust {
		f(&opts)
	}
	h, err := classify.New(opts)
	if err != nil {
		t.Fatalf("testrig: building the host: %v", err)
	}
	return h
}
