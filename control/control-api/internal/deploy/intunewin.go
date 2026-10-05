package deploy

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"time"
)

// The Intune Win32 app content format (.intunewin), as Microsoft's Win32 Content Prep Tool
// (IntuneWinAppUtil) writes it. Intune accepts no other upload for a Win32 app, so the package is
// built here rather than asking each customer admin to run the tool.
//
// Layout, verified against the tool's documented behaviour and two independent reimplementations
// (svrooij/ContentPrep, which round-trips with the tool):
//
//	<name>.intunewin                          a zip, entries stored (no compression):
//	  IntuneWinPackage/Contents/IntunePackage.intunewin   the encrypted inner zip
//	  IntuneWinPackage/Metadata/Detection.xml             ApplicationInfo
//
// The inner zip is the setup folder's contents at the archive root (no base directory), deflated.
// It is encrypted as HMAC(32) || IV(16) || AES-256-CBC-PKCS7(inner zip), where the HMAC is
// HMAC-SHA256 under a separate random key over IV || ciphertext. Detection.xml carries both keys,
// the IV, the MAC, and FileDigest = base64(SHA-256 of the unencrypted inner zip); Intune uses them
// to verify the upload and the Intune Management Extension uses them to decrypt on the device.
// UnencryptedContentSize is the inner zip's size.
//
// The encrypted entry is IntunePackage.intunewin: the name the tool writes, which Detection.xml's
// FileName must match and which readers locate the content by.

// Names and constants of the format.
const (
	intuneContentsEntry  = "IntuneWinPackage/Contents/" + IntunePackageFileName
	intuneDetectionEntry = "IntuneWinPackage/Metadata/Detection.xml"
	// IntunePackageFileName is the encrypted content's name, and Detection.xml's FileName.
	IntunePackageFileName = "IntunePackage.intunewin"
	// IntuneToolVersion is the Content Prep Tool version whose output this reproduces.
	IntuneToolVersion = "1.8.6.0"
	intuneProfile     = "ProfileVersion1"
	intuneDigestAlg   = "SHA256"
)

// ApplicationInfo is Detection.xml. The element order is the tool's.
type ApplicationInfo struct {
	XMLName                xml.Name       `xml:"ApplicationInfo"`
	XSD                    string         `xml:"xmlns:xsd,attr"`
	XSI                    string         `xml:"xmlns:xsi,attr"`
	ToolVersion            string         `xml:"ToolVersion,attr"`
	Name                   string         `xml:"Name"`
	UnencryptedContentSize int64          `xml:"UnencryptedContentSize"`
	FileName               string         `xml:"FileName"`
	SetupFile              string         `xml:"SetupFile"`
	EncryptionInfo         EncryptionInfo `xml:"EncryptionInfo"`
	MsiInfo                *MsiInfo       `xml:"MsiInfo,omitempty"`
}

// EncryptionInfo is the content's keys and digests, each base64 (standard, padded) as the tool
// writes them. Intune's fileEncryptionInfo takes the same seven values.
type EncryptionInfo struct {
	EncryptionKey        string `xml:"EncryptionKey"`
	MacKey               string `xml:"MacKey"`
	InitializationVector string `xml:"InitializationVector"`
	Mac                  string `xml:"Mac"`
	ProfileIdentifier    string `xml:"ProfileIdentifier"`
	FileDigest           string `xml:"FileDigest"`
	FileDigestAlgorithm  string `xml:"FileDigestAlgorithm"`
}

// MsiInfo is what the Intune portal pre-fills an MSI app's install context and detection rule from.
// The values describe the generic ShadowAICapture.msi: a per-machine install that runs as SYSTEM,
// registers a service, and writes machine registry keys and system folders.
type MsiInfo struct {
	MsiProductCode                string `xml:"MsiProductCode"`
	MsiProductVersion             string `xml:"MsiProductVersion"`
	MsiPackageCode                string `xml:"MsiPackageCode,omitempty"`
	MsiUpgradeCode                string `xml:"MsiUpgradeCode"`
	MsiExecutionContext           string `xml:"MsiExecutionContext"`
	MsiRequiresLogon              bool   `xml:"MsiRequiresLogon"`
	MsiRequiresReboot             bool   `xml:"MsiRequiresReboot"`
	MsiIsMachineInstall           bool   `xml:"MsiIsMachineInstall"`
	MsiIsUserInstall              bool   `xml:"MsiIsUserInstall"`
	MsiIncludesServices           bool   `xml:"MsiIncludesServices"`
	MsiIncludesODBCDataSource     bool   `xml:"MsiIncludesODBCDataSource"`
	MsiContainsSystemRegistryKeys bool   `xml:"MsiContainsSystemRegistryKeys"`
	MsiContainsSystemFolders      bool   `xml:"MsiContainsSystemFolders"`
	MsiPublisher                  string `xml:"MsiPublisher"`
}

