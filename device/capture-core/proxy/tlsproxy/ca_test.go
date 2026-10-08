package tlsproxy

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

func testKeyPEM(t *testing.T, ca *CA) []byte {
	t.Helper()
	keyPEM, err := encodeKeyPEM(ca.key.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatalf("encodeKeyPEM: %v", err)
	}
	return keyPEM
}

// A CA that survives a PEM round-trip (mint -> PEM -> NewCAFromPEM) must be the same CA: the same
// fingerprint, and a leaf minted by the reloaded CA verifies against the reloaded pool.
func TestCAPEMRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ca, err := NewCA("device-1", now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	reloaded, err := NewCAFromPEM(ca.PEM(), testKeyPEM(t, ca), now)
	if err != nil {
		t.Fatalf("NewCAFromPEM: %v", err)
	}
	if reloaded.Info().Fingerprint != ca.Info().Fingerprint {
		t.Fatalf("reloaded fingerprint = %q, want %q", reloaded.Info().Fingerprint, ca.Info().Fingerprint)
	}
	verifyLeaf(t, reloaded, now)
}

// verifyLeaf mints a leaf and verifies it against the CA's pool.
func verifyLeaf(t *testing.T, ca *CA, now time.Time) {
	t.Helper()
	leaf, err := ca.Leaf("api.example.invalid", now)
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := parsed.Verify(x509.VerifyOptions{
		Roots:       ca.Pool(),
		DNSName:     "api.example.invalid",
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: now,
	}); err != nil {
		t.Fatalf("minted leaf does not verify against the CA's pool: %v", err)
	}
}

// A SEC1 ("EC PRIVATE KEY") key is accepted alongside PKCS#8.
func TestNewCAFromPEMAcceptsSEC1Key(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ca, err := NewCA("device-1", now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	sec1, err := x509.MarshalECPrivateKey(ca.key.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	sec1PEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})
	if _, err := NewCAFromPEM(ca.PEM(), sec1PEM, now); err != nil {
		t.Fatalf("SEC1 key was refused: %v", err)
	}
}

// A mismatched certificate/key pair, a non-EC key and a multi-block PEM are all refused, because a
// CA the proxy cannot actually use must not be accepted silently.
func TestNewCAFromPEMRejectsUnusableInputs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ca, err := NewCA("device-1", now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM := ca.PEM()
	keyPEM := testKeyPEM(t, ca)

	other, err := NewCA("device-2", now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	if _, err := NewCAFromPEM(certPEM, testKeyPEM(t, other), now); err == nil {
		t.Fatal("a mismatched cert/key pair was accepted")
	}

	// A PKCS#8 block that does not hold an EC key must fail to parse as EC.
	garbage := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{0x30, 0x03, 0x02, 0x01, 0x01}})
	if _, err := NewCAFromPEM(certPEM, garbage, now); err == nil {
		t.Fatal("a non-EC PKCS#8 key was accepted")
	}

	// Two certificates are refused: exactly one is required.
	if _, err := NewCAFromPEM(append(append([]byte(nil), certPEM...), certPEM...), keyPEM, now); err == nil {
		t.Fatal("two concatenated certificates were accepted")
	}
}

// A configured CA becomes the provider's CA.
func TestProviderUsesTheConfiguredCA(t *testing.T) {
	ca, err := NewCA("device-1", time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	p := New(Config{CA: ca, Log: testLogger{t}})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start with a configured CA: %v", err)
	}
	defer p.Stop(context.Background())
	if got := p.CA(); got == nil || got.Info().Fingerprint != ca.Info().Fingerprint {
		t.Fatalf("the provider does not run the configured CA")
	}
}

// ---- the kept device root -------------------------------------------------------------------

// opaqueSigner exposes only crypto.Signer, as a keystore key does.
type opaqueSigner struct{ key *ecdsa.PrivateKey }

func (s opaqueSigner) Public() crypto.PublicKey { return s.key.Public() }
func (s opaqueSigner) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return s.key.Sign(r, digest, opts)
}

// fakeKeyStore is a keystore that holds one key in memory and never hands out its private half.
type fakeKeyStore struct {
	key       *ecdsa.PrivateKey
	generated int
	log       *[]string
}

func (s *fakeKeyStore) Key() (crypto.Signer, error) {
	if s.key == nil {
		return nil, errors.New("no key")
	}
	return opaqueSigner{s.key}, nil
}

func (s *fakeKeyStore) Generate() (crypto.Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	s.key = key
	s.generated++
	if s.log != nil {
		*s.log = append(*s.log, "keystore.generate")
	}
	return opaqueSigner{key}, nil
}

// recordingTrust is a trust store that holds several roots and removes the one it last installed,
// as the platform stores do. It records each call, and whether the key file was still there when a
// root was removed.
type recordingTrust struct {
	roots     map[string]bool
	last      []byte
	log       *[]string
	keyFile   string
	removeErr error
}

