package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
)

// Agent update delivery. An enrolled device asks for the signed statement of the release this
// deployment runs, and, when it names a newer version than its own, downloads the package. The
// statement is signed by the release build and served byte for byte; this service holds no key
// that could sign one, and checks only that it names the MSI beside it, so a release whose
// statement and package disagree is reported as unavailable rather than served.

// AgentReleaseFileName is the signed statement in the release directory.
const AgentReleaseFileName = "agent-release.json"

// maxAgentReleaseBytes bounds the statement, a few hundred bytes.
const maxAgentReleaseBytes = 16 << 10

// DeviceAuthenticator resolves the device a request authenticates as, from its certificate.
type DeviceAuthenticator func(r *http.Request) (tenantID, deviceID string, err error)

// Agents serves GET /v1/agent/release and GET /v1/agent/package to enrolled devices.
type Agents struct {
	releaseDir string
	logger     *slog.Logger
	now        func() time.Time
}

// NewAgents builds the agent update routes over the release directory.
func NewAgents(releaseDir string, logger *slog.Logger) *Agents {
	return &Agents{releaseDir: releaseDir, logger: logger, now: time.Now}
}

// Register adds the routes to mux, each authenticated by auth.
func (a *Agents) Register(mux *http.ServeMux, auth DeviceAuthenticator) {
	mux.HandleFunc("GET "+protocol.AgentReleasePath, a.authenticated(auth, a.handleRelease))
	mux.HandleFunc("GET "+protocol.AgentPackagePath, a.authenticated(auth, a.handlePackage))
}

func (a *Agents) authenticated(auth DeviceAuthenticator, next func(http.ResponseWriter, *http.Request, string, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID, deviceID, err := auth(r)
		if err != nil {
			a.fail(w, err)
			return
		}
		next(w, r, tenantID, deviceID)
	}
}

// statement reads the signed statement and checks it names release.json's MSI.
func (a *Agents) statement() ([]byte, protocol.AgentRelease, error) {
	rel, err := ReadRelease(a.releaseDir)
	if err != nil {
		return nil, protocol.AgentRelease{}, err
	}
	raw, err := readBounded(filepath.Join(a.releaseDir, AgentReleaseFileName), maxAgentReleaseBytes)
	if err != nil {
		return nil, protocol.AgentRelease{}, fmt.Errorf("%w: %s: %v", ErrReleaseUnavailable, AgentReleaseFileName, err)
	}
	var signed protocol.SignedAgentRelease
	if err := json.Unmarshal(raw, &signed); err != nil {
		return nil, protocol.AgentRelease{}, fmt.Errorf("%w: %s is not JSON: %v", ErrReleaseUnavailable, AgentReleaseFileName, err)
	}
	var st protocol.AgentRelease
	if err := json.Unmarshal(signed.Payload, &st); err != nil {
		return nil, protocol.AgentRelease{}, fmt.Errorf("%w: %s payload: %v", ErrReleaseUnavailable, AgentReleaseFileName, err)
	}
	switch {
	case st.Type != protocol.AgentReleaseType || st.Platform != protocol.AgentPlatformWindowsAMD64:
		return nil, st, fmt.Errorf("%w: %s is a %q statement for %q", ErrReleaseUnavailable, AgentReleaseFileName, st.Type, st.Platform)
	case st.Version != rel.Version || st.File != MSIFileName || st.Size != rel.Size ||
		!strings.EqualFold(st.SHA256, strings.TrimPrefix(rel.SHA256, "sha256:")):
		return nil, st, fmt.Errorf("%w: %s names %s %s, not release.json's %s", ErrReleaseUnavailable, AgentReleaseFileName, st.File, st.Version, rel.Version)
	}
	return bytes.TrimSpace(raw), st, nil
}

func (a *Agents) handleRelease(w http.ResponseWriter, r *http.Request, tenantID, deviceID string) {
	raw, st, err := a.statement()
	if err != nil {
		a.unavailable(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
	a.logger.Info("control: agent release served", "tenant", tenantID, "device", deviceID, "version", st.Version)
}

// handlePackage serves the MSI with range support, so a device on a slow link resumes a download
// the gateway's request timeout cut short. The device checks the bytes against the statement.
func (a *Agents) handlePackage(w http.ResponseWriter, r *http.Request, tenantID, deviceID string) {
	_, st, err := a.statement()
	if err != nil {
		a.unavailable(w, err)
		return
	}
	f, err := os.Open(filepath.Join(a.releaseDir, st.File))
	if err != nil {
		a.unavailable(w, fmt.Errorf("%w: %s: %v", ErrReleaseUnavailable, st.File, err))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != st.Size {
		a.unavailable(w, fmt.Errorf("%w: %s is not the %d bytes the statement names", ErrReleaseUnavailable, st.File, st.Size))
		return
	}
	w.Header().Set("Content-Type", "application/x-msi")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, st.File, info.ModTime(), f)
	if r.Header.Get("Range") == "" {
		a.logger.Info("control: agent package served", "tenant", tenantID, "device", deviceID, "version", st.Version)
	}
}

func (a *Agents) unavailable(w http.ResponseWriter, err error) {
	if !errors.Is(err, ErrReleaseUnavailable) {
		a.fail(w, apierr.Internal(err))
		return
	}
	a.logger.Error("control: agent release unavailable", "error", err)
	a.fail(w, apierr.New(http.StatusNotFound, apierr.CodeReleaseUnavailable,
		"no agent release is available on this deployment"))
}

func (a *Agents) fail(w http.ResponseWriter, err error) {
	apierr.Write(w, err, a.now(), a.logger)
}

// readBounded reads a file of at most max bytes.
func readBounded(path string, max int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%d bytes, more than %d", info.Size(), max)
	}
	return os.ReadFile(path)
}
