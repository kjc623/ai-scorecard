package deploy

import (
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
)

// Browser extension delivery. Chrome and Edge install the extension from a force-install policy
// that names its id and this update manifest; they fetch the manifest and the CRX it points to
// without credentials, so both routes are unauthenticated and serve only what the release
// directory holds.
const (
	ExtensionFileName     = "shadow-ai-capture.crx"
	ExtensionManifestPath = "/v1/extension/updates.xml"
	ExtensionDownloadPath = "/v1/extension/" + ExtensionFileName
	maxExtensionBytes     = 32 << 20
)

var (
	extensionIDRE      = regexp.MustCompile(`^[a-p]{32}$`)
	extensionVersionRE = regexp.MustCompile(`^\d{1,9}(\.\d{1,9}){0,3}$`)
)

// Extensions serves the browser extension's update manifest and CRX.
type Extensions struct {
	releaseDir     string
	deviceEndpoint string
	logger         *slog.Logger
	now            func() time.Time
}

// NewExtensions builds the extension routes. deviceEndpoint is the public device origin the
// manifest's codebase is built on.
func NewExtensions(releaseDir, deviceEndpoint string, logger *slog.Logger) *Extensions {
	return &Extensions{
		releaseDir:     releaseDir,
		deviceEndpoint: strings.TrimRight(deviceEndpoint, "/"),
		logger:         logger,
		now:            time.Now,
	}
}

// Register adds GET (and HEAD) of the manifest and the CRX to mux.
func (e *Extensions) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+ExtensionManifestPath, e.handleManifest)
	mux.HandleFunc("GET "+ExtensionDownloadPath, e.handleCRX)
}

// extension reads the release's extension entry and checks its shape.
func (e *Extensions) extension() (ReleaseExtension, error) {
	r, err := ReadRelease(e.releaseDir)
	if err != nil {
		return ReleaseExtension{}, err
	}
	x := r.Extension
	switch {
	case x == nil:
		return ReleaseExtension{}, fmt.Errorf("%w: release.json names no extension", ErrReleaseUnavailable)
	case !extensionIDRE.MatchString(x.ID):
		return ReleaseExtension{}, fmt.Errorf("%w: extension id %q is not 32 characters a-p", ErrReleaseUnavailable, x.ID)
	case !extensionVersionRE.MatchString(x.Version):
		return ReleaseExtension{}, fmt.Errorf("%w: extension version %q is not dotted numbers", ErrReleaseUnavailable, x.Version)
	case x.File != ExtensionFileName:
		return ReleaseExtension{}, fmt.Errorf("%w: extension file %q is not %s", ErrReleaseUnavailable, x.File, ExtensionFileName)
	case x.Size <= 0 || x.Size > maxExtensionBytes:
		return ReleaseExtension{}, fmt.Errorf("%w: extension size %d is out of range", ErrReleaseUnavailable, x.Size)
	}
	return *x, nil
}

type gupdate struct {
	XMLName  xml.Name   `xml:"http://www.google.com/update2/response gupdate"`
	Protocol string     `xml:"protocol,attr"`
	Apps     []gupdateA `xml:"app"`
}

type gupdateA struct {
	AppID       string      `xml:"appid,attr"`
	UpdateCheck updateCheck `xml:"updatecheck"`
}

type updateCheck struct {
	Codebase   string `xml:"codebase,attr"`
	Version    string `xml:"version,attr"`
	HashSHA256 string `xml:"hash_sha256,attr"`
	Size       int64  `xml:"size,attr"`
}

func (e *Extensions) handleManifest(w http.ResponseWriter, r *http.Request) {
	if e.deviceEndpoint == "" {
		e.fail(w, apierr.New(http.StatusServiceUnavailable, apierr.CodeReleaseUnavailable,
			"this deployment has no public device endpoint configured"))
		return
	}
	x, err := e.extension()
	if err != nil {
		e.unavailable(w, err)
		return
	}
	doc, err := xml.Marshal(gupdate{Protocol: "2.0", Apps: []gupdateA{{
		AppID: x.ID,
		UpdateCheck: updateCheck{
			Codebase:   e.deviceEndpoint + "/v1/extension/" + x.File,
			Version:    x.Version,
			HashSHA256: strings.ToLower(strings.TrimPrefix(x.SHA256, "sha256:")),
			Size:       x.Size,
		},
	}}})
	if err != nil {
		e.fail(w, apierr.Internal(err))
		return
	}
	body := append([]byte(xml.Header), doc...)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func (e *Extensions) handleCRX(w http.ResponseWriter, r *http.Request) {
	x, err := e.extension()
	if err != nil {
		e.unavailable(w, err)
		return
	}
	crx, err := readVerified(filepath.Join(e.releaseDir, x.File), x.SHA256, x.Size, maxExtensionBytes)
	if err != nil {
		e.unavailable(w, fmt.Errorf("%w: %s: %v", ErrReleaseUnavailable, x.File, err))
		return
	}
	w.Header().Set("Content-Type", "application/x-chrome-extension")
	w.Header().Set("Content-Length", strconv.Itoa(len(crx)))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(crx)
	}
}

func (e *Extensions) unavailable(w http.ResponseWriter, err error) {
	if !errors.Is(err, ErrReleaseUnavailable) {
		e.fail(w, apierr.Internal(err))
		return
	}
	e.logger.Error("control: browser extension unavailable", "error", err)
	e.fail(w, apierr.New(http.StatusNotFound, apierr.CodeReleaseUnavailable,
		"the browser extension is not available on this deployment"))
}

func (e *Extensions) fail(w http.ResponseWriter, err error) {
	apierr.Write(w, err, e.now(), e.logger)
}
