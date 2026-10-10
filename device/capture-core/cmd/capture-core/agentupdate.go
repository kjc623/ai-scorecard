package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// The agent updates itself from the deployment it is enrolled with. It asks for the release
// statement, verifies it under the pinned release key (the classifier release's), and when it names
// a newer version for this platform, downloads the package, checks it against the statement, writes
// the tenant file beside it as the MDM's package has it, and starts the platform installer, which
// replaces the agent and restarts the service. Nothing here can move the agent to an older or
// unsigned version: an unverified statement, an older version or a package that does not match is
// refused and reported.

const (
	// agentUpdateInterval is how often the agent asks: a release reaches the fleet within it.
	agentUpdateInterval = 15 * time.Minute
	// agentUpdateFirst is the first check after the service starts, so a just-updated agent settles
	// before it looks again.
	agentUpdateFirst = 2 * time.Minute
	// agentUpdateRetry is how long a version whose install did not take is left before it is tried
	// again, so a package that fails to install is not re-run every interval.
	agentUpdateRetry = 6 * time.Hour
	// maxAgentPackageBytes bounds a package the statement may name.
	maxAgentPackageBytes = 512 << 20
	// agentDownloadAttempts bounds the resumed requests one check makes.
	agentDownloadAttempts = 20
	// updateDir is the updater's folder under the state directory.
	updateDir = "update"
	// tenantPackageFile is the tenant file's name beside a package, where the installer reads it,
	// and tenantConfigFile the installed copy the service reads.
	tenantPackageFile = "ShadowAICapture.tenant.env"
	tenantConfigFile  = "tenant.env"
)

var agentFileRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.msi$`)

// verifyAgentRelease checks a statement's signature under pub and returns its payload. The
// signature covers the payload bytes as received; the payload is decoded strictly.
func verifyAgentRelease(raw []byte, pub ed25519.PublicKey) (protocol.AgentRelease, error) {
	var signed protocol.SignedAgentRelease
	if err := json.Unmarshal(raw, &signed); err != nil {
		return protocol.AgentRelease{}, fmt.Errorf("the agent release is not JSON: %w", err)
	}
	if !strings.EqualFold(signed.Algorithm, "ed25519") {
		return protocol.AgentRelease{}, fmt.Errorf("the agent release names algorithm %q", signed.Algorithm)
	}
	sig, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || len(signed.Payload) == 0 || !ed25519.Verify(pub, signed.Payload, sig) {
		return protocol.AgentRelease{}, errors.New("the agent release's signature does not verify under the pinned release key")
	}
	dec := json.NewDecoder(bytes.NewReader(signed.Payload))
	dec.DisallowUnknownFields()
	var st protocol.AgentRelease
	if err := dec.Decode(&st); err != nil {
		return protocol.AgentRelease{}, fmt.Errorf("the agent release payload: %w", err)
	}
	switch {
	case st.Type != protocol.AgentReleaseType:
		return st, fmt.Errorf("the signed payload is a %q, not an agent release", st.Type)
	case !agentFileRE.MatchString(st.File):
		return st, fmt.Errorf("the agent release names the file %q", st.File)
	case st.Size <= 0 || st.Size > maxAgentPackageBytes:
		return st, fmt.Errorf("the agent release names a size of %d bytes", st.Size)
	}
	if b, err := hex.DecodeString(st.SHA256); err != nil || len(b) != sha256.Size {
		return st, fmt.Errorf("the agent release names the sha256 %q", st.SHA256)
	}
	if _, ok := parseAgentVersion(st.Version); !ok {
		return st, fmt.Errorf("the agent release names the version %q", st.Version)
	}
	return st, nil
}

// parseAgentVersion reads a dotted numeric version such as 1.0.42.
func parseAgentVersion(v string) ([]int, bool) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || len(p) > 9 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

// agentVersionNewer reports whether candidate is a later version than current. A current version
// that is not a release version (a development build) is never replaced.
func agentVersionNewer(candidate, current string) bool {
	c, ok1 := parseAgentVersion(candidate)
	f, ok2 := parseAgentVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < len(c) || i < len(f); i++ {
		var a, b int
		if i < len(c) {
			a = c[i]
		}
		if i < len(f) {
			b = f[i]
		}
		if a != b {
			return a > b
		}
	}
	return false
}

// agentSource is the drain's half of the exchange, behind a seam for tests.
type agentSource interface {
	FetchAgentRelease(ctx context.Context) ([]byte, error)
	DownloadAgentPackage(ctx context.Context, w io.Writer, offset, limit int64) (int64, error)
}

// agentInstaller starts the platform installer for the package at path, logging to logPath, and
// returns without waiting: the installer stops this service.
type agentInstaller func(path, logPath string) error

// agentUpdateStatus is the updater's state in the health document.
type agentUpdateStatus struct {
	LastCheckAt    *time.Time `json:"last_check_at,omitempty"`
	LastAnswer     string     `json:"last_answer,omitempty"` // current | no_release | installing | retry_later | error
	OfferedVersion string     `json:"offered_version,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	InstallStarted *time.Time `json:"install_started_at,omitempty"`
	InstallerLog   string     `json:"installer_log,omitempty"`
	NextCheckAt    *time.Time `json:"next_check_at,omitempty"`
}

