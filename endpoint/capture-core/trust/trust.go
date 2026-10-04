// Package trust installs, verifies and removes the per-device root CA in the operating system's
// real trust store (docs/01-collectors.md §4.5, §5.2, §14). It is the platform half of "the
// device trusts the CA that intercepts its traffic", and it exists because writing the CA to the
// wrong store fails silently: the platform honours exactly one store, and the manager must choose
// the one the platform actually reads.
//
// Behaviour is selected by Config.OS, not build tags, so a Linux test can exercise the macOS and
// Windows command construction with a fake Runner. A Manager whose OS value is not one of the
// three supported values fails closed: every operation returns an error rather than guessing.
package trust

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// OS names a target operating system. It is a value so tests can force a platform.
type OS string

const (
	OSLinux   OS = "linux"
	OSDarwin  OS = "darwin"
	OSWindows OS = "windows"
)

const (
	// defaultName is the base file name (without extension) of the CA on Linux.
	defaultName = "sac-device-ca"
	// defaultCertDir is the Debian/Ubuntu CA directory, where update-ca-certificates reads.
	defaultCertDir = "/usr/local/share/ca-certificates"
	// defaultKeychain is the macOS system keychain.
	defaultKeychain = "/Library/Keychains/System.keychain"
	// defaultStore is the Windows store a root CA is added to.
	defaultStore = "Root"
)

// linuxFallbackDir is the RHEL/Fedora anchor directory used when update-ca-certificates is
// absent or fails. It is a var (not const) so an in-package test can point it at a temp dir
// instead of writing to /etc on the host running the tests.
var linuxFallbackDir = "/etc/pki/ca-trust/source/anchors"

// HostOS reports the operating system this binary runs on.
func HostOS() OS {
	switch runtime.GOOS {
	case "linux":
		return OSLinux
	case "darwin":
		return OSDarwin
	case "windows":
		return OSWindows
	default:
		return OS(runtime.GOOS)
	}
}

// Runner executes one command. It is a seam so a test can assert the exact argv and script the
// output without touching the real OS trust store.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// ExecRunner runs commands with os/exec.CommandContext.
type ExecRunner struct{}

// Run runs name with args and returns its stdout.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return string(out), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// Config carries the platform seams. All paths and commands are configurable so a test can
// redirect them away from the host's real trust store.
type Config struct {
	OS       OS
	Runner   Runner
	CertDir  string // Linux: where update-ca-certificates reads (default /usr/local/share/ca-certificates).
	TempDir  string // darwin/windows: where the staged PEM/DER is written.
	Name     string // Linux: the CA file base name, without extension.
	Store    string // Windows: "Root" (default) or "enterprise".
	Keychain string // darwin: the keychain path (default /Library/Keychains/System.keychain).
	Logf     func(string, ...any)
}

// Info describes the last certificate this manager installed.
type Info struct {
	Subject           string
	FingerprintSHA256 string
	Path              string // Linux: the .crt file; darwin: the keychain; windows: "" (no filesystem path).
}

// Manager installs/verifies/removes one root CA. It remembers the last installed certificate so
// Remove needs no argument (and satisfies core.TrustRoot).
type Manager struct {
	cfg Config

	mu            sync.Mutex
	der           []byte
	subject       string
	cn            string
	sha256        string // lowercase hex
	sha1          string // uppercase hex, no separators
	path          string
	linuxFallback bool // Linux: whether the cert went to the pki ca-trust anchors instead of CertDir.
}

