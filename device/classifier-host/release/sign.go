package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Default artefact names inside a release directory.
const (
	RulesFileName    = "rules.json"
	ModelFileName    = "model.json"
	ManifestFileName = "manifest.json"
)

// Sign fills in the manifest's key id and signature over the payload the loader will verify.
// It is the only place the signing side is allowed to construct a signature, so the signing
// tool and Load cannot drift: both call LoadSigningPayload.
func Sign(m *Manifest, rulesRaw, modelRaw []byte, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: private key is %d bytes", ErrManifest, len(priv))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("%w: signing key has no public half", ErrManifest)
	}
	m.KeyID = KeyID(pub)
	m.Signature = ""
	payload, err := LoadSigningPayload(*m, rulesRaw, modelRaw)
	if err != nil {
		return err
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))
	return nil
}

// Write builds a signed release directory from raw artefact bytes. It is what the CLI's
// `release` subcommand, the equivalence harness and the tests use.
func Write(dir string, m Manifest, rulesRaw, modelRaw []byte, priv ed25519.PrivateKey) (Manifest, error) {
	if m.RulesFile == "" {
		m.RulesFile = RulesFileName
	}
	if m.ModelFile == "" && len(modelRaw) > 0 {
		m.ModelFile = ModelFileName
	}
	m.RulesDigest = digestOf(rulesRaw)
	if len(modelRaw) > 0 {
		m.ModelDigest = digestOf(modelRaw)
	} else {
		m.ModelFile = ""
		m.ModelDigest = ""
	}
	if err := Sign(&m, rulesRaw, modelRaw, priv); err != nil {
		return Manifest{}, err
	}
	if !m.State.Valid() {
		return Manifest{}, fmt.Errorf("%w: state %q is outside the closed set", ErrManifest, m.State)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, m.RulesFile), rulesRaw, 0o600); err != nil {
		return Manifest{}, err
	}
	if len(modelRaw) > 0 {
		if err := os.WriteFile(filepath.Join(dir, m.ModelFile), modelRaw, 0o600); err != nil {
			return Manifest{}, err
		}
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), append(body, '\n'), 0o600); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func decodeSignature(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
