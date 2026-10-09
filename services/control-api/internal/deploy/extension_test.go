package deploy_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/deploy"
)

const (
	extensionID = "abcdefghijklmnopabcdefghijklmnop"
	publicURL   = "https://app.eu.example.com"
)

var crxBytes = []byte("Cr24\x03\x00\x00\x00 extension payload")

func writeExtensionRelease(t *testing.T, mutate func(*deploy.ReleaseExtension)) string {
	t.Helper()
	sum := sha256.Sum256(crxBytes)
	x := &deploy.ReleaseExtension{
		ID: extensionID, Version: "1.2.3", File: deploy.ExtensionFileName,
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(crxBytes)),
	}
	if mutate != nil {
		mutate(x)
	}
	dir := writeRelease(t, func(r *deploy.Release) { r.Extension = x })
	if err := os.WriteFile(filepath.Join(dir, deploy.ExtensionFileName), crxBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func extensionMux(dir, origin string) *http.ServeMux {
	mux := http.NewServeMux()
	deploy.NewExtensions(dir, origin, quiet).Register(mux)
	return mux
}

func get(mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestUpdateManifestNamesTheReleasedExtension(t *testing.T) {
	mux := extensionMux(writeExtensionRelease(t, nil), publicURL+"/")
	rec := get(mux, http.MethodGet, deploy.ExtensionManifestPath)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/xml") {
		t.Fatalf("manifest: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var doc struct {
		XMLName  xml.Name `xml:"http://www.google.com/update2/response gupdate"`
		Protocol string   `xml:"protocol,attr"`
		App      struct {
			AppID       string `xml:"appid,attr"`
			UpdateCheck struct {
				Codebase string `xml:"codebase,attr"`
				Version  string `xml:"version,attr"`
				Hash     string `xml:"hash_sha256,attr"`
				Size     string `xml:"size,attr"`
			} `xml:"updatecheck"`
		} `xml:"app"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("manifest is not the gupdate document: %v\n%s", err, rec.Body.String())
	}
	sum := sha256.Sum256(crxBytes)
	uc := doc.App.UpdateCheck
	if doc.Protocol != "2.0" || doc.App.AppID != extensionID || uc.Version != "1.2.3" ||
		uc.Codebase != publicURL+"/v1/extension/shadow-ai-capture.crx" ||
		uc.Hash != hex.EncodeToString(sum[:]) || uc.Size != strconv.Itoa(len(crxBytes)) {
		t.Fatalf("manifest = %+v", doc)
	}
	if head := get(mux, http.MethodHead, deploy.ExtensionManifestPath); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d with %d body bytes", head.Code, head.Body.Len())
	}
}

func TestCRXIsServedOnlyWhenItMatchesTheRelease(t *testing.T) {
	mux := extensionMux(writeExtensionRelease(t, nil), publicURL)
	rec := get(mux, http.MethodGet, deploy.ExtensionDownloadPath)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), crxBytes) ||
		rec.Header().Get("Content-Type") != "application/x-chrome-extension" {
		t.Fatalf("crx: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	tampered := writeExtensionRelease(t, func(x *deploy.ReleaseExtension) { x.SHA256 = strings.Repeat("0", 64) })
	if rec := get(extensionMux(tampered, publicURL), http.MethodGet, deploy.ExtensionDownloadPath); rec.Code != http.StatusNotFound {
		t.Fatalf("a CRX that does not match release.json was served: %d", rec.Code)
	}
}

func TestExtensionRoutesRefuseAnIncompleteRelease(t *testing.T) {
	for name, dir := range map[string]string{
		"no extension": writeRelease(t, nil),
		"bad id":       writeExtensionRelease(t, func(x *deploy.ReleaseExtension) { x.ID = "not-an-extension-id" }),
		"bad version":  writeExtensionRelease(t, func(x *deploy.ReleaseExtension) { x.Version = "1.x" }),
		"other file":   writeExtensionRelease(t, func(x *deploy.ReleaseExtension) { x.File = "../release.json" }),
		"no size":      writeExtensionRelease(t, func(x *deploy.ReleaseExtension) { x.Size = 0 }),
		"no release":   t.TempDir(),
	} {
		mux := extensionMux(dir, publicURL)
		for _, path := range []string{deploy.ExtensionManifestPath, deploy.ExtensionDownloadPath} {
			if rec := get(mux, http.MethodGet, path); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", name, path, rec.Code)
			}
		}
	}
	if rec := get(extensionMux(writeExtensionRelease(t, nil), ""), http.MethodGet, deploy.ExtensionManifestPath); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no public URL: %d", rec.Code)
	}
	if rec := get(extensionMux(writeExtensionRelease(t, nil), publicURL), http.MethodPost, deploy.ExtensionManifestPath); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}