// New returns a Manager for cfg, applying the platform defaults for any field left zero.
func New(cfg Config) *Manager {
	if cfg.OS == "" {
		cfg.OS = HostOS()
	}
	if cfg.Runner == nil {
		cfg.Runner = ExecRunner{}
	}
	if cfg.Name == "" {
		cfg.Name = defaultName
	}
	if cfg.CertDir == "" {
		cfg.CertDir = defaultCertDir
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	if cfg.Store == "" {
		cfg.Store = defaultStore
	}
	if cfg.Keychain == "" {
		cfg.Keychain = defaultKeychain
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Manager{cfg: cfg}
}

// Install adds certDER to the platform trust store. It is idempotent: installing the same
// certificate again succeeds and leaves the same end state.
func (m *Manager) Install(ctx context.Context, certDER []byte) error {
	if len(certDER) == 0 {
		return fmt.Errorf("trust: %s: empty certificate", m.cfg.OS)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("trust: %s: parsing certificate: %w", m.cfg.OS, err)
	}
	sha256hex, sha1hex := fingerprints(certDER)

	switch m.cfg.OS {
	case OSLinux:
		return m.installLinux(ctx, certDER, cert, sha256hex, sha1hex)
	case OSDarwin:
		return m.installDarwin(ctx, certDER, cert, sha256hex, sha1hex)
	case OSWindows:
		return m.installWindows(ctx, certDER, cert, sha256hex, sha1hex)
	default:
		return fmt.Errorf("trust: unsupported operating system %q", m.cfg.OS)
	}
}

func (m *Manager) installLinux(ctx context.Context, certDER []byte, cert *x509.Certificate, sha256hex, sha1hex string) error {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	primary := filepath.Join(m.cfg.CertDir, m.cfg.Name+".crt")
	if err := os.MkdirAll(m.cfg.CertDir, 0o755); err != nil {
		return fmt.Errorf("trust: linux: creating ca-certificates directory %s: %w", m.cfg.CertDir, err)
	}
	if err := writeFile(primary, pemBytes); err != nil {
		return fmt.Errorf("trust: linux: writing %s to the ca-certificates store (%s): %w", primary, m.cfg.CertDir, err)
	}
	if _, err := m.runner().Run(ctx, "update-ca-certificates"); err == nil {
		m.setInstalled(certDER, cert, sha256hex, sha1hex, primary, false)
		m.cfg.Logf("trust: installed %s root CA at %s", m.cfg.OS, primary)
		return nil
	} else {
		// The Debian/Ubuntu tool is absent or failed; fall back to the RHEL/Fedora store.
		_ = os.Remove(primary) // best-effort: do not leave a dangling file in a store we do not use
		fallback := filepath.Join(linuxFallbackDir, m.cfg.Name+".crt")
		if mkErr := os.MkdirAll(linuxFallbackDir, 0o755); mkErr != nil {
			return fmt.Errorf("trust: linux: update-ca-certificates failed (%v), and creating pki ca-trust anchors directory %s: %w", err, linuxFallbackDir, mkErr)
		}
		if wErr := writeFile(fallback, pemBytes); wErr != nil {
			return fmt.Errorf("trust: linux: update-ca-certificates failed (%v), and writing %s to the pki ca-trust store (%s): %w", err, fallback, linuxFallbackDir, wErr)
		}
		if _, exErr := m.runner().Run(ctx, "update-ca-trust", "extract"); exErr != nil {
			return fmt.Errorf("trust: linux: update-ca-certificates failed (%v), and update-ca-trust extract in the pki ca-trust store failed: %w", err, exErr)
		}
		m.setInstalled(certDER, cert, sha256hex, sha1hex, fallback, true)
		m.cfg.Logf("trust: installed %s root CA at %s (pki ca-trust)", m.cfg.OS, fallback)
		return nil
	}
}

func (m *Manager) installDarwin(ctx context.Context, certDER []byte, cert *x509.Certificate, sha256hex, sha1hex string) error {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	tmp, cleanup, err := stageTemp(m.cfg.TempDir, "sac-ca-*.pem", pemBytes)
	if err != nil {
		return fmt.Errorf("trust: darwin: staging certificate to %s: %w", m.cfg.TempDir, err)
	}
	defer cleanup()
	if _, err := m.runner().Run(ctx, "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", m.cfg.Keychain, tmp); err != nil {
		return fmt.Errorf("trust: darwin: adding to keychain %s: %w", m.cfg.Keychain, err)
	}
	m.setInstalled(certDER, cert, sha256hex, sha1hex, m.cfg.Keychain, false)
	m.cfg.Logf("trust: installed %s root CA in keychain %s", m.cfg.OS, m.cfg.Keychain)
	return nil
}

func (m *Manager) installWindows(ctx context.Context, certDER []byte, cert *x509.Certificate, sha256hex, sha1hex string) error {
	tmp, cleanup, err := stageTemp(m.cfg.TempDir, "sac-ca-*.cer", certDER)
	if err != nil {
		return fmt.Errorf("trust: windows: staging certificate to %s: %w", m.cfg.TempDir, err)
	}
	defer cleanup()
	if _, err := m.runner().Run(ctx, "certutil", m.addstoreArgs(tmp)...); err != nil {
		return fmt.Errorf("trust: windows: adding to the %s store: %w", m.windowsStore(), err)
	}
	m.setInstalled(certDER, cert, sha256hex, sha1hex, "", false)
	m.cfg.Logf("trust: installed %s root CA in the %s store", m.cfg.OS, m.windowsStore())
	return nil
}

// Verify reports whether certDER is present in the platform trust store. It never trusts the
// runner's exit code alone: Linux reads the installed file back and compares bytes; darwin and
// Windows require the runner's output to confirm the fingerprint. It returns an error (rather
// than just false) when the store cannot be consulted at all.
func (m *Manager) Verify(ctx context.Context, certDER []byte) (bool, error) {
	switch m.cfg.OS {
	case OSLinux:
		return m.verifyLinux(certDER)
	case OSDarwin:
		return m.verifyDarwin(ctx, certDER)
	case OSWindows:
		return m.verifyWindows(ctx, certDER)
	default:
		return false, fmt.Errorf("trust: unsupported operating system %q", m.cfg.OS)
	}
}

func (m *Manager) verifyLinux(certDER []byte) (bool, error) {
	want := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	for _, p := range m.linuxCertPaths() {
		data, err := os.ReadFile(p)
		switch {
		case err == nil && bytes.Equal(data, want):
			return true, nil
		case err == nil:
			// A different certificate under our name: not this one.
			continue
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return false, fmt.Errorf("trust: linux: reading %s from the trust store: %w", p, err)
		}
	}
	return false, nil
}

func (m *Manager) verifyDarwin(ctx context.Context, certDER []byte) (bool, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return false, fmt.Errorf("trust: darwin: parsing certificate: %w", err)
	}
	_, sha1hex := fingerprints(certDER)
	out, err := m.runner().Run(ctx, "security", "find-certificate", "-a", "-c", cert.Subject.CommonName, "-Z", m.cfg.Keychain)
	if err != nil {
		return false, fmt.Errorf("trust: darwin: querying keychain %s: %w", m.cfg.Keychain, err)
	}
	return strings.Contains(strings.ToUpper(out), sha1hex), nil
}

func (m *Manager) verifyWindows(ctx context.Context, certDER []byte) (bool, error) {
	_, sha1hex := fingerprints(certDER)
	out, err := m.runner().Run(ctx, "certutil", m.storeArgs("-store", sha1hex)...)
	if err != nil {
		return false, fmt.Errorf("trust: windows: querying the %s store: %w", m.windowsStore(), err)
	}
	return strings.Contains(strings.ToUpper(out), sha1hex), nil
}

// Remove removes the last installed root CA. It returns an error when nothing was installed or
// when the removal could not be verified.
func (m *Manager) Remove(ctx context.Context) error {
	switch m.cfg.OS {
	case OSLinux:
		return m.removeLinux(ctx)
	case OSDarwin:
		return m.removeDarwin(ctx)
	case OSWindows:
		return m.removeWindows(ctx)
	default:
		return fmt.Errorf("trust: unsupported operating system %q", m.cfg.OS)
	}
}

func (m *Manager) removeLinux(ctx context.Context) error {
	m.mu.Lock()
	if len(m.der) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("trust: linux: nothing installed to remove")
	}
	path, fallback := m.path, m.linuxFallback
	m.mu.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("trust: linux: removing %s from the %s store: %w", path, m.linuxStoreName(fallback), err)
	}
	if fallback {
		if _, err := m.runner().Run(ctx, "update-ca-trust", "extract"); err != nil {
			return fmt.Errorf("trust: linux: update-ca-trust extract in the pki ca-trust store failed: %w", err)
		}
	} else {
		if _, err := m.runner().Run(ctx, "update-ca-certificates"); err != nil {
			return fmt.Errorf("trust: linux: update-ca-certificates failed: %w", err)
		}
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("trust: linux: %s is still present after removal", path)
	}
	m.clear()
	m.cfg.Logf("trust: removed %s root CA", m.cfg.OS)
	return nil
}