func newRecordingTrust(log *[]string, keyFile string) *recordingTrust {
	return &recordingTrust{roots: map[string]bool{}, log: log, keyFile: keyFile}
}

func (t *recordingTrust) Install(_ context.Context, der []byte) error {
	*t.log = append(*t.log, "trust.install")
	t.roots[string(der)] = true
	t.last = append([]byte(nil), der...)
	return nil
}

func (t *recordingTrust) Verify(_ context.Context, der []byte) (bool, error) {
	*t.log = append(*t.log, "trust.verify")
	return t.roots[string(der)], nil
}

func (t *recordingTrust) Remove(context.Context) error {
	if t.removeErr != nil {
		return t.removeErr
	}
	entry := "trust.remove"
	if _, err := os.Lstat(t.keyFile); err == nil {
		entry += " (key file present)"
	}
	*t.log = append(*t.log, entry)
	delete(t.roots, string(t.last))
	t.last = nil
	return nil
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The root is minted once and reused: a restart must hand proxy.tls and the trust store the root
// the device already trusts, not a new one every start.
func TestDeviceCAGeneratedOnceAndReused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), state.DeviceCADir)
	keys := fileKeyStore{path: filepath.Join(dir, deviceCAKeyFile)}
	var log []string
	trust := newRecordingTrust(&log, keys.path)
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

	ca, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now, trust)
	if err != nil || !created {
		t.Fatalf("first start: created=%v err=%v", created, err)
	}
	cert, key := readFile(t, filepath.Join(dir, deviceCACertFile)), readFile(t, keys.path)
	if _, err := NewCAFromPEM(cert, key, now); err != nil {
		t.Fatalf("the kept pair is not a usable interception CA: %v", err)
	}
	if !ca.cert.IsCA || ca.cert.NotAfter.Sub(now) < deviceCAValidity-2*time.Hour {
		t.Fatalf("root is CA=%v valid until %s; want a CA valid for %s", ca.cert.IsCA, ca.cert.NotAfter, deviceCAValidity)
	}
	if err := state.CheckFile(keys.path); err != nil {
		t.Fatalf("the key file is not protected: %v", err)
	}

	for i := 0; i < 2; i++ {
		again, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now.Add(time.Duration(i+1)*24*time.Hour), trust)
		if err != nil || created {
			t.Fatalf("restart %d: created=%v err=%v; the kept CA must be reused", i, created, err)
		}
		if again.Info().Fingerprint != ca.Info().Fingerprint || !bytes.Equal(key, readFile(t, keys.path)) {
			t.Fatalf("restart %d handed out a different root", i)
		}
	}
	if len(log) != 0 {
		t.Fatalf("a file-kept root touched the trust store: %v", log)
	}
}

// Renewal happens at a start within the margin, never by letting the root expire under live
// connections.
func TestDeviceCARenewedNearExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), state.DeviceCADir)
	keys := &fakeKeyStore{}
	var log []string
	trust := newRecordingTrust(&log, filepath.Join(dir, deviceCAKeyFile))
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	ca, _, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now, trust)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(deviceCAValidity - deviceCARenewBefore + time.Hour)
	renewed, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", later, trust)
	if err != nil || !created || renewed.Info().Fingerprint == ca.Info().Fingerprint {
		t.Fatalf("near expiry: created=%v err=%v; want a new root", created, err)
	}
	if keys.generated != 2 {
		t.Fatalf("the keystore generated %d keys, want 2", keys.generated)
	}
}

// A certificate whose key is missing or does not match is not this device's CA.
func TestDeviceCAReplacedWhenThePairIsBroken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), state.DeviceCADir)
	keys := fileKeyStore{path: filepath.Join(dir, deviceCAKeyFile)}
	var log []string
	trust := newRecordingTrust(&log, keys.path)
	now := time.Now()
	if _, _, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now, trust); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keys.path); err != nil {
		t.Fatal(err)
	}
	if _, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now, trust); err != nil || !created {
		t.Fatalf("missing key: created=%v err=%v; want a new pair", created, err)
	}

	// A keystore that lost its key, or holds another one, gets a new root too.
	ks := &fakeKeyStore{}
	ksDir := filepath.Join(t.TempDir(), state.DeviceCADir)
	if _, _, err := openDeviceCA(context.Background(), ks, ksDir, "DESKTOP-01", now, trust); err != nil {
		t.Fatal(err)
	}
	ks.key = nil
	if _, created, err := openDeviceCA(context.Background(), ks, ksDir, "DESKTOP-01", now, trust); err != nil || !created {
		t.Fatalf("lost keystore key: created=%v err=%v; want a new root", created, err)
	}
}