// agentAttempt records the last install started, so a failed one is not re-run every interval.
type agentAttempt struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

type agentUpdater struct {
	source   agentSource
	pub      ed25519.PublicKey
	current  string
	platform string
	dir      string
	// tenantFile is the installed tenant file, copied beside the package for the installer.
	tenantFile string
	install    agentInstaller
	log        *slog.Logger
	clock      func() time.Time

	mu     sync.Mutex
	status agentUpdateStatus

	stop chan struct{}
	wg   sync.WaitGroup
}

func newAgentUpdater(source agentSource, pub ed25519.PublicKey, current, platform, dir, tenantFile string, install agentInstaller, log *slog.Logger) *agentUpdater {
	return &agentUpdater{source: source, pub: pub, current: current, platform: platform, dir: dir, tenantFile: tenantFile,
		install: install, log: log, clock: time.Now, stop: make(chan struct{})}
}

func (u *agentUpdater) attemptPath() string { return filepath.Join(u.dir, "attempt.json") }

// once performs one check, and installs when the deployment offers a newer version.
func (u *agentUpdater) once(ctx context.Context) {
	now := u.clock()
	answer, offered, err := u.check(ctx, now)
	next := now.Add(agentUpdateInterval)
	u.mu.Lock()
	u.status.LastCheckAt, u.status.NextCheckAt = &now, &next
	u.status.LastAnswer, u.status.OfferedVersion, u.status.LastError = answer, offered, ""
	if err != nil {
		u.status.LastError = err.Error()
	}
	u.mu.Unlock()
	if err != nil {
		u.log.Warn("agent update: not installed", "answer", answer, "offered", offered, "error", err)
	}
}

func (u *agentUpdater) check(ctx context.Context, now time.Time) (answer, offered string, err error) {
	u.tidy()
	raw, err := u.source.FetchAgentRelease(ctx)
	if errors.Is(err, drain.ErrNoAgentRelease) {
		return "no_release", "", nil
	}
	if err != nil {
		return "error", "", err
	}
	st, err := verifyAgentRelease(raw, u.pub)
	if err != nil {
		return "error", st.Version, err
	}
	if st.Platform != u.platform || !agentVersionNewer(st.Version, u.current) {
		return "current", st.Version, nil
	}
	if a, ok := u.lastAttempt(); ok && a.Version == st.Version && now.Sub(a.At) < agentUpdateRetry {
		return "retry_later", st.Version, fmt.Errorf("the install of %s started at %s has not replaced %s; it is tried again after %s",
			st.Version, a.At.UTC().Format(time.RFC3339), u.current, a.At.Add(agentUpdateRetry).UTC().Format(time.RFC3339))
	}
	pkg, err := u.download(ctx, st)
	if err != nil {
		return "error", st.Version, err
	}
	tenant, err := os.ReadFile(u.tenantFile)
	if err != nil {
		return "error", st.Version, fmt.Errorf("reading the installed tenant file: %w", err)
	}
	if err := state.WriteFile(filepath.Join(filepath.Dir(pkg), tenantPackageFile), tenant); err != nil {
		return "error", st.Version, fmt.Errorf("writing the tenant file beside the package: %w", err)
	}
	attempt, _ := json.Marshal(agentAttempt{Version: st.Version, At: now})
	if err := state.WriteFile(u.attemptPath(), attempt); err != nil {
		return "error", st.Version, fmt.Errorf("recording the install: %w", err)
	}
	logPath := filepath.Join(filepath.Dir(pkg), "install.log")
	u.log.Info("agent update: installing", "from", u.current, "to", st.Version, "package", pkg, "installer_log", logPath)
	if err := u.install(pkg, logPath); err != nil {
		return "error", st.Version, fmt.Errorf("starting the installer: %w", err)
	}
	u.mu.Lock()
	u.status.InstallStarted, u.status.InstallerLog = &now, logPath
	u.mu.Unlock()
	return "installing", st.Version, nil
}