func (m *Manager) removeDarwin(ctx context.Context) error {
	m.mu.Lock()
	if len(m.der) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("trust: darwin: nothing installed to remove")
	}
	sha1hex, cn, keychain := m.sha1, m.cn, m.cfg.Keychain
	m.mu.Unlock()

	if _, err := m.runner().Run(ctx, "security", "delete-certificate", "-Z", sha1hex, keychain); err != nil {
		return fmt.Errorf("trust: darwin: deleting certificate %s from keychain %s: %w", sha1hex, keychain, err)
	}
	out, err := m.runner().Run(ctx, "security", "find-certificate", "-a", "-c", cn, "-Z", keychain)
	if err != nil {
		return fmt.Errorf("trust: darwin: verifying removal from keychain %s: %w", keychain, err)
	}
	if strings.Contains(strings.ToUpper(out), sha1hex) {
		return fmt.Errorf("trust: darwin: certificate %s still present in keychain %s after removal", sha1hex, keychain)
	}
	m.clear()
	m.cfg.Logf("trust: removed %s root CA", m.cfg.OS)
	return nil
}

func (m *Manager) removeWindows(ctx context.Context) error {
	m.mu.Lock()
	if len(m.der) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("trust: windows: nothing installed to remove")
	}
	sha1hex := m.sha1
	m.mu.Unlock()

	if _, err := m.runner().Run(ctx, "certutil", m.storeArgs("-delstore", sha1hex)...); err != nil {
		return fmt.Errorf("trust: windows: deleting certificate %s from the %s store: %w", sha1hex, m.windowsStore(), err)
	}
	out, err := m.runner().Run(ctx, "certutil", m.storeArgs("-store", sha1hex)...)
	if err != nil {
		return fmt.Errorf("trust: windows: verifying removal from the %s store: %w", m.windowsStore(), err)
	}
	if strings.Contains(strings.ToUpper(out), sha1hex) {
		return fmt.Errorf("trust: windows: certificate %s still present in the %s store after removal", sha1hex, m.windowsStore())
	}
	m.clear()
	m.cfg.Logf("trust: removed %s root CA", m.cfg.OS)
	return nil
}

