// Package model is the fuzzy-class stage of docs/01-collectors.md §9.2: "The model runs on what
// the rules did not resolve, for the fuzzy classes brief §6 lists" (customer_pii, source_code,
// legal, health).
//
// Two properties matter more here than modelling quality, and both are structural:
//
//   - **Determinism across the two targets.** §9.1 requires one source to produce byte-identical
//     labels natively and in the wasm copy. A model that computes in float64 through a
//     transcendental (a sigmoid, an exponential) is at the mercy of the platform's libm: amd64
//     has assembly implementations that wasm does not, and the last-ulp difference can move a
//     score across a threshold. Scoring here is therefore **fixed-point integer arithmetic**
//     (int64 sums over integer weights) converted to float64 once by exact division, so the two
//     targets agree bit for bit by construction rather than by testing.
//   - **The artefact is signed data, verified before use.** A missing, unloadable or unverified
//     artefact is §9.7's "model artefact was missing, unloadable or failed to verify" — degraded,
//     never a confident label and never silently "no rules".
//
// The artefact is a sparse linear scorer over token counts. That is deliberately modest: §9.2
// puts the rules and validators first, the model is the backstop for classes no rule resolves,
// and a heavier artefact would be a heavier thing to sign, verify and load on the interactive
// path.
package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Caps bound the artefact at load time, the same way §9.5 bounds the rules.
type Caps struct {
	MaxArtefactBytes int
	MaxClasses       int
	MaxFeatures      int
	MaxTokenBytes    int
	MaxVersionBytes  int
	MaxTokens        int // tokens read from the classified text, per call
	MaxCount         int // per-feature count saturation
}

// DefaultCaps is the shipped bound.
func DefaultCaps() Caps {
	return Caps{
		MaxArtefactBytes: 1 << 20,
		MaxClasses:       32,
		MaxFeatures:      4096,
		MaxTokenBytes:    64,
		MaxVersionBytes:  64,
		MaxTokens:        20000,
		MaxCount:         4,
	}
}

// ErrUnavailable is returned when no artefact was supplied: the model stage is skipped and the
// response is degraded (§9.7).
var ErrUnavailable = errors.New("model: no model artefact is loaded")

// ErrVerify is returned when the artefact does not match its signed digest.
var ErrVerify = errors.New("model: artefact does not match its signed digest")

// ErrRejected wraps a load-time rejection of the artefact's contents.
var ErrRejected = errors.New("model: artefact rejected")

// Feature is one literal token or space-separated phrase with an integer weight.
type Feature struct {
	Token  string `json:"token"`
	Weight int64  `json:"weight"`
	// MaxCount saturates the count, so a token repeated a thousand times cannot dominate.
	// Zero means the artefact's default.
	MaxCount int `json:"max_count,omitempty"`
}

// ClassModel is one fuzzy class.
type ClassModel struct {
	Class     string    `json:"class"`
	Threshold int64     `json:"threshold"`
	Bias      int64     `json:"bias"`
	Features  []Feature `json:"features"`
}

// Artefact is the signed model document.
type Artefact struct {
	Version string       `json:"version"`
	Scale   int64        `json:"scale"`
	Classes []ClassModel `json:"classes"`
}

// Model is a loaded, validated artefact.
type Model struct {
	artefact Artefact
	caps     Caps
}

// Load verifies the artefact against its signed digest and validates it whole. wantDigest is
// "sha256:<hex>"; an empty digest is a caller defect and is refused, because §9.7 makes
// "failed to verify" a degraded case and a caller that skips verification would turn it into a
// silent success.
func Load(raw []byte, wantDigest string, caps Caps) (*Model, error) {
	if caps.MaxClasses == 0 {
		caps = DefaultCaps()
	}
	if wantDigest == "" {
		return nil, fmt.Errorf("%w: no digest to verify against", ErrVerify)
	}
	sum := sha256.Sum256(raw)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantDigest) {
		return nil, fmt.Errorf("%w: artefact is %s, manifest declares %s", ErrVerify, got, wantDigest)
	}
	if len(raw) > caps.MaxArtefactBytes {
		return nil, fmt.Errorf("%w: artefact is %d bytes, over the %d-byte cap", ErrRejected, len(raw), caps.MaxArtefactBytes)
	}
	var a Artefact
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing content after the first JSON value", ErrRejected)
	}
	if a.Version == "" || len(a.Version) > caps.MaxVersionBytes {
		return nil, fmt.Errorf("%w: version is empty or over %d bytes", ErrRejected, caps.MaxVersionBytes)
	}
	if a.Scale <= 0 || a.Scale > 1<<40 {
		return nil, fmt.Errorf("%w: scale %d is outside 1..2^40", ErrRejected, a.Scale)
	}
	if len(a.Classes) == 0 || len(a.Classes) > caps.MaxClasses {
		return nil, fmt.Errorf("%w: %d classes, outside 1..%d", ErrRejected, len(a.Classes), caps.MaxClasses)
	}
	seen := map[string]bool{}
	features := 0
	for i := range a.Classes {
		c := &a.Classes[i]
		if !classPattern.MatchString(c.Class) {
			return nil, fmt.Errorf("%w: class %q does not match %s", ErrRejected, c.Class, classPattern)
		}
		if seen[c.Class] {
			return nil, fmt.Errorf("%w: class %q declared twice", ErrRejected, c.Class)
		}
		seen[c.Class] = true
		if c.Threshold < 0 || c.Threshold > a.Scale {
			return nil, fmt.Errorf("%w: class %q threshold %d outside 0..scale", ErrRejected, c.Class, c.Threshold)
		}
		if len(c.Features) == 0 {
			return nil, fmt.Errorf("%w: class %q has no features", ErrRejected, c.Class)
		}
		features += len(c.Features)
		for j := range c.Features {
			f := &c.Features[j]
			if f.Token == "" || len(f.Token) > caps.MaxTokenBytes {
				return nil, fmt.Errorf("%w: class %q feature %d token is empty or over %d bytes", ErrRejected, c.Class, j, caps.MaxTokenBytes)
			}
			if f.Weight == 0 {
				return nil, fmt.Errorf("%w: class %q feature %q has zero weight", ErrRejected, c.Class, f.Token)
			}
			if f.Weight > a.Scale || f.Weight < -a.Scale {
				return nil, fmt.Errorf("%w: class %q feature %q weight %d exceeds scale", ErrRejected, c.Class, f.Token, f.Weight)
			}
			if f.MaxCount < 0 || f.MaxCount > 1024 {
				return nil, fmt.Errorf("%w: class %q feature %q max_count %d out of range", ErrRejected, c.Class, f.Token, f.MaxCount)
			}
			if f.MaxCount == 0 {
				f.MaxCount = caps.MaxCount
			}
			f.Token = strings.ToLower(f.Token)
		}
	}
	if features > caps.MaxFeatures {
		return nil, fmt.Errorf("%w: %d features, over the %d cap", ErrRejected, features, caps.MaxFeatures)
	}
	return &Model{artefact: a, caps: caps}, nil
}

var classPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Version is the artefact version, which the classifier release folds into classifier_version.
func (m *Model) Version() string { return m.artefact.Version }

// Classes lists the artefact's classes in declaration order.
func (m *Model) Classes() []string {
	out := make([]string, 0, len(m.artefact.Classes))
	for _, c := range m.artefact.Classes {
		out = append(out, c.Class)
	}
	return out
}

// Prediction is one class's score.
type Prediction struct {
	Class     string
	Score     float64 // Fixed / Scale, by exact integer division
	Fixed     int64
	Threshold float64
}

// Score runs the model. skip names classes the rules already resolved: §9.2 says the model runs
// on what the rules did not resolve, so a class with a defensible rules verdict is not
// second-guessed by a fuzzy score.
//
// Deadline is checked between classes, so a model that is slow degrades the model stage rather
// than the submission (§9.4).
func (m *Model) Score(text string, skip map[string]bool, deadlineExpired func() bool) []Prediction {
	tokens := tokenise(text, m.caps.MaxTokens)
	counts := map[string]int{}
	for _, t := range tokens {
		counts[t]++
	}
	out := make([]Prediction, 0, len(m.artefact.Classes))
	for _, c := range m.artefact.Classes {
		if deadlineExpired != nil && deadlineExpired() {
			break
		}
		if skip[c.Class] {
			continue
		}
		sum := c.Bias
		for _, f := range c.Features {
			n := countFeature(f, tokens, counts)
			if n > f.MaxCount {
				n = f.MaxCount
			}
			sum += f.Weight * int64(n)
		}
		if sum < 0 {
			sum = 0
		}
		if sum > m.artefact.Scale {
			sum = m.artefact.Scale
		}
		if sum < c.Threshold {
			continue
		}
		out = append(out, Prediction{
			Class:     c.Class,
			Score:     float64(sum) / float64(m.artefact.Scale),
			Fixed:     sum,
			Threshold: float64(c.Threshold) / float64(m.artefact.Scale),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Fixed != out[j].Fixed {
			return out[i].Fixed > out[j].Fixed
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// countFeature counts a feature's occurrences. A single-token feature is a map lookup; a phrase
// is matched over the token stream driven by the first word, so the cost is bounded by the
// token count rather than by a re-scan of the raw text.
func countFeature(f Feature, tokens []string, counts map[string]int) int {
	if !strings.Contains(f.Token, " ") {
		return counts[f.Token]
	}
	parts := strings.Split(f.Token, " ")
	n := 0
	for i := 0; i+len(parts) <= len(tokens); i++ {
		if tokens[i] != parts[0] {
			continue
		}
		ok := true
		for j := 1; j < len(parts); j++ {
			if tokens[i+j] != parts[j] {
				ok = false
				break
			}
		}
		if ok {
			n++
			i += len(parts) - 1
			if n >= f.MaxCount {
				return n
			}
		}
	}
	return n
}

// tokenise lowercases and splits on anything that is not a letter, digit, underscore or
// hyphen. The result is capped: the model is a fuzzy backstop, and reading a megabyte of
// attacker text into it is not the point of the stage.
func tokenise(s string, max int) []string {
	out := make([]string, 0, 256)
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		word := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if word {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, strings.ToLower(s[start:i]))
			start = -1
			if len(out) >= max {
				return out
			}
		}
	}
	if start >= 0 {
		out = append(out, strings.ToLower(s[start:]))
	}
	return out
}
