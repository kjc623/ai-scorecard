package trust

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
)

// The manager must satisfy the two interfaces the rest of capture-core consumes: the
// supervisor's core.TrustRoot (Remove) and tlsproxy's Install type-assertion.
var (
	_ core.TrustRoot = (*Manager)(nil)
	_ interface {
		Install(context.Context, []byte) error
	} = (*Manager)(nil)
)

// fakeRunner records the exact argv of every call and scripts the output.
type fakeRunner struct {
	calls [][]string
	fn    func(name string, args []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.fn != nil {
		return f.fn(name, args)
	}
	return "", nil
}

func (f *fakeRunner) all() [][]string { return f.calls }
func (f *fakeRunner) last() []string  { return f.calls[len(f.calls)-1] }

func assertCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func assertSlice(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
}

func mustTestCert(t *testing.T, serial int64) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "sac-device-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return der
}

func wantSHA256(der []byte) string { s := sha256.Sum256(der); return hex.EncodeToString(s[:]) }
func wantSHA1(der []byte) string {
	s := sha1.Sum(der)
	return strings.ToUpper(hex.EncodeToString(s[:]))
}

func TestHostOS(t *testing.T) {
	var want OS
	switch runtime.GOOS {
	case "linux":
		want = OSLinux
	case "darwin":
		want = OSDarwin
	case "windows":
		want = OSWindows
	default:
		want = OS(runtime.GOOS)
	}
	if got := HostOS(); got != want {
		t.Fatalf("HostOS() = %q, want %q", got, want)
	}
}

func TestFingerprints(t *testing.T) {
	sha256hex, sha1hex := fingerprints([]byte("abc"))
	if sha256hex != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256 = %q", sha256hex)
	}
	// SHA-1 is uppercase, no separators: it is the cert identifier, not a security primitive.
	if sha1hex != "A9993E364706816ABA3E25717850C26C9CD0D89D" {
		t.Errorf("sha1 = %q", sha1hex)
	}
}

func TestInstallLinux(t *testing.T) {
	der := mustTestCert(t, 1)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := &fakeRunner{}
	m := New(Config{OS: OSLinux, Runner: r, CertDir: dir, Name: "ca"})
	ctx := context.Background()

	if err := m.Install(ctx, der); err != nil {
		t.Fatalf("install: %v", err)
	}

	// The PEM was written to <CertDir>/<Name>.crt.
	wantPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	gotFile, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("reading installed file: %v", err)
	}
	if !bytes.Equal(gotFile, wantPEM) {
		t.Fatalf("installed file content does not match the DER's PEM")
	}

	assertCalls(t, r.all(), [][]string{{"update-ca-certificates"}})

	info := m.Info()
	if info.Subject != cert.Subject.String() {
		t.Errorf("subject = %q, want %q", info.Subject, cert.Subject.String())
	}
	if info.FingerprintSHA256 != wantSHA256(der) {
		t.Errorf("sha256 = %q, want %q", info.FingerprintSHA256, wantSHA256(der))
	}
	if info.Path != filepath.Join(dir, "ca.crt") {
		t.Errorf("path = %q", info.Path)
	}

	// Idempotent: installing the same certificate again succeeds.
	if err := m.Install(ctx, der); err != nil {
		t.Fatalf("second install: %v", err)
	}
}