// Info describes the last installed certificate, or a zero Info when none is installed.
func (m *Manager) Info() Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.der) == 0 {
		return Info{}
	}
	return Info{Subject: m.subject, FingerprintSHA256: m.sha256, Path: m.path}
}

// Describe renders the manager's state for a log line or a health detail.
func (m *Manager) Describe() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.der) == 0 {
		return fmt.Sprintf("trust: %s root CA: not installed", m.cfg.OS)
	}
	return fmt.Sprintf("trust: %s root CA %q sha256=%s at %s", m.cfg.OS, m.subject, m.sha256, m.location())
}

// ---------------------------------------------------------------------------------------------
// Small helpers.

func (m *Manager) runner() Runner { return m.cfg.Runner }

func (m *Manager) setInstalled(der []byte, cert *x509.Certificate, sha256hex, sha1hex, path string, fallback bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.der = append(m.der[:0], der...)
	m.subject = cert.Subject.String()
	m.cn = cert.Subject.CommonName
	m.sha256 = sha256hex
	m.sha1 = sha1hex
	m.path = path
	m.linuxFallback = fallback
}

func (m *Manager) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.der = nil
	m.subject, m.cn, m.sha256, m.sha1, m.path = "", "", "", "", ""
	m.linuxFallback = false
}

func (m *Manager) location() string {
	if m.cfg.OS == OSWindows {
		return m.windowsStore()
	}
	return m.path
}

func (m *Manager) linuxCertPaths() []string {
	name := m.cfg.Name + ".crt"
	return []string{filepath.Join(m.cfg.CertDir, name), filepath.Join(linuxFallbackDir, name)}
}

func (m *Manager) linuxStoreName(fallback bool) string {
	if fallback {
		return "pki ca-trust"
	}
	return "ca-certificates"
}

// windowsStore is the certutil store this manager targets: the machine Root store, or the
// enterprise Root store when Store == "enterprise".
func (m *Manager) windowsStore() string {
	if m.cfg.Store == "enterprise" {
		return "enterprise Root"
	}
	return "Root"
}

func (m *Manager) windowsEnterprise() bool { return m.cfg.Store == "enterprise" }

// addstoreArgs builds `certutil -addstore -f [-enterprise] Root <file>`.
func (m *Manager) addstoreArgs(file string) []string {
	args := []string{"-addstore", "-f"}
	if m.windowsEnterprise() {
		args = append(args, "-enterprise")
	}
	return append(args, "Root", file)
}

// storeArgs builds `certutil <verb> [-enterprise] Root <thumbprint>` for -store and -delstore.
func (m *Manager) storeArgs(verb, thumbprint string) []string {
	args := []string{verb}
	if m.windowsEnterprise() {
		args = append(args, "-enterprise")
	}
	return append(args, "Root", thumbprint)
}

func fingerprints(der []byte) (sha256hex, sha1hex string) {
	s256 := sha256.Sum256(der)
	s1 := sha1.Sum(der)
	// SHA-1 here is not used for security: it is the certificate identifier that macOS `security`
	// and Windows `certutil` use to address a cert in the store.
	return hex.EncodeToString(s256[:]), strings.ToUpper(hex.EncodeToString(s1[:]))
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// stageTemp writes data to a new temp file under dir and returns its path plus a cleanup func.
func stageTemp(dir, pattern string, data []byte) (string, func(), error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", nil, err
	}
	return name, func() { _ = os.Remove(name) }, nil
}