// download fetches the package into the version's folder, resuming a partial download, and
// returns its path once its size and sha256 are the statement's.
func (u *agentUpdater) download(ctx context.Context, st protocol.AgentRelease) (string, error) {
	dir := filepath.Join(u.dir, st.Version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, st.File)
	if verifyAgentPackage(final, st) == nil {
		return final, nil
	}
	_ = os.Remove(final)
	part := final + ".part"
	for attempt := 0; ; attempt++ {
		info, err := os.Stat(part)
		offset := int64(0)
		if err == nil {
			offset = info.Size()
		}
		if offset >= st.Size {
			break
		}
		if attempt == agentDownloadAttempts {
			return "", fmt.Errorf("the package is %d of %d bytes after %d requests", offset, st.Size, attempt)
		}
		f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return "", err
		}
		_, err = u.source.DownloadAgentPackage(ctx, f, offset, st.Size-offset)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if errors.Is(err, drain.ErrRangeRefused) {
			_ = os.Remove(part)
			continue
		}
		if err != nil && ctx.Err() != nil {
			return "", err
		}
		// Any other failure is retried from what arrived: progress is kept across requests.
	}
	if err := verifyAgentPackage(part, st); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

// verifyAgentPackage checks a downloaded package against the statement.
func verifyAgentPackage(path string, st protocol.AgentRelease) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, st.Size+1))
	if err != nil {
		return err
	}
	if n != st.Size {
		return fmt.Errorf("the package is %d bytes, not the %d the release names", n, st.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, st.SHA256) {
		return fmt.Errorf("the package's sha256 is %s, not the release's %s", got, st.SHA256)
	}
	return nil
}

func (u *agentUpdater) lastAttempt() (agentAttempt, bool) {
	raw, err := os.ReadFile(u.attemptPath())
	if err != nil {
		return agentAttempt{}, false
	}
	var a agentAttempt
	return a, json.Unmarshal(raw, &a) == nil && a.Version != ""
}

// tidy removes what a finished install no longer needs: the tenant file, which carries the
// deployment key, from every version's folder, and the folders of versions not newer than this
// one, keeping their installer logs.
func (u *agentUpdater) tidy() {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(u.dir, e.Name())
		_ = os.Remove(filepath.Join(dir, tenantPackageFile))
		if agentVersionNewer(e.Name(), u.current) {
			continue
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if f.Name() != "install.log" {
				_ = os.RemoveAll(filepath.Join(dir, f.Name()))
			}
		}
	}
}

// Status is the updater's state for the health document.
func (u *agentUpdater) Status() agentUpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

// Start checks after first, then every interval, until Stop.
func (u *agentUpdater) Start(ctx context.Context, first time.Duration) {
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		wait := first
		for {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-u.stop:
				t.Stop()
				return
			case <-t.C:
			}
			u.once(ctx)
			wait = agentUpdateInterval
		}
	}()
}

// Stop ends the loop and waits for it.
func (u *agentUpdater) Stop() {
	select {
	case <-u.stop:
		return
	default:
	}
	close(u.stop)
	u.wg.Wait()
}

// buildAgentUpdater builds the updater over the drain, or returns nil where the platform has no
// installer for a release, or the agent has no release key or tenant file to update with.
func (s *service) buildAgentUpdater(source agentSource) *agentUpdater {
	platform, install := platformAgentUpdate()
	if install == nil {
		return nil
	}
	tenantFile, ok := installedTenantFile(s.cfg)
	pub, err := hex.DecodeString(s.cfg.ClassifierPubkey)
	if !ok || err != nil || len(pub) != ed25519.PublicKeySize {
		s.log.Warn("agent update: off; it needs the pinned release key (SAC_CLASSIFIER_PUBKEY) and a tenant.env configuration file")
		return nil
	}
	return newAgentUpdater(source, ed25519.PublicKey(pub), version, platform, s.dir.Path(updateDir), tenantFile, install, s.log)
}

// installedTenantFile is the tenant file the service was started with: the installer replaces the
// installed copy with the one beside the package, so the package carries it unchanged.
func installedTenantFile(cfg Config) (string, bool) {
	for i := len(cfg.configFiles) - 1; i >= 0; i-- {
		if strings.EqualFold(filepath.Base(cfg.configFiles[i]), tenantConfigFile) {
			return cfg.configFiles[i], true
		}
	}
	return "", false
}