func TestInstallLinuxFallback(t *testing.T) {
	der := mustTestCert(t, 2)
	dir := t.TempDir()
	anchors := t.TempDir()
	old := linuxFallbackDir
	linuxFallbackDir = anchors
	defer func() { linuxFallbackDir = old }()

	r := &fakeRunner{fn: func(name string, args []string) (string, error) {
		switch name {
		case "update-ca-certificates":
			return "", errors.New("command not found")
		case "update-ca-trust":
			return "", nil
		}
		return "", nil
	}}
	m := New(Config{OS: OSLinux, Runner: r, CertDir: dir, Name: "ca"})
	if err := m.Install(context.Background(), der); err != nil {
		t.Fatalf("install: %v", err)
	}

	assertCalls(t, r.all(), [][]string{{"update-ca-certificates"}, {"update-ca-trust", "extract"}})

	if _, err := os.Stat(filepath.Join(anchors, "ca.crt")); err != nil {
		t.Fatalf("fallback file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("primary file should have been removed, got %v", err)
	}
	if got := m.Info().Path; got != filepath.Join(anchors, "ca.crt") {
		t.Errorf("path = %q, want the anchors path", got)
	}
}

func TestVerifyLinux(t *testing.T) {
	der := mustTestCert(t, 3)
	dir := t.TempDir()
	m := New(Config{OS: OSLinux, Runner: &fakeRunner{}, CertDir: dir, Name: "ca"})
	ctx := context.Background()

	if ok, err := m.Verify(ctx, der); err != nil || ok {
		t.Fatalf("verify before install = %v, %v; want false, nil", ok, err)
	}
	if err := m.Install(ctx, der); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.Verify(ctx, der); err != nil || !ok {
		t.Fatalf("verify after install = %v, %v; want true, nil", ok, err)
	}

	other := mustTestCert(t, 4)
	if bytes.Equal(other, der) {
		t.Fatal("test certificates unexpectedly identical")
	}
	if ok, err := m.Verify(ctx, other); err != nil || ok {
		t.Fatalf("verify different cert = %v, %v; want false, nil", ok, err)
	}
}

func TestVerifyLinuxUnusableStore(t *testing.T) {
	der := mustTestCert(t, 5)
	// A file where a directory is expected makes the store unreadable, which must be an error,
	// not a silent "not installed".
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Config{OS: OSLinux, CertDir: notADir, Name: "ca"})
	if _, err := m.Verify(context.Background(), der); err == nil {
		t.Fatal("verify against an unusable store must return an error")
	}
}

func TestInstallDarwin(t *testing.T) {
	der := mustTestCert(t, 6)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keychain := "/tmp/test.keychain"

	var stagedPath string
	var stagedContent []byte
	r := &fakeRunner{fn: func(name string, args []string) (string, error) {
		if name == "security" && args[0] == "add-trusted-cert" {
			stagedPath = args[len(args)-1]
			b, err := os.ReadFile(stagedPath)
			if err != nil {
				return "", err
			}
			stagedContent = b
		}
		return "", nil
	}}
	m := New(Config{OS: OSDarwin, Runner: r, TempDir: dir, Keychain: keychain})
	if err := m.Install(context.Background(), der); err != nil {
		t.Fatalf("install: %v", err)
	}

	assertSlice(t, r.last(), []string{"security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", keychain, stagedPath})

	if !strings.HasPrefix(stagedPath, dir) {
		t.Errorf("staged path %q not inside TempDir %q", stagedPath, dir)
	}
	wantPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if !bytes.Equal(stagedContent, wantPEM) {
		t.Errorf("darwin stages PEM, got %d bytes", len(stagedContent))
	}
	if _, err := os.Stat(stagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged file was not cleaned up: %v", err)
	}

	info := m.Info()
	if info.Path != keychain {
		t.Errorf("path = %q, want keychain %q", info.Path, keychain)
	}
	if info.Subject != cert.Subject.String() {
		t.Errorf("subject = %q", info.Subject)
	}
}

func TestVerifyDarwin(t *testing.T) {
	der := mustTestCert(t, 7)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	sha1hex := wantSHA1(der)
	keychain := "/tmp/test.keychain"
	wantArgv := []string{"security", "find-certificate", "-a", "-c", cert.Subject.CommonName, "-Z", keychain}

	t.Run("present", func(t *testing.T) {
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "SHA-1 hash: " + sha1hex, nil }}
		m := New(Config{OS: OSDarwin, Runner: r, Keychain: keychain})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || !ok {
			t.Fatalf("verify = %v, %v; want true, nil", ok, err)
		}
		assertSlice(t, r.last(), wantArgv)
	})
	t.Run("absent", func(t *testing.T) {
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "", nil }}
		m := New(Config{OS: OSDarwin, Runner: r, Keychain: keychain})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || ok {
			t.Fatalf("verify = %v, %v; want false, nil", ok, err)
		}
	})
	t.Run("query-error-is-absence", func(t *testing.T) {
		// security exits non-zero when nothing matches; a failed query is "absent", not a
		// store error, otherwise the post-removal check would fail after a successful removal.
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "", errors.New("item not found") }}
		m := New(Config{OS: OSDarwin, Runner: r, Keychain: keychain})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || ok {
			t.Fatalf("verify = %v, %v; want false, nil", ok, err)
		}
	})
}

