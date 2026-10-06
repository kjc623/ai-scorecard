package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Cause is the closed list of verification-failure causes. Each maps onto protocol.Detail's
// closed vocabulary: a cause string invented locally would be a coverage cause the reporting
// layer cannot group.
type Cause string

// The causes, in the order the verification chain checks them.
const (
	CauseSignatureInvalid  Cause = "bundle_signature_invalid"
	CauseSchemaInvalid     Cause = "bundle_schema_invalid"
	CauseVersionRegression Cause = "bundle_version_regression"
	CauseAccepted          Cause = ""
)

// Valid reports whether the cause is in the closed set.
func (c Cause) Valid() bool {
	switch c {
	case CauseSignatureInvalid, CauseSchemaInvalid, CauseVersionRegression:
		return true
	default:
		return false
	}
}

// Detail maps the cause to the health channel's closed vocabulary.
func (c Cause) Detail() protocol.Detail {
	switch c {
	case CauseSignatureInvalid:
		return protocol.DetailBundleSignatureInvalid
	case CauseSchemaInvalid:
		return protocol.DetailBundleSchemaInvalid
	case CauseVersionRegression:
		return protocol.DetailBundleVersionRegression
	default:
		return protocol.DetailNone
	}
}

// SignedBundle is the envelope around a bundle payload. The signature covers the payload bytes
// exactly as received, never a re-serialisation of the parsed structure: a canonical-JSON
// requirement would add a second way for two implementations to disagree, and the payload is
// only ever read through this envelope.
type SignedBundle struct {
	KeyID     string          `json:"key_id"`
	Algorithm string          `json:"algorithm"`
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"` // base64 (standard encoding)
}

// Verifier checks a bundle against the tenant's pinned policy key. There is no method that
// parses a bundle without verifying it, and no option to skip the check.
type Verifier struct {
	keyID string
	pub   ed25519.PublicKey
}