// BuildIntuneWin writes the .intunewin package for the setup folder {MSI, tenant file}, with
// ShadowAICapture.msi as the setup file.
func BuildIntuneWin(r Release, msi, tenantEnv []byte, now time.Time) ([]byte, error) {
	var inner bytes.Buffer
	zw := zip.NewWriter(&inner)
	zw.RegisterCompressor(zip.Deflate, fastDeflate)
	if err := addDeflated(zw, MSIFileName, msi, now); err != nil {
		return nil, err
	}
	if err := addDeflated(zw, TenantEnvFileName, tenantEnv, now); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("deploy: close inner zip: %w", err)
	}

	encrypted, enc, err := encryptIntuneContent(inner.Bytes())
	if err != nil {
		return nil, err
	}
	info := ApplicationInfo{
		XSD:                    "http://www.w3.org/2001/XMLSchema",
		XSI:                    "http://www.w3.org/2001/XMLSchema-instance",
		ToolVersion:            IntuneToolVersion,
		Name:                   r.ProductName(),
		UnencryptedContentSize: int64(inner.Len()),
		FileName:               IntunePackageFileName,
		SetupFile:              MSIFileName,
		EncryptionInfo:         enc,
		MsiInfo: &MsiInfo{
			MsiProductCode:                r.ProductCode,
			MsiProductVersion:             r.Version,
			MsiPackageCode:                r.PackageCode,
			MsiUpgradeCode:                r.UpgradeCode,
			MsiExecutionContext:           "System",
			MsiRequiresLogon:              false,
			MsiRequiresReboot:             false,
			MsiIsMachineInstall:           true,
			MsiIsUserInstall:              false,
			MsiIncludesServices:           true,
			MsiIncludesODBCDataSource:     false,
			MsiContainsSystemRegistryKeys: true,
			MsiContainsSystemFolders:      true,
			MsiPublisher:                  r.Publisher,
		},
	}
	detection, err := xml.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("deploy: detection.xml: %w", err)
	}

	var outer bytes.Buffer
	ow := zip.NewWriter(&outer)
	if err := addStored(ow, intuneContentsEntry, encrypted, now); err != nil {
		return nil, err
	}
	if err := addStored(ow, intuneDetectionEntry, detection, now); err != nil {
		return nil, err
	}
	if err := ow.Close(); err != nil {
		return nil, fmt.Errorf("deploy: close intunewin: %w", err)
	}
	return outer.Bytes(), nil
}

// encryptIntuneContent applies the format's encryption with fresh keys and returns the encrypted
// file and its EncryptionInfo. A key is never reused: each package has its own.
func encryptIntuneContent(plain []byte) ([]byte, EncryptionInfo, error) {
	var key, macKey [32]byte
	var iv [aes.BlockSize]byte
	for _, b := range [][]byte{key[:], macKey[:], iv[:]} {
		if _, err := rand.Read(b); err != nil {
			return nil, EncryptionInfo{}, fmt.Errorf("deploy: no entropy for intunewin keys: %w", err)
		}
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, EncryptionInfo{}, fmt.Errorf("deploy: aes: %w", err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize // PKCS#7: always 1..16 bytes, never zero
	out := make([]byte, sha256.Size+aes.BlockSize+len(plain)+pad)
	body := out[sha256.Size:]
	copy(body, iv[:])
	ct := body[aes.BlockSize:]
	copy(ct, plain)
	for i := len(plain); i < len(ct); i++ {
		ct[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(ct, ct)
	mac := hmac.New(sha256.New, macKey[:])
	mac.Write(body) // IV || ciphertext
	sum := mac.Sum(nil)
	copy(out, sum)
	digest := sha256.Sum256(plain)
	b64 := base64.StdEncoding.EncodeToString
	return out, EncryptionInfo{
		EncryptionKey:        b64(key[:]),
		MacKey:               b64(macKey[:]),
		InitializationVector: b64(iv[:]),
		Mac:                  b64(sum),
		ProfileIdentifier:    intuneProfile,
		FileDigest:           b64(digest[:]),
		FileDigestAlgorithm:  intuneDigestAlg,
	}, nil
}