// fileKeyRoot leaves dir as the version that kept the key in a file left it.
func fileKeyRoot(t *testing.T, dir string, now time.Time) *CA {
	t.Helper()
	var log []string
	keyFile := filepath.Join(dir, deviceCAKeyFile)
	old, _, err := openDeviceCA(context.Background(), fileKeyStore{path: keyFile}, dir, "DESKTOP-01", now, newRecordingTrust(&log, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	return old
}

// The first start of a version whose key is in the keystore replaces a file-key root: a new root
// is minted in the keystore, the old root leaves the trust store, the key file is deleted, and the
// proxy's Start installs the new root. Later starts and policy toggles keep that key.
func TestFileKeyRootReplacedOnFirstStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), state.DeviceCADir)
	keyFile := filepath.Join(dir, deviceCAKeyFile)
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	old := fileKeyRoot(t, dir, now)

	var log []string
	trust := newRecordingTrust(&log, keyFile)
	_ = trust.Install(context.Background(), old.DER()) // the earlier version's root is trusted
	log = nil
	keys := &fakeKeyStore{log: &log}

	ca, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now.Add(time.Hour), trust)
	if err != nil || !created {
		t.Fatalf("first start of the keystore version: created=%v err=%v", created, err)
	}
	want := []string{"keystore.generate", "trust.verify", "trust.install", "trust.remove (key file present)"}
	if !slices.Equal(log, want) {
		t.Fatalf("steps = %q, want %q", log, want)
	}
	if trust.roots[string(old.DER())] {
		t.Fatal("the file-key root is still trusted")
	}
	if len(trust.roots) != 0 {
		t.Fatal("the replacement installed a root; installing is the proxy's Start")
	}
	if _, err := os.Lstat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the key file is still there: %v", err)
	}
	if ca.Info().Fingerprint == old.Info().Fingerprint {
		t.Fatal("the file-key root was kept")
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, deviceCACertFile)), ca.PEM()) {
		t.Fatal("the state directory does not hold the new root's certificate")
	}
	if !ca.cert.PublicKey.(*ecdsa.PublicKey).Equal(keys.key.Public()) {
		t.Fatal("the new root is not signed for by the keystore's key")
	}
	verifyLeaf(t, ca, now.Add(time.Hour))

	// The proxy installs the new root at each Start and removes it at each Stop; the key stays.
	p := newProviderForTest(t, Config{CA: ca, TrustRoot: trust})
	for round := 1; round <= 2; round++ {
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("round %d Start: %v", round, err)
		}
		if !trust.roots[string(ca.DER())] || p.CA() != ca {
			t.Fatalf("round %d: the new root is not installed and in use", round)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatalf("round %d Stop: %v", round, err)
		}
		if len(trust.roots) != 0 {
			t.Fatalf("round %d: a root is still trusted after Stop", round)
		}
	}

	again, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now.Add(2*time.Hour), trust)
	if err != nil || created || again.Info().Fingerprint != ca.Info().Fingerprint {
		t.Fatalf("restart: created=%v err=%v; the keystore root must be reused", created, err)
	}
	if keys.generated != 1 {
		t.Fatalf("the keystore generated %d keys across toggles and a restart, want 1", keys.generated)
	}
}

// A file-key root the trust store does not hold (TLS inspection was off) is never added to it on
// the way out.
func TestFileKeyRootNotTrustedIsNotInstalled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), state.DeviceCADir)
	keyFile := filepath.Join(dir, deviceCAKeyFile)
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	fileKeyRoot(t, dir, now)

	var log []string
	trust := newRecordingTrust(&log, keyFile)
	if _, created, err := openDeviceCA(context.Background(), &fakeKeyStore{}, dir, "DESKTOP-01", now, trust); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if !slices.Equal(log, []string{"trust.verify"}) || len(trust.roots) != 0 {
		t.Fatalf("trust steps = %q, roots %d; want only the check", log, len(trust.roots))
	}
	if _, err := os.Lstat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the key file is still there: %v", err)
	}
}

// A trust store that cannot remove the file-key root stops the replacement before the key file and
// the old certificate are gone, so the next start tries again.
func TestFileKeyRootReplacementRetriedWhenTrustRemovalFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), state.DeviceCADir)
	keyFile := filepath.Join(dir, deviceCAKeyFile)
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	old := fileKeyRoot(t, dir, now)

	var log []string
	trust := newRecordingTrust(&log, keyFile)
	_ = trust.Install(context.Background(), old.DER())
	trust.removeErr = errors.New("certutil failed")
	keys := &fakeKeyStore{}
	if _, _, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now, trust); err == nil {
		t.Fatal("the replacement succeeded with the old root still trusted")
	}
	if _, err := os.Lstat(keyFile); err != nil {
		t.Fatalf("the key file was deleted before the old root left the trust store: %v", err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, deviceCACertFile)), old.PEM()) {
		t.Fatal("the old certificate was replaced before the old root left the trust store")
	}

	trust.removeErr = nil
	ca, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-01", now, trust)
	if err != nil || !created || trust.roots[string(old.DER())] {
		t.Fatalf("retry: created=%v err=%v, old root trusted %v", created, err, trust.roots[string(old.DER())])
	}
	if !ca.cert.PublicKey.(*ecdsa.PublicKey).Equal(keys.key.Public()) {
		t.Fatal("the retried root is not signed for by the keystore's key")
	}
}
