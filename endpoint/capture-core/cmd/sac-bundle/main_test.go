package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/policy"
)

func testPolicyKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate policy key: %v", err)
	}
	return pub, priv
}

// runBundle runs the generator into a fresh temp dir with a known policy key and returns the dir
// and the public key the bundle is signed under.
func runBundle(t *testing.T, extra ...string) (string, ed25519.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	pub, priv := testPolicyKey(t)
	args := append([]string{
		"--out", dir,
		"--policy-priv", hex.EncodeToString(priv),
		"--policy-key-id", "policy-key-1",
	}, extra...)
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("Run returned %d: %s", code, stderr.String())
	}
	return dir, pub
}

// The generated bundle must verify through the same chain the device uses (NewVerifier/NewStore),
// the CA PEM must parse, its sha256 must match root_ca_fingerprint, and the key must be 0600.
func TestRun_ProducesVerifiableBundle(t *testing.T) {
	dir, pub := runBundle(t, "--hosts", "api.example.invalid", "--tenant-hosts", "corp.example.invalid")

	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		t.Fatalf("read bundle.json: %v", err)
	}

	v, err := policy.NewVerifier("policy-key-1", pub)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	store, err := policy.NewStore(v, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if res := store.Apply(raw); res.Outcome != policy.OutcomeAccepted {
		t.Fatalf("bundle did not verify: %+v", res)
	}
	b := store.InForce()
	if b.Version != "1" {
		t.Fatalf("version = %q, want %q", b.Version, "1")
	}
	if len(b.Interception.SeedHosts) != 1 || b.Interception.SeedHosts[0] != "api.example.invalid" {
		t.Fatalf("seed hosts = %v", b.Interception.SeedHosts)
	}

	// The CA PEM on disk must parse and its sha256 must match root_ca_fingerprint.
	caPEMBytes, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	block, _ := pem.Decode(caPEMBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("ca.pem does not hold a certificate PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ca.pem does not parse: %v", err)
	}
	sum := sha256.Sum256(cert.Raw)
	want := hex.EncodeToString(sum[:])
	if b.Interception.RootCAFingerprint != want {
		t.Fatalf("root_ca_fingerprint = %q, want %q", b.Interception.RootCAFingerprint, want)
	}

	// ca.key must be mode 0600.
	fi, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("stat ca.key: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode = %v, want 0600", fi.Mode().Perm())
	}
	// ca.pem is also written 0600 (a CA root is a trust capability).
	if fi, err := os.Stat(filepath.Join(dir, "ca.pem")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca.pem mode = %v, want 0600 (err=%v)", fi.Mode().Perm(), err)
	}
}

// With no --policy-priv the generator mints a key, writes policy-key.pub, and the bundle verifies
// under that public key.
func TestRun_GeneratesPolicyKeyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--out", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run returned %d: %s", code, stderr.String())
	}

	pubHex, err := os.ReadFile(filepath.Join(dir, "policy-key.pub"))
	if err != nil {
		t.Fatalf("policy-key.pub not written: %v", err)
	}
	pub, err := hex.DecodeString(string(bytes.TrimSpace(pubHex)))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("policy-key.pub is not a public key: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		t.Fatalf("read bundle.json: %v", err)
	}
	v, err := policy.NewVerifier("policy-key-1", pub)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := v.Open(raw, nil, nil); err != nil {
		t.Fatalf("generated-key bundle did not verify: %v", err)
	}
}

// A --tool-mode outside the closed set is refused before anything is written.
func TestRun_RefusesBadToolMode(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--out", dir, "--tool-mode", "foo=m9"}, &stdout, &stderr); code == 0 {
		t.Fatal("Run accepted a --tool-mode outside {m0,m1,m2,m3}")
	}
}

// Loading an existing CA pair must produce a bundle whose root_ca_pem matches the loaded cert and
// must not overwrite the loaded --ca-key/--ca-cert files.
func TestRun_LoadsExistingCA(t *testing.T) {
	seedDir := t.TempDir()
	var seedOut, seedErr bytes.Buffer
	if code := Run([]string{"--out", seedDir}, &seedOut, &seedErr); code != 0 {
		t.Fatalf("seed Run failed: %s", seedErr.String())
	}
	caCert, err := os.ReadFile(filepath.Join(seedDir, "ca.pem"))
	if err != nil {
		t.Fatalf("read seed ca.pem: %v", err)
	}
	caKey, err := os.ReadFile(filepath.Join(seedDir, "ca.key"))
	if err != nil {
		t.Fatalf("read seed ca.key: %v", err)
	}

	dir := t.TempDir()
	_, priv := testPolicyKey(t)
	args := []string{
		"--out", dir,
		"--policy-priv", hex.EncodeToString(priv),
		"--ca-cert", filepath.Join(seedDir, "ca.pem"),
		"--ca-key", filepath.Join(seedDir, "ca.key"),
	}
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("Run with a loaded CA: %s", stderr.String())
	}

	// The loaded inputs must be byte-for-byte intact.
	loadedKey, _ := os.ReadFile(filepath.Join(seedDir, "ca.key"))
	if !bytes.Equal(loadedKey, caKey) {
		t.Fatal("--ca-key was overwritten")
	}
	loadedCert, _ := os.ReadFile(filepath.Join(seedDir, "ca.pem"))
	if !bytes.Equal(loadedCert, caCert) {
		t.Fatal("--ca-cert was overwritten")
	}

	// The output ca.pem carries the same certificate as the loaded one.
	outCert, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatalf("read output ca.pem: %v", err)
	}
	outBlk, _ := pem.Decode(outCert)
	loadedBlk, _ := pem.Decode(caCert)
	if outBlk == nil || loadedBlk == nil || !bytes.Equal(outBlk.Bytes, loadedBlk.Bytes) {
		t.Fatal("output ca.pem does not match the loaded certificate")
	}
}

// Re-minting a bundle into the same --out directory while reusing the same CA must work: keeping
// the CA and changing only the collection mode is the normal operator path ("raise this tenant to
// m2"), and the CA files must be left byte-for-byte intact.
func TestRun_ReusesCAInPlace(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := Run([]string{"--out", dir, "--tenant-default", "m1"}, &out, &errb); code != 0 {
		t.Fatalf("seed Run: %s", errb.String())
	}
	cert, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	key, _ := os.ReadFile(filepath.Join(dir, "ca.key"))

	var out2, err2 bytes.Buffer
	code := Run([]string{
		"--out", dir,
		"--ca-cert", filepath.Join(dir, "ca.pem"),
		"--ca-key", filepath.Join(dir, "ca.key"),
		"--tenant-default", "m2",
	}, &out2, &err2)
	if code != 0 {
		t.Fatalf("in-place re-mint failed: %s", err2.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "ca.pem")); !bytes.Equal(got, cert) {
		t.Fatal("ca.pem changed during an in-place re-mint")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "ca.key")); !bytes.Equal(got, key) {
		t.Fatal("ca.key changed during an in-place re-mint")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	var sb struct {
		Payload struct {
			TenantDefault string `json:"tenant_default_mode"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &sb); err != nil {
		t.Fatalf("bundle is not JSON: %v", err)
	}
	if sb.Payload.TenantDefault != "m2" {
		t.Fatalf("tenant_default_mode = %q, want m2", sb.Payload.TenantDefault)
	}
}