// NewVerifier pins one policy key. key_id mismatch is treated as a signature failure, not a
// "try the other key": a bundle signed by a key the device does not hold is not policy.
func NewVerifier(keyID string, pub ed25519.PublicKey) (*Verifier, error) {
	if keyID == "" {
		return nil, fmt.Errorf("policy: verifier needs a key id so a rotation is attributable")
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("policy: verifier key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	return &Verifier{keyID: keyID, pub: pub}, nil
}

// KeyID returns the pinned key's id.
func (v *Verifier) KeyID() string { return v.keyID }

// VerifyEnvelope checks the signature and returns the raw payload. The chain in Open then
// applies schema, version and artefact checks in order, stopping at the first failure.
func (v *Verifier) VerifyEnvelope(raw []byte) (json.RawMessage, error) {
	var sb SignedBundle
	if err := json.Unmarshal(raw, &sb); err != nil {
		return nil, &Failure{Cause: CauseSchemaInvalid, Err: fmt.Errorf("signed bundle envelope is not JSON: %w", err)}
	}
	if !strings.EqualFold(sb.Algorithm, "ed25519") {
		return nil, &Failure{Cause: CauseSignatureInvalid, Err: fmt.Errorf("signed bundle names algorithm %q", sb.Algorithm)}
	}
	if sb.KeyID != v.keyID {
		return nil, &Failure{Cause: CauseSignatureInvalid, Err: fmt.Errorf("bundle key id %q is not the pinned policy key %q", sb.KeyID, v.keyID)}
	}
	sig, err := base64.StdEncoding.DecodeString(sb.Signature)
	if err != nil {
		return nil, &Failure{Cause: CauseSignatureInvalid, Err: fmt.Errorf("signature is not base64: %w", err)}
	}
	if len(sb.Payload) == 0 {
		return nil, &Failure{Cause: CauseSchemaInvalid, Err: fmt.Errorf("signed bundle carries no payload")}
	}
	if !ed25519.Verify(v.pub, sb.Payload, sig) {
		return nil, &Failure{Cause: CauseSignatureInvalid, Err: fmt.Errorf("bundle signature does not verify under the pinned policy key")}
	}
	return sb.Payload, nil
}

// Open runs the verification chain: signature, then schema, then version.
// The bundle in force is the caller's to compare against, because only the Store knows it.
func (v *Verifier) Open(raw []byte, inForce *Bundle) (*Bundle, error) {
	payload, err := v.VerifyEnvelope(raw)
	if err != nil {
		return nil, err
	}
	b, err := decode(payload)
	if err != nil {
		return nil, &Failure{Cause: CauseSchemaInvalid, Err: err}
	}
	if inForce != nil {
		older, unknown := versionOlder(b.Version, inForce.Version)
		if unknown {
			return nil, &Failure{Cause: CauseVersionRegression, Err: fmt.Errorf(
				"bundle version %q cannot be ordered against the version in force %q; a downgrade must not be reachable, so the previous bundle stays enforced",
				b.Version, inForce.Version)}
		}
		if older {
			return nil, &Failure{Cause: CauseVersionRegression, Err: fmt.Errorf(
				"bundle version %q is older than the version in force %q", b.Version, inForce.Version)}
		}
	}
	return b, nil
}

// Failure is a verification failure with its cause.
type Failure struct {
	Cause Cause
	Err   error
}

// Error implements error.
func (f *Failure) Error() string { return string(f.Cause) + ": " + f.Err.Error() }

// Unwrap exposes the underlying error.
func (f *Failure) Unwrap() error { return f.Err }

// CauseOf extracts the cause from an error, defaulting to schema-invalid for an unattributed
// failure — the conservative direction, because an unattributed failure must not be mistaken
// for an accepted bundle.
func CauseOf(err error) Cause {
	var f *Failure
	if e, ok := err.(*Failure); ok {
		f = e
	}
	if f == nil {
		return CauseSchemaInvalid
	}
	if !f.Cause.Valid() {
		return CauseSchemaInvalid
	}
	return f.Cause
}

// versionOlder compares two bundle versions, newest-wins. It accepts dot-separated numeric
// components ("2026.10.02.3") and refuses an ordering it cannot perform, because the safe
// direction for an unorderable version is to retain the previous bundle.
func versionOlder(candidate, inForce string) (older bool, unknown bool) {
	c, ok1 := parseVersion(candidate)
	f, ok2 := parseVersion(inForce)
	if !ok1 || !ok2 {
		if candidate == inForce {
			return false, false // an identical opaque version is idempotent, not a downgrade
		}
		return false, true
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
			return a < b, false
		}
	}
	return false, false
}

func parseVersion(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// Sign produces a SignedBundle. It exists for tests and for a deployment tool that mints
// policy; the device never signs a bundle it enforces.
func Sign(keyID string, priv ed25519.PrivateKey, b *Bundle) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, payload)
	return json.Marshal(SignedBundle{
		KeyID:     keyID,
		Algorithm: "ed25519",
		Payload:   payload,
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
}

// Outcome is what happened to a bundle that arrived on a policy poll.
type Outcome string

const (
	// OutcomeAccepted means the bundle verified and is now in force.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeUnchanged means the bundle is the one already in force; a 304 or a re-poll.
	OutcomeUnchanged Outcome = "unchanged"
	// OutcomeRetainedPrevious means verification failed and the previous bundle stays in
	// force — not partially applied, not applied with the check skipped.
	OutcomeRetainedPrevious Outcome = "retained_previous"
	// OutcomeFellToM0 means verification failed with no previous bundle, so the device is at
	// M0: metadata only, no content read.
	OutcomeFellToM0 Outcome = "fell_to_m0"
)

// Result is the reportable result of one policy poll.
type Result struct {
	Outcome  Outcome
	Cause    Cause
	Version  string // version in force after the poll ("" when M0)
	Failures int    // consecutive verification failures, for escalation
	Severity string // info | warning | critical
	Err      error
}

// Severity levels: repeated failures escalate rather than becoming a
// request storm.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// escalationThreshold is the consecutive-failure count at which severity escalates.
const escalationThreshold = 3

// Store holds the bundle in force. It is the only writer of that field, so "which policy is
// the device enforcing" has exactly one answer at any instant.
type Store struct {
	verifier *Verifier

	mu         sync.Mutex
	inForce    *Bundle
	inForceRaw []byte
	failures   int
	lastCause  Cause
}

// NewStore builds a store around a pinned verifier. A nil verifier is a programming error:
// the store refuses to exist rather than accepting unverified bundles.
func NewStore(v *Verifier) (*Store, error) {
	if v == nil {
		return nil, fmt.Errorf("policy: store needs a verifier; an unverified bundle must never be enforceable")
	}
	return &Store{verifier: v}, nil
}

// InForce returns the bundle being enforced, or nil when there is none — and nil is what
// every caller must read as M0, never as "unrestricted".
func (s *Store) InForce() *Bundle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inForce
}

// InForceRaw returns the last accepted bundle's bytes, for the extension's policy_sync.
func (s *Store) InForceRaw() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inForceRaw
}

