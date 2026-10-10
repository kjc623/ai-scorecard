package deploy

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The tenant package. Every customer gets the same generic, code-only ShadowAICapture.msi; what
// makes a package theirs is one small file beside it, ShadowAICapture.tenant.env, which the MSI
// copies from its source folder at install. The file holds the tenant id, the device endpoint and
// the deployment key, so the MSI never changes per customer and needs no install parameters:
// `msiexec /i ShadowAICapture.msi /qn`.

// File names inside a package and in the release directory.
const (
	MSIFileName       = "ShadowAICapture.msi"
	TenantEnvFileName = "ShadowAICapture.tenant.env"
	ReadmeFileName    = "README.txt"
	ReleaseFileName   = "release.json"
	// DefaultProductName is the MSI's ProductName when release.json does not name it.
	DefaultProductName = "Shadow AI Capture"
	// DefaultMaxReleaseBytes bounds the MSI a package is built from. Packages are built in memory,
	// so this bounds one download's footprint (about three times the MSI).
	DefaultMaxReleaseBytes = 256 << 20
)

// How an MDM detects and removes the agent whatever version it has updated itself to: the MSI
// records its version in the registry, and the uninstall removes the product carrying the
// UpgradeCode (INSTALLED in device/installer/manifest.mjs).
const (
	DetectionKey     = `HKEY_LOCAL_MACHINE\SOFTWARE\ShadowAICapture`
	DetectionValue   = "Version"
	UninstallCommand = `powershell.exe -NoProfile -NonInteractive -Command "foreach ($p in (New-Object -ComObject WindowsInstaller.Installer).RelatedProducts('{7E9C2B7A-6D0E-4C6A-9F2B-1A6E6C2D44A1}')) { $r = (Start-Process msiexec.exe -ArgumentList '/x',$p,'/qn' -Wait -PassThru).ExitCode; if ($r -ne 0) { exit $r } }"`
)

// Release is release.json, written by the release build beside the generic MSI and the browser
// extension.
type Release struct {
	Version     string `json:"version"`
	ProductCode string `json:"product_code"`
	UpgradeCode string `json:"upgrade_code"`
	PackageCode string `json:"package_code,omitempty"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Publisher   string `json:"publisher"`
	Signed      bool   `json:"signed"`
	// Name is the MSI ProductName, when the build records it.
	Name string `json:"name,omitempty"`
	// Extension is the packaged browser extension, when the release carries one.
	Extension *ReleaseExtension `json:"extension,omitempty"`
}

// ReleaseExtension describes the browser extension's CRX in the release directory.
type ReleaseExtension struct {
	// ID is the extension id: 32 characters a-p, derived from the CRX signing key.
	ID      string `json:"id"`
	Version string `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// ProductName is what the Intune app is named after.
func (r Release) ProductName() string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return DefaultProductName
}

// ErrReleaseUnavailable wraps every reason the release directory cannot serve what was asked.
var ErrReleaseUnavailable = errors.New("deploy: agent release unavailable")

// ReadRelease reads and checks release.json.
func ReadRelease(dir string) (Release, error) {
	if strings.TrimSpace(dir) == "" {
		return Release{}, fmt.Errorf("%w: no release directory is configured", ErrReleaseUnavailable)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ReleaseFileName))
	if err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrReleaseUnavailable, err)
	}
	var r Release
	if err := json.Unmarshal(raw, &r); err != nil {
		return Release{}, fmt.Errorf("%w: release.json is not JSON: %v", ErrReleaseUnavailable, err)
	}
	if strings.TrimSpace(r.Version) == "" {
		return Release{}, fmt.Errorf("%w: release.json has no version", ErrReleaseUnavailable)
	}
	if r.ProductCode, err = msiGUID(r.ProductCode); err != nil {
		return Release{}, fmt.Errorf("%w: product_code: %v", ErrReleaseUnavailable, err)
	}
	if r.UpgradeCode, err = msiGUID(r.UpgradeCode); err != nil {
		return Release{}, fmt.Errorf("%w: upgrade_code: %v", ErrReleaseUnavailable, err)
	}
	if r.PackageCode != "" {
		if r.PackageCode, err = msiGUID(r.PackageCode); err != nil {
			return Release{}, fmt.Errorf("%w: package_code: %v", ErrReleaseUnavailable, err)
		}
	}
	return r, nil
}

// LoadRelease reads release.json and the MSI it describes, refusing an MSI whose size or sha256
// disagrees with it: a package carries exactly the build the release names.
func LoadRelease(dir string, maxBytes int64) (Release, []byte, error) {
	r, err := ReadRelease(dir)
	if err != nil {
		return Release{}, nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxReleaseBytes
	}
	msi, err := readVerified(filepath.Join(dir, MSIFileName), r.SHA256, r.Size, maxBytes)
	if err != nil {
		return Release{}, nil, fmt.Errorf("%w: %s: %v", ErrReleaseUnavailable, MSIFileName, err)
	}
	return r, msi, nil
}

