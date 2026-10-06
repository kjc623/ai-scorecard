// Package testrig gives this module's tests the shipped rules and model, fresh signing keys, and
// releases signed and loaded through package release.
package testrig

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/release"
)

// ModuleRoot is the classifier-host module directory.
func ModuleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// Rules returns the shipped rules document, rules/default.json.
func Rules(t testing.TB) []byte { return readShipped(t, "default.json") }

// Model returns the shipped model artefact, rules/model.json.
func Model(t testing.TB) []byte { return readShipped(t, "model.json") }

func readShipped(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ModuleRoot(), "rules", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Key generates a release-signing key pair.
func Key(t testing.TB) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// Release signs the shipped rules and model as version into a temporary directory and loads
// them back.
func Release(t testing.TB, version string) *release.Release {
	t.Helper()
	return ReleaseOf(t, version, Rules(t))
}

// ReleaseOf is Release with the given rules document.
func ReleaseOf(t testing.TB, version string, rulesRaw []byte) *release.Release {
	t.Helper()
	priv, pub := Key(t)
	dir := filepath.Join(t.TempDir(), "release")
	if _, err := release.Write(dir, version, rulesRaw, Model(t), priv); err != nil {
		t.Fatal(err)
	}
	rel, err := release.Load(dir, pub)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}