// Failures reports consecutive verification failures.
func (s *Store) Failures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures
}

// LastCause reports the most recent failure cause, zero when the last poll succeeded.
func (s *Store) LastCause() Cause {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCause
}

// Apply verifies a bundle and, if it verifies, puts it in force. On any failure the bundle
// in force does not change; the only way to a different state is a *verified* bundle.
func (s *Store) Apply(raw []byte) Result {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := s.verifier.Open(raw, s.inForce)
	if err != nil {
		cause := CauseOf(err)
		s.failures++
		s.lastCause = cause
		res := Result{
			Outcome:  OutcomeRetainedPrevious,
			Cause:    cause,
			Failures: s.failures,
			Severity: severityFor(s.failures),
			Err:      err,
		}
		if s.inForce == nil {
			res.Outcome = OutcomeFellToM0
		} else {
			res.Version = s.inForce.Version
		}
		return res
	}
	s.failures = 0
	s.lastCause = CauseAccepted
	if s.inForce != nil && b.Version == s.inForce.Version {
		// Same version, verified again: refresh the bytes (a re-signed but identical bundle
		// is still the version in force) without reporting a change.
		s.inForce = b
		s.inForceRaw = append([]byte(nil), raw...)
		return Result{Outcome: OutcomeUnchanged, Version: b.Version, Severity: SeverityInfo}
	}
	s.inForce = b
	s.inForceRaw = append([]byte(nil), raw...)
	return Result{Outcome: OutcomeAccepted, Version: b.Version, Severity: SeverityInfo}
}

// Withdraw takes the bundle out of force because the server stated, over the device's
// authenticated channel, that the tenant has none (GET /v1/policy answered 404). The device
// is then at M0. It is the one change of state that does not need a verified bundle, and it is
// safe for the reason Apply's rule exists: M0 is the floor, so withdrawing can only narrow what the
// device does, never widen it.
func (s *Store) Withdraw() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inForce = nil
	s.inForceRaw = nil
	s.failures = 0
	s.lastCause = CauseAccepted
	return Result{Outcome: OutcomeFellToM0, Severity: SeverityWarning}
}

// PollBackoff backs polling off after repeated failures, so a fleet-wide signing
// problem does not become a request storm. The bundle stays in force throughout.
func (s *Store) PollBackoff(base time.Duration) time.Duration {
	s.mu.Lock()
	f := s.failures
	s.mu.Unlock()
	if f < escalationThreshold {
		return base
	}
	shift := f - escalationThreshold + 1
	if shift > 6 { // cap: one hour at a 1-minute base, and never unbounded
		shift = 6
	}
	return base * time.Duration(1<<uint(shift))
}

func severityFor(failures int) string {
	switch {
	case failures >= escalationThreshold:
		return SeverityCritical
	case failures > 0:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}
