package release_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeUnvalidated builds a release directory without release.Write's own state check, so the
// loader's validation can be exercised with inputs Write would refuse to produce.
func writeUnvalidated(t *testing.T, dir string, priv ed25519.PrivateKey, m release.Manifest, rulesRaw, modelRaw []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m.RulesFile = release.RulesFileName
	m.RulesDigest = digestOf(rulesRaw)
	if len(modelRaw) > 0 {
		m.ModelFile = release.ModelFileName
		m.ModelDigest = digestOf(modelRaw)
	}
	if err := release.Sign(&m, rulesRaw, modelRaw, priv); err != nil {
		t.Fatalf("signing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, release.RulesFileName), rulesRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(modelRaw) > 0 {
		if err := os.WriteFile(filepath.Join(dir, release.ModelFileName), modelRaw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, release.ManifestFileName), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

const testRules = `{"version":"r1","rules":[{"rule_id":"PAN","class":"payment_card","score":0.9,
	"when":[{"signal":"regex","dialect":"linear","pattern":"[0-9]{13,19}"},{"signal":"validator","validator":"luhn"}]}]}`

func write(t *testing.T, dir string, priv ed25519.PrivateKey, m release.Manifest, rulesRaw, modelRaw []byte) release.Manifest {
	t.Helper()
	out, err := release.Write(dir, m, rulesRaw, modelRaw, priv)
	if err != nil {
		t.Fatalf("release.Write: %v", err)
	}
	return out
}

func key(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func TestLoadVerifiesSignatureDigestsAndState(t *testing.T) {
	priv, pub := key(t)
	dir := filepath.Join(t.TempDir(), "rel")
	write(t, dir, priv, release.Manifest{Version: "v1", State: release.StateShadow}, []byte(testRules), model.DevArtefactJSON())

	rel, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps())
	if err != nil {
		t.Fatalf("a correctly signed release was rejected: %v", err)
	}
	if rel.Version != "v1" || rel.State != release.StateShadow || rel.Rules.RuleCount() != 1 || !rel.HasModel() {
		t.Errorf("loaded release is %+v", rel)
	}

	// A different key must not verify it: §9.5's "signed with the bundle".
	_, otherPub := key(t)
	if _, err := release.Load(dir, release.NewTrust(otherPub), rules.DefaultCaps(), model.DefaultCaps()); err == nil {
		t.Error("a release verified under a key that did not sign it")
	}
	// No trust store at all is not "unverified is fine".
	if _, err := release.Load(dir, release.NewTrust(), rules.DefaultCaps(), model.DefaultCaps()); err == nil {
		t.Error("a release loaded with no trust store")
	}
}

func TestTamperedArtefactsAreRejected(t *testing.T) {
	priv, pub := key(t)

	t.Run("rules bytes changed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rel")
		write(t, dir, priv, release.Manifest{Version: "v1", State: release.StateEnforcing}, []byte(testRules), model.DevArtefactJSON())
		tampered := strings.Replace(testRules, "0.9", "0.1", 1)
		if err := os.WriteFile(filepath.Join(dir, release.RulesFileName), []byte(tampered), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps())
		if err == nil {
			t.Fatal("tampered rules were accepted")
		}
		if !strings.Contains(err.Error(), "declared digest") {
			t.Errorf("the rejection does not name the digest: %v", err)
		}
	})

	t.Run("manifest version changed after signing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rel")
		write(t, dir, priv, release.Manifest{Version: "v1", State: release.StateEnforcing}, []byte(testRules), model.DevArtefactJSON())
		raw, _ := os.ReadFile(filepath.Join(dir, release.ManifestFileName))
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		m["state"] = "enforcing" // the version is signed; a change must break the signature
		m["version"] = "v2"
		out, _ := json.Marshal(m)
		os.WriteFile(filepath.Join(dir, release.ManifestFileName), out, 0o600)
		if _, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps()); err == nil {
			t.Fatal("a manifest edited after signing was accepted")
		}
	})

	t.Run("model bytes changed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rel")
		write(t, dir, priv, release.Manifest{Version: "v1", State: release.StateEnforcing}, []byte(testRules), model.DevArtefactJSON())
		os.WriteFile(filepath.Join(dir, release.ModelFileName), []byte(`{"version":"evil","scale":1,"classes":[]}`), 0o600)
		if _, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps()); err == nil {
			t.Fatal("a tampered model artefact was accepted")
		}
	})
}