func TestInstallWindows(t *testing.T) {
	der := mustTestCert(t, 8)
	dir := t.TempDir()

	var stagedPath string
	var stagedContent []byte
	r := &fakeRunner{fn: func(name string, args []string) (string, error) {
		if name == "certutil" && args[0] == "-addstore" {
			stagedPath = args[len(args)-1]
			b, err := os.ReadFile(stagedPath)
			if err != nil {
				return "", err
			}
			stagedContent = b
		}
		return "", nil
	}}
	m := New(Config{OS: OSWindows, Runner: r, TempDir: dir})
	if err := m.Install(context.Background(), der); err != nil {
		t.Fatalf("install: %v", err)
	}

	assertSlice(t, r.last(), []string{"certutil", "-addstore", "-f", "Root", stagedPath})
	if !strings.HasPrefix(stagedPath, dir) {
		t.Errorf("staged path %q not inside TempDir %q", stagedPath, dir)
	}
	if !bytes.Equal(stagedContent, der) {
		t.Errorf("windows stages DER, got %d bytes", len(stagedContent))
	}
	if _, err := os.Stat(stagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged file was not cleaned up: %v", err)
	}
}

func TestInstallWindowsEnterprise(t *testing.T) {
	der := mustTestCert(t, 9)
	dir := t.TempDir()
	r := &fakeRunner{}
	m := New(Config{OS: OSWindows, Runner: r, TempDir: dir, Store: "enterprise"})
	if err := m.Install(context.Background(), der); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := r.last()
	// got = [certutil -addstore -f -enterprise Root <tmp>]
	want := []string{"certutil", "-addstore", "-f", "-enterprise", "Root", got[len(got)-1]}
	assertSlice(t, got, want)
}

func TestVerifyWindows(t *testing.T) {
	der := mustTestCert(t, 10)
	sha1hex := wantSHA1(der)
	wantArgv := []string{"certutil", "-store", "Root", sha1hex}

	t.Run("present", func(t *testing.T) {
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "Thumbprint: " + sha1hex, nil }}
		m := New(Config{OS: OSWindows, Runner: r})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || !ok {
			t.Fatalf("verify = %v, %v; want true, nil", ok, err)
		}
		assertSlice(t, r.last(), wantArgv)
	})
	t.Run("present-space-separated", func(t *testing.T) {
		// certutil prints a thumbprint spaced ("e6 c4 aa 7a ...") as well as contiguous; a raw
		// substring match read that as absent and made health permanently degraded.
		spaced := strings.Join(strings.Split(sha1hex, ""), " ")
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "Cert Hash(sha1): " + spaced, nil }}
		m := New(Config{OS: OSWindows, Runner: r})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || !ok {
			t.Fatalf("verify = %v, %v; want true, nil for space-separated output", ok, err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "", nil }}
		m := New(Config{OS: OSWindows, Runner: r})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || ok {
			t.Fatalf("verify = %v, %v; want false, nil", ok, err)
		}
	})
	t.Run("query-error-is-absence", func(t *testing.T) {
		// certutil exits non-zero when nothing matches; a failed query is "absent", not a
		// store error, otherwise the post-removal check would fail after a successful removal.
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "", errors.New("cert not found") }}
		m := New(Config{OS: OSWindows, Runner: r})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || ok {
			t.Fatalf("verify = %v, %v; want false, nil", ok, err)
		}
	})
	t.Run("different-fingerprint-is-absent", func(t *testing.T) {
		other := strings.Repeat("AB", 20)
		if other == sha1hex {
			t.Fatal("test fixture collides with the certificate's own fingerprint")
		}
		r := &fakeRunner{fn: func(string, []string) (string, error) { return "Cert Hash(sha1): " + other, nil }}
		m := New(Config{OS: OSWindows, Runner: r})
		ok, err := m.Verify(context.Background(), der)
		if err != nil || ok {
			t.Fatalf("verify = %v, %v; want false, nil for a different certificate", ok, err)
		}
	})
}

func TestRemoveLinux(t *testing.T) {
	der := mustTestCert(t, 11)
	dir := t.TempDir()
	r := &fakeRunner{}
	m := New(Config{OS: OSLinux, Runner: r, CertDir: dir, Name: "ca"})
	ctx := context.Background()
	if err := m.Install(ctx, der); err != nil {
		t.Fatal(err)
	}

	r.calls = nil
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("installed file was not deleted: %v", err)
	}
	// Removal re-runs the update so the store reflects the deletion.
	assertCalls(t, r.all(), [][]string{{"update-ca-certificates"}})
	if got := m.Info(); got != (Info{}) {
		t.Errorf("info not cleared: %+v", got)
	}
}

