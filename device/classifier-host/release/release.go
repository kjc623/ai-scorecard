// Package release loads and holds the signed classifier release of docs/01-collectors.md §9.5,
// §9.6 and §9.7.
//
// A release is a directory of signed data: a manifest, the rules document, and optionally a
// model artefact. The manifest is the promotion mechanism of §9.6 —
// `classifier_release: { version, state, rules_digest, model_digest }` — plus the ed25519
// signature over all three, so "a rule set and the policy using it cannot be separated by an
// attacker" (§9.5).
//
// Two load-time rules from §9.6 shape this package:
//
//   - **A release that fails to load retains the previously loaded release.** The store only
//     swaps a release in after every check has passed, and it keeps the outgoing release as the
//     rollback target. There is no path that ends at "no rules".
//   - **Failure is reported with a specific cause.** A rejected release records the cause and
//     its detail (always `release_load_failed`, §9.7's last row) so the response can be degraded
//     *and attributable* rather than merely wrong.
//
// Files are opened through os.Root, so a manifest naming `..\..\secret` (or a symlink pointing
// outside the release directory) cannot read anything outside it: §9.5's "signed data" is also
// "data from a directory whose boundary the loader enforces".
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
)

// State is §9.6's release state machine.
type State string

const (
	// StateShadow computes and records decisions and enforces nothing.
	StateShadow State = "shadow"
	// StateEnforcing computes, records and enforces.
	StateEnforcing State = "enforcing"
	// StateRolledBack classifies at the previous version and enforces nothing.
	StateRolledBack State = "rolled_back"
)

// Valid reports whether the state is in §9.6's closed set. An unknown state is a rejected
// release, never a default: defaulting here would turn a typo into "enforce nothing".
func (s State) Valid() bool {
	switch s {
	case StateShadow, StateEnforcing, StateRolledBack:
		return true
	default:
		return false
	}
}

// Enforces reports whether this state may act on a decision (§9.6's "Enforcement acts" column).
func (s State) Enforces() bool { return s == StateEnforcing }

// Shadowed reports whether the decision is computed and recorded but not enforced.
func (s State) Shadowed() bool { return !s.Enforces() }