// readVerified reads a release file and checks it against its recorded sha256 and size.
func readVerified(path, wantSHA256 string, wantSize, maxBytes int64) ([]byte, error) {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(wantSHA256), "sha256:"))
	if len(want) != sha256.Size*2 {
		return nil, errors.New("release.json records no sha256")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("over the %d-byte bound", maxBytes)
	}
	if wantSize > 0 && int64(len(data)) != wantSize {
		return nil, fmt.Errorf("%d bytes, release.json says %d", len(data), wantSize)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return nil, errors.New("sha256 does not match release.json")
	}
	return data, nil
}

// msiGUID normalises a GUID to the MSI spelling: braces, upper case.
func msiGUID(s string) (string, error) {
	g := strings.ToUpper(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(g) != 36 {
		return "", fmt.Errorf("%q is not a GUID", s)
	}
	for i, c := range g {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", fmt.Errorf("%q is not a GUID", s)
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
				return "", fmt.Errorf("%q is not a GUID", s)
			}
		}
	}
	return "{" + g + "}", nil
}

// TenantEnv renders ShadowAICapture.tenant.env: the tenant id, the device endpoint and the
// deployment key, CRLF, in the vocabulary the agent's configuration file reads.
func TenantEnv(tenantID, deviceEndpoint, deploymentKey string) []byte {
	lines := []string{
		"# Shadow AI Capture tenant settings. Keep this file beside ShadowAICapture.msi; the installer copies it.",
		"SAC_TENANT_ID=" + tenantID,
		"SAC_DEVICE_ENDPOINT=" + deviceEndpoint,
		"SAC_DEPLOYMENT_KEY=" + deploymentKey,
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// Readme renders README.txt for the .zip package: how to install and detect it, for an admin
// deploying through ConfigMgr, Group Policy or another MDM.
func Readme(r Release) []byte {
	lines := []string{
		r.ProductName() + " " + r.Version + " - deployment package",
		"",
		"Files",
		"  " + MSIFileName + "          the agent. Code only: the same for every customer.",
		"  " + TenantEnvFileName + "   this organisation's settings and deployment key.",
		"                               Keep it in the same folder as the MSI; the installer copies it.",
		"",
		"Install (as SYSTEM or an administrator; no other parameters are needed)",
		"  msiexec /i " + MSIFileName + " /qn",
		"",
		"Uninstall (any version)",
		"  " + UninstallCommand,
		"",
		"Detection rule",
		"  Registry " + DetectionKey + ", value " + DetectionValue + ",",
		"  version greater than or equal to " + r.Version,
		"",
		"Updates",
		"  The agent updates itself from the deployment it enrols with, verifying each release's signature,",
		"  so deploy this package once: the detection rule above stays satisfied as it updates.",
		"",
		"Requirements",
		"  Windows 10 21H2 or later, 64-bit (x64).",
		"",
		"The tenant file holds a deployment key. Anyone with this package can try to enrol a device into",
		"your organisation, so treat the package as you would an installer credential. If it leaks,",
		"revoke the key in Settings > Deployment and download a new package.",
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// BuildZip writes the .zip package: the MSI, the tenant file and the README, at the archive root.
func BuildZip(r Release, msi, tenantEnv []byte, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, fastDeflate)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{MSIFileName, msi},
		{TenantEnvFileName, tenantEnv},
		{ReadmeFileName, Readme(r)},
	} {
		if err := addDeflated(zw, f.name, f.data, now); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("deploy: close zip: %w", err)
	}
	return buf.Bytes(), nil
}

func fastDeflate(w io.Writer) (io.WriteCloser, error) { return flate.NewWriter(w, flate.BestSpeed) }

func addDeflated(zw *zip.Writer, name string, data []byte, now time.Time) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
	if err != nil {
		return fmt.Errorf("deploy: zip entry %s: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("deploy: zip entry %s: %w", name, err)
	}
	return nil
}

// addStored writes an uncompressed entry with its CRC and sizes in the local header and no data
// descriptor, the layout Microsoft's packaging tool writes. CreateRaw writes the header exactly as
// given, so the MS-DOS timestamp fields are filled here: CreateRaw does not derive them from
// Modified.
func addStored(zw *zip.Writer, name string, data []byte, now time.Time) error {
	date, clock := msDosTime(now)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name:           name,
		Method:         zip.Store,
		CreatorVersion: zipVersion20,
		ReaderVersion:  zipVersion20,
		//lint:ignore SA1019 CreateRaw writes only the MS-DOS fields; Modified is not consulted.
		ModifiedDate: date,
		//lint:ignore SA1019 as above.
		ModifiedTime:       clock,
		CRC32:              crc32.ChecksumIEEE(data),
		CompressedSize64:   uint64(len(data)),
		UncompressedSize64: uint64(len(data)),
	})
	if err != nil {
		return fmt.Errorf("deploy: zip entry %s: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("deploy: zip entry %s: %w", name, err)
	}
	return nil
}

// zipVersion20 is "version 2.0", what a stored or deflated entry needs to be extracted.
const zipVersion20 = 20

// msDosTime is the zip format's two-second, local-calendar timestamp.
func msDosTime(t time.Time) (date, clock uint16) {
	if t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	date = uint16(t.Day() + int(t.Month())<<5 + (t.Year()-1980)<<9)
	clock = uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)
	return date, clock
}