func TestRemoveNothingInstalled(t *testing.T) {
	for _, os := range []OS{OSLinux, OSDarwin, OSWindows} {
		m := New(Config{OS: os, Runner: &fakeRunner{}, CertDir: t.TempDir(), TempDir: t.TempDir()})
		if err := m.Remove(context.Background()); err == nil {
			t.Errorf("%s: removing with nothing installed should error", os)
		}
	}
}

func TestRemoveDarwin(t *testing.T) {
	der := mustTestCert(t, 12)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	sha1hex := wantSHA1(der)
	keychain := "/tmp/test.keychain"

	r := &fakeRunner{fn: func(name string, args []string) (string, error) {
		// find-certificate returns nothing after deletion, so removal verifies.
		return "", nil
	}}
	m := New(Config{OS: OSDarwin, Runner: r, TempDir: t.TempDir(), Keychain: keychain})
	ctx := context.Background()
	if err := m.Install(ctx, der); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// The trust setting is removed first (add-trusted-cert's inverse), then the keychain item,
	// then the absence check. The removal temp is staged and cleaned.
	var removedPath string
	for _, c := range r.all() {
		if len(c) >= 2 && c[1] == "remove-trusted-cert" {
			removedPath = c[len(c)-1]
		}
	}
	if removedPath == "" {
		t.Fatal("remove-trusted-cert was never invoked; the admin trust setting would be left behind")
	}
	if _, err := os.Stat(removedPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removal temp was not cleaned up: %v", err)
	}
	assertCalls(t, r.all(), [][]string{
		{"security", "remove-trusted-cert", "-d", removedPath},
		{"security", "delete-certificate", "-Z", sha1hex, keychain},
		{"security", "find-certificate", "-a", "-c", cert.Subject.CommonName, "-Z", keychain},
	})
}

func TestRemoveWindows(t *testing.T) {
	der := mustTestCert(t, 13)
	sha1hex := wantSHA1(der)

	r := &fakeRunner{fn: func(string, []string) (string, error) { return "", nil }}
	m := New(Config{OS: OSWindows, Runner: r, TempDir: t.TempDir()})
	ctx := context.Background()
	if err := m.Install(ctx, der); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}
	assertCalls(t, r.all(), [][]string{
		{"certutil", "-delstore", "Root", sha1hex},
		{"certutil", "-store", "Root", sha1hex},
	})
}

func TestRemoveVerificationFails(t *testing.T) {
	der := mustTestCert(t, 14)
	sha1hex := wantSHA1(der)
	// The store still reports the certificate after deletion: removal must fail, not claim success.
	r := &fakeRunner{fn: func(string, []string) (string, error) { return "Thumbprint: " + sha1hex, nil }}
	m := New(Config{OS: OSWindows, Runner: r, TempDir: t.TempDir()})
	ctx := context.Background()
	if err := m.Install(ctx, der); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx); err == nil {
		t.Fatal("remove should fail when the certificate is still present")
	}
}

func TestUnknownOSFailsClosed(t *testing.T) {
	der := mustTestCert(t, 15)
	m := New(Config{OS: OS("plan9"), Runner: &fakeRunner{}, CertDir: t.TempDir(), TempDir: t.TempDir()})

	if err := m.Install(context.Background(), der); err == nil {
		t.Error("install on an unknown OS should fail")
	}
	if ok, err := m.Verify(context.Background(), der); err == nil || ok {
		t.Errorf("verify on an unknown OS = %v, %v; want an error", ok, err)
	}
	if err := m.Remove(context.Background()); err == nil {
		t.Error("remove on an unknown OS should fail")
	}
}

func TestDescribe(t *testing.T) {
	der := mustTestCert(t, 16)
	m := New(Config{OS: OSLinux, Runner: &fakeRunner{}, CertDir: t.TempDir(), Name: "ca"})
	if got := m.Describe(); !strings.Contains(got, "not installed") {
		t.Errorf("Describe before install = %q", got)
	}
	if err := m.Install(context.Background(), der); err != nil {
		t.Fatal(err)
	}
	got := m.Describe()
	if !strings.Contains(got, string(OSLinux)) || !strings.Contains(got, wantSHA256(der)) {
		t.Errorf("Describe after install = %q", got)
	}
}