// Manifest is the signed release descriptor.
type Manifest struct {
	Version         string `json:"version"`
	State           State  `json:"state"`
	RulesFile       string `json:"rules_file"`
	RulesDigest     string `json:"rules_digest"`
	ModelFile       string `json:"model_file,omitempty"`
	ModelDigest     string `json:"model_digest,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	KeyID           string `json:"key_id"`
	Signature       string `json:"signature,omitempty"` // base64 ed25519, over the signing payload
}

// Release is a loaded, verified release.
type Release struct {
	Version         string
	State           State
	Rules           *rules.Set
	Model           *model.Model
	RulesDigest     string
	ModelDigest     string
	ManifestDigest  string
	Dir             string
	PreviousVersion string
}

// HasModel reports whether the release carries a model artefact. A rules-only release is a
// legitimate signed release; the model stage then degrades with `model_unavailable` (§9.7)
// instead of the release failing to load, because "the model artefact was missing" is a listed
// degraded cause and not a retention case.
func (r *Release) HasModel() bool { return r != nil && r.Model != nil }

// Trust is the device's release-signing trust store: key id -> public key.
type Trust struct {
	Keys map[string]ed25519.PublicKey
}

// NewTrust builds a trust store from key id -> public key. The key id is derived from the key
// (KeyID) so a manifest cannot select a key it was not signed with.
func NewTrust(keys ...ed25519.PublicKey) *Trust {
	t := &Trust{Keys: map[string]ed25519.PublicKey{}}
	for _, k := range keys {
		t.Keys[KeyID(k)] = k
	}
	return t
}

// KeyID is the identifier of a signing key: the first sixteen hex characters of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

var (
	// ErrSignature is a signature that does not verify under the manifest's declared key.
	ErrSignature = errors.New("release: signature does not verify")
	// ErrDigest is a declared digest that does not match the artefact.
	ErrDigest = errors.New("release: artefact does not match its declared digest")
	// ErrManifest is a malformed or rejected manifest.
	ErrManifest = errors.New("release: manifest rejected")
	// ErrRules is a rejected rules document.
	ErrRules = errors.New("release: rules rejected")
	// ErrModel is a rejected model artefact.
	ErrModel = errors.New("release: model rejected")
	// ErrUnknownKey is a signature by a key this device does not trust.
	ErrUnknownKey = errors.New("release: signing key is not in the trust store")
	// ErrNothingToRollBackTo is `rolled_back` with no retained release: enforcement stops, but
	// the classification version cannot change.
	ErrNothingToRollBackTo = errors.New("release: rolled_back with no retained release to classify with")
)

const (
	maxManifestBytes = 64 << 10
	// maxArtefactBytes bounds a single artefact read; the rules and model packages impose their
	// own, smaller caps after the bytes are read, and this bound keeps a hostile manifest from
	// asking the loader to read a 4 GB file before that check happens.
	maxArtefactBytes = 8 << 20
)

// LoadSigningPayload is the byte string a release signature covers: the canonical manifest with
// the signature field cleared, followed by the rules bytes and, when declared, the model bytes.
// Exported so the signing tool and the loader cannot drift.
func LoadSigningPayload(m Manifest, rulesRaw, modelRaw []byte) ([]byte, error) {
	m.Signature = ""
	head, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 0, len(head)+len(rulesRaw)+len(modelRaw))
	payload = append(payload, head...)
	payload = append(payload, rulesRaw...)
	payload = append(payload, modelRaw...)
	return payload, nil
}

// Load reads, verifies and compiles a release directory. It returns a Release only when every
// step passed; a partial release is never returned.
func Load(dir string, trust *Trust, rc rules.Caps, mc model.Caps) (*Release, error) {
	if trust == nil || len(trust.Keys) == 0 {
		return nil, fmt.Errorf("%w: no release-signing keys are configured, so nothing can be verified", ErrManifest)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	defer root.Close()

	manifestRaw, err := readArtifact(root, "manifest.json", maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(manifestRaw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if err := validateManifest(m); err != nil {
		return nil, err
	}

	rulesRaw, err := readArtifact(root, m.RulesFile, maxArtefactBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRules, err)
	}
	var modelRaw []byte
	if m.ModelFile != "" {
		modelRaw, err = readArtifact(root, m.ModelFile, maxArtefactBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrModel, err)
		}
	}

	if err := verifyDigest(rulesRaw, m.RulesDigest, "rules"); err != nil {
		return nil, err
	}
	if m.ModelDigest != "" {
		if err := verifyDigest(modelRaw, m.ModelDigest, "model"); err != nil {
			return nil, err
		}
	}

	pub, ok := trust.Keys[m.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownKey, m.KeyID)
	}
	sig, err := decodeSignature(m.Signature)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	payload, err := LoadSigningPayload(m, rulesRaw, modelRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, fmt.Errorf("%w: manifest %s was not signed by key %s", ErrSignature, m.Version, m.KeyID)
	}

	set, err := rules.Compile(rulesRaw, rc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRules, err)
	}
	rel := &Release{
		Version:         m.Version,
		State:           m.State,
		Rules:           set,
		RulesDigest:     m.RulesDigest,
		ModelDigest:     m.ModelDigest,
		ManifestDigest:  digestOf(manifestRaw),
		Dir:             abs,
		PreviousVersion: m.PreviousVersion,
	}
	if m.ModelFile != "" {
		mdl, err := model.Load(modelRaw, m.ModelDigest, mc)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrModel, err)
		}
		rel.Model = mdl
	}
	return rel, nil
}

func validateManifest(m Manifest) error {
	if m.Version == "" || len(m.Version) > 64 {
		return fmt.Errorf("%w: version is empty or over 64 bytes", ErrManifest)
	}
	if !m.State.Valid() {
		return fmt.Errorf("%w: state %q is outside the closed set {shadow, enforcing, rolled_back}", ErrManifest, m.State)
	}
	if !filepath.IsLocal(m.RulesFile) {
		return fmt.Errorf("%w: rules_file %q is not a plain file name inside the release directory", ErrManifest, m.RulesFile)
	}
	if m.RulesDigest == "" {
		return fmt.Errorf("%w: rules_digest is required", ErrManifest)
	}
	if m.ModelFile != "" {
		if !filepath.IsLocal(m.ModelFile) {
			return fmt.Errorf("%w: model_file %q is not a plain file name inside the release directory", ErrManifest, m.ModelFile)
		}
		if m.ModelDigest == "" {
			return fmt.Errorf("%w: model_file is declared without model_digest, so the artefact cannot be verified", ErrManifest)
		}
	}
	if m.ModelDigest != "" && m.ModelFile == "" {
		return fmt.Errorf("%w: model_digest is declared without model_file", ErrManifest)
	}
	if m.KeyID == "" {
		return fmt.Errorf("%w: key_id is required", ErrManifest)
	}
	if m.Signature == "" {
		return fmt.Errorf("%w: signature is required", ErrManifest)
	}
	return nil
}

// readArtifact reads one artefact through the release root, bounded before allocation.
func readArtifact(root *os.Root, name string, max int64) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("artefact name %q is not a plain file name inside the release directory", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%q is a directory", name)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%q is %d bytes, over the %d-byte cap", name, info.Size(), max)
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}

func verifyDigest(raw []byte, want, what string) error {
	got := digestOf(raw)
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s is %s, manifest declares %s", ErrDigest, what, got, want)
	}
	return nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Failure records a rejected release load, with the cause the response must report.
type Failure struct {
	Version string
	Dir     string
	Cause   string
	Err     string
	When    time.Time
}

// Report is what an Apply did, for counters and for the health channel.
type Report struct {
	Version         string
	State           State
	RetainedVersion string
	ShadowVersion   string
	RulesVersion    string
	ModelVersion    string
	RolledBack      bool
}

// Store holds the resident release, the optional shadow release, and the outgoing release kept
// as the rollback target. Reads are lock-free for the common case: Active and Shadow are
// pointer loads under a read lock, and every release is immutable after Load.
type Store struct {
	mu         sync.RWMutex
	active     *Release
	shadow     *Release
	retained   *Release
	state      State
	failure    *Failure
	loads      uint64
	rejections uint64
}

// NewStore returns an empty store. An empty store classifies nothing: the host degrades every
// request with `release_load_failed` rather than reporting an empty label set as a result.
func NewStore() *Store { return &Store{} }

// Apply loads the release in dir and applies §9.6's state transition atomically.
//
//   - shadow: the release becomes the shadow release; it becomes the active release only if
//     there is none yet (a first release is evaluated in shadow, which is C20's "every new
//     release's first state").
//   - enforcing: the release becomes active, the outgoing release becomes the rollback target.
//   - rolled_back: enforcement stops and classification reverts to the retained release.
//
// A load failure changes nothing and records a Failure. The returned error is the load error;
// the caller reports degraded with `release_load_failed` and the *retained* release's labels.
func (s *Store) Apply(dir string, trust *Trust, rc rules.Caps, mc model.Caps) (Report, error) {
	rel, err := Load(dir, trust, rc, mc)
	if err != nil {
		s.mu.Lock()
		s.rejections++
		s.failure = &Failure{Version: fileVersion(dir), Dir: dir, Cause: causeOf(err), Err: err.Error(), When: time.Now()}
		s.mu.Unlock()
		return Report{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	s.failure = nil
	rep := Report{Version: rel.Version, State: rel.State, RulesVersion: rel.Rules.Version()}
	if rel.Model != nil {
		rep.ModelVersion = rel.Model.Version()
	}
	switch rel.State {
	case StateShadow:
		s.shadow = rel
		rep.ShadowVersion = rel.Version
		if s.active == nil {
			s.active = rel
			s.state = StateShadow
		} else {
			rep.State = s.state
		}
	case StateEnforcing:
		if s.active != nil && s.active != rel {
			s.retained = s.active
			rep.RetainedVersion = s.retained.Version
		}
		s.active = rel
		s.state = StateEnforcing
	case StateRolledBack:
		s.state = StateRolledBack
		if s.retained != nil {
			s.active = s.retained
			rep.Version = s.retained.Version
			rep.RulesVersion = s.retained.Rules.Version()
			rep.ModelVersion = ""
			if s.retained.Model != nil {
				rep.ModelVersion = s.retained.Model.Version()
			}
		}
		rep.RolledBack = true
	}
	rep.State = s.state
	if s.retained != nil {
		rep.RetainedVersion = s.retained.Version
	}
	return rep, nil
}

// SetActive installs a release directly. It exists for tests and for the harness that builds a
// release in a temporary directory; production loads through Apply so the signature is checked.
func (s *Store) SetActive(rel *Release) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active != rel {
		s.retained = s.active
	}
	s.active = rel
	s.state = rel.State
}

// Active returns the release used for classification, or nil.
func (s *Store) Active() *Release {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// Shadow returns the release being shadow-evaluated alongside the active one, or nil.
func (s *Store) Shadow() *Release {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shadow
}

// Retained returns the rollback target, or nil.
func (s *Store) Retained() *Release {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retained
}

// EffectiveState is the enforcement state the host must apply. It is the active release's
// manifest state, except that a `rolled_back` transition keeps enforcement off until a later
// enforcing release.
func (s *Store) EffectiveState() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Failure returns the last rejected load, or nil.
func (s *Store) Failure() *Failure {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failure == nil {
		return nil
	}
	f := *s.failure
	return &f
}

// Counters reports loads and rejections for the health channel.
func (s *Store) Counters() (loads, rejections uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loads, s.rejections
}

// causeOf maps a load error to a short, stable cause string for the counters and the report.
func causeOf(err error) string {
	switch {
	case errors.Is(err, ErrSignature):
		return "signature"
	case errors.Is(err, ErrUnknownKey):
		return "unknown_key"
	case errors.Is(err, ErrDigest):
		return "digest"
	case errors.Is(err, ErrRules):
		return "rules_rejected"
	case errors.Is(err, ErrModel):
		return "model_rejected"
	case errors.Is(err, ErrManifest):
		return "manifest"
	default:
		return "load_failed"
	}
}

// fileVersion guesses the version from the directory name, for the failure record: a rejected
// release may not have a readable manifest, and "which release was rejected" must still be
// answerable.
func fileVersion(dir string) string {
	base := filepath.Base(dir)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return dir
	}
	return base
}