// TestPathTraversalIsRefused is the loader-side half of "signed data": a manifest naming a file
// outside the release directory must not read it, whatever os.Root does underneath.
func TestPathTraversalIsRefused(t *testing.T) {
	priv, pub := key(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("private key material"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []string{"../secret.txt", "..\\secret.txt", "/etc/passwd", "sub/../../secret.txt"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "rel")
			write(t, dir, priv, release.Manifest{Version: "v1", State: release.StateShadow}, []byte(testRules), model.DevArtefactJSON())
			raw, _ := os.ReadFile(filepath.Join(dir, release.ManifestFileName))
			var m release.Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			// Re-sign a manifest that points outside the directory: even a *validly signed* one
			// must not be able to read outside its own directory.
			m.RulesFile = name
			payload, err := release.LoadSigningPayload(m, []byte(testRules), mustRead(t, filepath.Join(dir, release.ModelFileName)))
			if err != nil {
				t.Fatal(err)
			}
			m.KeyID = release.KeyID(pub)
			m.Signature = sign(priv, payload)
			out, _ := json.Marshal(m)
			os.WriteFile(filepath.Join(dir, release.ManifestFileName), out, 0o600)
			_, err = release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps())
			if err == nil {
				t.Fatalf("a signed manifest pointing at %q loaded", name)
			}
			if strings.Contains(err.Error(), "private key material") {
				t.Fatalf("the loader read outside the release directory: %v", err)
			}
		})
	}
}

func TestRegectedRulesRetainNothing(t *testing.T) {
	priv, pub := key(t)
	dir := filepath.Join(t.TempDir(), "rel")
	// A rule set that violates the caps: §9.5's "a signed release that violates them is rejected".
	bad := `{"version":"bad","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"` +
		strings.Repeat("a", 600) + `"}]}]}`
	write(t, dir, priv, release.Manifest{Version: "v1", State: release.StateShadow}, []byte(bad), nil)
	_, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps())
	if err == nil {
		t.Fatal("a rules document over the pattern cap was accepted")
	}
	if !strings.Contains(err.Error(), "rules rejected") {
		t.Errorf("the rejection should be attributed to the rules: %v", err)
	}
}

// TestStoreRetainsThePreviousReleaseOnFailure is §9.6: "a release that fails to load ... retains the
// previously loaded release, reports degraded with a specific cause, and never falls back to 'no
// rules'".
func TestStoreRetainsThePreviousReleaseOnFailure(t *testing.T) {
	priv, pub := key(t)
	trust := release.NewTrust(pub)
	store := release.NewStore()

	v1 := filepath.Join(t.TempDir(), "v1")
	write(t, v1, priv, release.Manifest{Version: "v1", State: release.StateEnforcing}, []byte(testRules), model.DevArtefactJSON())
	if _, err := store.Apply(v1, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
		t.Fatalf("applying v1: %v", err)
	}
	if store.Active().Version != "v1" || store.EffectiveState() != release.StateEnforcing {
		t.Fatalf("store after v1: %+v %s", store.Active(), store.EffectiveState())
	}

	// v2: a valid signature over a rules document that violates the caps.
	v2 := filepath.Join(t.TempDir(), "v2")
	bad := `{"version":"bad","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"` +
		strings.Repeat("a", 600) + `"}]}]}`
	write(t, v2, priv, release.Manifest{Version: "v2", State: release.StateEnforcing}, []byte(bad), model.DevArtefactJSON())
	if _, err := store.Apply(v2, trust, rules.DefaultCaps(), model.DefaultCaps()); err == nil {
		t.Fatal("the bad release loaded")
	}
	if store.Active().Version != "v1" {
		t.Fatalf("a failed load replaced the resident release with %q", store.Active().Version)
	}
	f := store.Failure()
	if f == nil || f.Version != "v2" || f.Cause == "" {
		t.Fatalf("the failure was not recorded with a cause: %+v", f)
	}
	if got := store.EffectiveState(); got != release.StateEnforcing {
		t.Errorf("a failed load changed the enforcement state to %q", got)
	}
}

