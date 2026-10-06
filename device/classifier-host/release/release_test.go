package release_test

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/release"
)

func write(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "release")
	if _, err := release.Write(dir, "1.2.3", testrig.Rules(t), testrig.Model(t), priv); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func readFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setManifestField rewrites one manifest field without re-signing.
func setManifestField(t *testing.T, dir, field, value string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readFile(t, dir, release.ManifestFile), &m); err != nil {
		t.Fatal(err)
	}
	m[field] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, release.ManifestFile, out)
}

func TestWriteThenLoad(t *testing.T) {
	priv, pub := testrig.Key(t)
	rel, err := release.Load(write(t, priv), pub)
	if err != nil {
		t.Fatalf("a correctly signed release was refused: %v", err)
	}
	if rel.Version != "1.2.3" || rel.Rules.Len() == 0 || rel.Model == nil {
		t.Errorf("loaded release %+v", rel)
	}
}

func TestReleaseUnderAnotherKeyIsRefused(t *testing.T) {
	priv, _ := testrig.Key(t)
	_, other := testrig.Key(t)
	if _, err := release.Load(write(t, priv), other); !errors.Is(err, release.ErrSignature) {
		t.Fatalf("a release signed by another key: %v", err)
	}
	if _, err := release.Load(write(t, priv), nil); !errors.Is(err, release.ErrSignature) {
		t.Fatalf("a load with no trusted key: %v", err)
	}
}

func TestTamperingIsRefused(t *testing.T) {
	priv, pub := testrig.Key(t)
	cases := []struct {
		name   string
		tamper func(t *testing.T, dir string)
		want   error
	}{
		{"rules edited", func(t *testing.T, dir string) {
			writeFile(t, dir, release.RulesFile, []byte(strings.Replace(string(readFile(t, dir, release.RulesFile)), "0.9", "0.1", 1)))
		}, release.ErrDigest},
		{"model edited", func(t *testing.T, dir string) {
			writeFile(t, dir, release.ModelFile, []byte(strings.Replace(string(readFile(t, dir, release.ModelFile)), "220", "221", 1)))
		}, release.ErrDigest},
		{"rules edited and their digest re-declared", func(t *testing.T, dir string) {
			edited := append(readFile(t, dir, release.RulesFile), ' ')
			writeFile(t, dir, release.RulesFile, edited)
			setManifestField(t, dir, "rules_digest", release.Digest(edited))
		}, release.ErrSignature},
		{"version edited", func(t *testing.T, dir string) { setManifestField(t, dir, "version", "9.9.9") }, release.ErrSignature},
		{"digest in uppercase", func(t *testing.T, dir string) {
			setManifestField(t, dir, "rules_digest", strings.ToUpper(release.Digest(readFile(t, dir, release.RulesFile))))
		}, release.ErrDigest},
		{"unknown manifest field", func(t *testing.T, dir string) { setManifestField(t, dir, "state", "enforcing") }, release.ErrManifest},
		{"model missing", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, release.ModelFile)); err != nil {
				t.Fatal(err)
			}
		}, release.ErrManifest},
		{"directory missing", func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}, release.ErrManifest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := write(t, priv)
			tc.tamper(t, dir)
			if _, err := release.Load(dir, pub); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWriteRefusesContentThatDoesNotCompile(t *testing.T) {
	priv, _ := testrig.Key(t)
	dir := filepath.Join(t.TempDir(), "release")
	if _, err := release.Write(dir, "1", []byte(`{"version":"v","rules":[]}`), testrig.Model(t), priv); !errors.Is(err, release.ErrContent) {
		t.Fatalf("empty rules: %v", err)
	}
	if _, err := release.Write(dir, "1", testrig.Rules(t), []byte(`{}`), priv); !errors.Is(err, release.ErrContent) {
		t.Fatalf("an empty model: %v", err)
	}
	if _, err := release.Write(dir, "", testrig.Rules(t), testrig.Model(t), priv); !errors.Is(err, release.ErrManifest) {
		t.Fatalf("no version: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a refused release left files behind: %v", err)
	}
}
