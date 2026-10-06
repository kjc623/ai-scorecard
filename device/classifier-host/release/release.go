// Package release writes and loads a signed classifier release: a directory holding the rules
// document, the model artefact and a manifest that binds both to a version with an Ed25519
// signature.
//
// The build signs the release once (cmd/classifier-release) and the installer ships it beside the
// binary; the device loads it with the release-signing public key it was installed with. A
// release that does not verify, or whose rules or model do not compile, is refused whole.
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
)

// The files of a release directory.
const (
	ManifestFile = "manifest.json"
	RulesFile    = "rules.json"
	ModelFile    = "model.json"
)

const (
	maxManifestBytes = 64 << 10
	maxVersionBytes  = 64
)

// Manifest binds a version to the digests of the rules and the model. Signature is the base64
// Ed25519 signature over the manifest with Signature empty, followed by the rules bytes and then
// the model bytes.
type Manifest struct {
	Version     string `json:"version"`
	RulesDigest string `json:"rules_digest"`
	ModelDigest string `json:"model_digest"`
	KeyID       string `json:"key_id"`
	Signature   string `json:"signature"`
}

// Release is a loaded, verified release.
type Release struct {
	Version string
	Rules   *rules.Set
	Model   *model.Model
}

var (
	// ErrManifest is a missing, malformed or incomplete manifest.
	ErrManifest = errors.New("release: manifest rejected")
	// ErrDigest is an artefact that does not match the digest the manifest declares.
	ErrDigest = errors.New("release: artefact does not match its declared digest")
	// ErrSignature is a signature that does not verify under the trusted key.
	ErrSignature = errors.New("release: signature does not verify")
	// ErrContent is rules or a model that do not compile.
	ErrContent = errors.New("release: content rejected")
)

// KeyID identifies a signing key: the first sixteen hex characters of the SHA-256 of the public
// key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Digest is the digest a manifest records for an artefact: "sha256:" and 64 lowercase hex
// characters, the only accepted spelling.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func signingPayload(m Manifest, rulesRaw, modelRaw []byte) []byte {
	m.Signature = ""
	head, err := json.Marshal(m)
	if err != nil {
		panic("release: a manifest of strings does not marshal: " + err.Error())
	}
	return append(append(head, rulesRaw...), modelRaw...)
}

// Write compiles the rules and the model, signs them as version and writes the release into dir,
// creating it if needed. Nothing is written unless both compile.
func Write(dir, version string, rulesRaw, modelRaw []byte, priv ed25519.PrivateKey) (Manifest, error) {
	if version == "" || len(version) > maxVersionBytes {
		return Manifest{}, fmt.Errorf("%w: version is empty or over %d bytes", ErrManifest, maxVersionBytes)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return Manifest{}, fmt.Errorf("release: the signing key is %d bytes, want an Ed25519 private key", len(priv))
	}
	if err := compile(rulesRaw, modelRaw); err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		Version:     version,
		RulesDigest: Digest(rulesRaw),
		ModelDigest: Digest(modelRaw),
		KeyID:       KeyID(priv.Public().(ed25519.PublicKey)),
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signingPayload(m, rulesRaw, modelRaw)))
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Manifest{}, err
	}
	for name, body := range map[string][]byte{RulesFile: rulesRaw, ModelFile: modelRaw, ManifestFile: append(manifest, '\n')} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return Manifest{}, err
		}
	}
	return m, nil
}

// Load reads the release in dir, verifies it under pub and compiles it. The files are opened
// through an os.Root, so a symbolic link cannot make the loader read outside dir.
func Load(dir string, pub ed25519.PublicKey) (*Release, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: the trusted key is %d bytes, want an Ed25519 public key", ErrSignature, len(pub))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	defer root.Close()

	raw, err := read(root, ManifestFile, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if m.Version == "" || len(m.Version) > maxVersionBytes || m.RulesDigest == "" || m.ModelDigest == "" || m.KeyID == "" || m.Signature == "" {
		return nil, fmt.Errorf("%w: version, rules_digest, model_digest, key_id and signature are all required", ErrManifest)
	}
	rulesRaw, err := read(root, RulesFile, rules.MaxDocumentBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	modelRaw, err := read(root, ModelFile, model.MaxArtefactBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if got := Digest(rulesRaw); got != m.RulesDigest {
		return nil, fmt.Errorf("%w: %s is %s, the manifest declares %s", ErrDigest, RulesFile, got, m.RulesDigest)
	}
	if got := Digest(modelRaw); got != m.ModelDigest {
		return nil, fmt.Errorf("%w: %s is %s, the manifest declares %s", ErrDigest, ModelFile, got, m.ModelDigest)
	}
	if m.KeyID != KeyID(pub) {
		return nil, fmt.Errorf("%w: release %s is signed by key %s, this device trusts key %s", ErrSignature, m.Version, m.KeyID, KeyID(pub))
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || !ed25519.Verify(pub, signingPayload(m, rulesRaw, modelRaw), sig) {
		return nil, fmt.Errorf("%w: release %s", ErrSignature, m.Version)
	}
	set, err := rules.Compile(rulesRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContent, err)
	}
	mdl, err := model.Parse(modelRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContent, err)
	}
	return &Release{Version: m.Version, Rules: set, Model: mdl}, nil
}

func compile(rulesRaw, modelRaw []byte) error {
	if _, err := rules.Compile(rulesRaw); err != nil {
		return fmt.Errorf("%w: %v", ErrContent, err)
	}
	if _, err := model.Parse(modelRaw); err != nil {
		return fmt.Errorf("%w: %v", ErrContent, err)
	}
	return nil
}

// read reads one file of the release, refusing a directory or a file over limit bytes.
func read(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("%s is not a regular file of at most %d bytes", name, limit)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}