// TestStoreStates walks §9.6's three states through the store.
func TestStoreStates(t *testing.T) {
	priv, pub := key(t)
	trust := release.NewTrust(pub)
	store := release.NewStore()

	v1 := filepath.Join(t.TempDir(), "v1")
	write(t, v1, priv, release.Manifest{Version: "v1", State: release.StateShadow}, []byte(testRules), model.DevArtefactJSON())
	res, err := store.Apply(v1, trust, rules.DefaultCaps(), model.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != release.StateShadow || store.Active().Version != "v1" {
		t.Fatalf("first shadow release: %+v", res)
	}
	if !store.EffectiveState().Shadowed() || store.EffectiveState().Enforces() {
		t.Error("shadow must not enforce")
	}

	v2 := filepath.Join(t.TempDir(), "v2")
	write(t, v2, priv, release.Manifest{Version: "v2", State: release.StateEnforcing}, []byte(testRules), model.DevArtefactJSON())
	res, err = store.Apply(v2, trust, rules.DefaultCaps(), model.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != release.StateEnforcing || res.RetainedVersion != "v1" {
		t.Fatalf("promotion to enforcing: %+v", res)
	}
	if !store.EffectiveState().Enforces() {
		t.Error("enforcing must enforce")
	}

	// §9.6's coexistence: v3 is shadow-evaluated while v2 enforces.
	v3 := filepath.Join(t.TempDir(), "v3")
	write(t, v3, priv, release.Manifest{Version: "v3", State: release.StateShadow}, []byte(testRules), model.DevArtefactJSON())
	if _, err := store.Apply(v3, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
		t.Fatal(err)
	}
	if store.Active().Version != "v2" || store.Shadow() == nil || store.Shadow().Version != "v3" {
		t.Fatalf("shadow release displaced the active one: active=%v shadow=%v", store.Active().Version, store.Shadow())
	}
	if !store.EffectiveState().Enforces() {
		t.Error("shadow-evaluating N+1 turned off N's enforcement")
	}

	v4 := filepath.Join(t.TempDir(), "v4")
	write(t, v4, priv, release.Manifest{Version: "v4", State: release.StateRolledBack, PreviousVersion: "v1"}, []byte(testRules), model.DevArtefactJSON())
	res, err = store.Apply(v4, trust, rules.DefaultCaps(), model.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	if !res.RolledBack || store.EffectiveState() != release.StateRolledBack {
		t.Fatalf("rolled_back: %+v", res)
	}
	if store.EffectiveState().Enforces() {
		t.Error("rolled_back must not enforce")
	}
	if store.Active().Version != "v1" {
		t.Errorf("rolled_back must classify at the previous version, got %q", store.Active().Version)
	}
}

func TestManifestStateAndFileNameRules(t *testing.T) {
	priv, pub := key(t)
	cases := []struct {
		name string
		m    release.Manifest
	}{
		{"unknown state", release.Manifest{Version: "v", State: "auditing"}},
		{"empty state", release.Manifest{Version: "v"}},
		{"no version", release.Manifest{State: release.StateShadow}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "rel")
			writeUnvalidated(t, dir, priv, tc.m, []byte(testRules), nil)
			if _, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps()); err == nil {
				t.Fatal("an invalid manifest was accepted")
			}
		})
	}
}

func TestStateHelpers(t *testing.T) {
	if !release.StateShadow.Shadowed() || release.StateShadow.Enforces() {
		t.Error("shadow")
	}
	if !release.StateRolledBack.Shadowed() || release.StateRolledBack.Enforces() {
		t.Error("rolled_back")
	}
	if !release.StateEnforcing.Enforces() || release.StateEnforcing.Shadowed() {
		t.Error("enforcing")
	}
	if release.State("nonsense").Valid() {
		t.Error("an unknown state validated")
	}
}

func sign(priv ed25519.PrivateKey, payload []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDigestHasExactlyOneSpelling is the seam between this loader and the store.
//
// A digest is `sha256:<64 lowercase hex>` and only that spelling is the same value. Uppercase hex is
// the same bytes written differently, which is exactly why it must not be accepted: the database's
// own digest columns carry `~ '^sha256:[0-9a-f]{64}$'`, so a loader that accepted the uppercase form
// would admit a release the store would refuse. This test was added after the vault's implementation
// found the same fact from the other side (ops.retrieval_grant.raw_digest), because two components
// each individually correct had disagreed here.
func TestDigestHasExactlyOneSpelling(t *testing.T) {
	priv, pub := key(t)
	dir := filepath.Join(t.TempDir(), "rel")
	rulesRaw := []byte(testRules)
	modelRaw := model.DevArtefactJSON()
	correct := digestOf(rulesRaw)
	upper := strings.ToUpper(correct)
	if upper == correct {
		t.Fatal("the fixture digest has no letters, so the test cannot distinguish the spellings")
	}

	writeSpelled := func(spelling string) {
		t.Helper()
		m := release.Manifest{
			Version: "v1", State: release.StateShadow,
			RulesFile: release.RulesFileName, RulesDigest: spelling,
			ModelFile: release.ModelFileName, ModelDigest: digestOf(modelRaw),
		}
		if err := release.Sign(&m, rulesRaw, modelRaw, priv); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string][]byte{release.RulesFileName: rulesRaw, release.ModelFileName: modelRaw} {
			if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, release.ManifestFileName), out, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The negative case: a valid signature over a correctly-computed digest written in uppercase.
	// Only the spelling is wrong, so anything that accepts it is accepting a second spelling of one
	// value rather than detecting a mismatch.
	writeSpelled(upper)
	_, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps())
	if err == nil {
		t.Fatal("an uppercase digest was accepted; the store would refuse the release the loader admitted")
	}
	if !strings.Contains(err.Error(), "does not match its declared digest") {
		t.Fatalf("the uppercase digest was refused for the wrong reason: %v", err)
	}

	// The positive control: the identical construction with the lowercase spelling loads, so the
	// negative case is about the spelling and not about a broken fixture.
	writeSpelled(correct)
	if _, err := release.Load(dir, release.NewTrust(pub), rules.DefaultCaps(), model.DefaultCaps()); err != nil {
		t.Fatalf("the correct lowercase digest was rejected: %v", err)
	}
}
