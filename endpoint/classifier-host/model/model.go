// Package model is the classifier's keyword scorer: for each class, a sparse linear model over
// token and phrase counts. It runs after the rules and scores only the classes the rules did not
// already label, which is where vocabulary rather than a single pattern is the evidence.
//
// Scores are integer sums divided once by the artefact's scale, so the same text always produces
// the same score on every platform.
package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Limits an artefact must stay within.
const (
	MaxArtefactBytes = 1 << 20
	MaxClasses       = 32
	MaxFeatures      = 4096
	MaxTokenBytes    = 64
	MaxVersionBytes  = 64
	maxTextTokens    = 20000
	defaultMaxCount  = 4
)

// Feature is a lowercase token, or a space-separated phrase, with an integer weight.
type Feature struct {
	Token  string `json:"token"`
	Weight int64  `json:"weight"`
	// MaxCount caps how many occurrences count, so one repeated word cannot dominate. Zero means
	// four.
	MaxCount int `json:"max_count,omitempty"`
}

// Class is one scored class. A class is reported when its clamped sum reaches Threshold.
type Class struct {
	Class     string    `json:"class"`
	Threshold int64     `json:"threshold"`
	Bias      int64     `json:"bias,omitempty"`
	Features  []Feature `json:"features"`
}

// Artefact is the model file.
type Artefact struct {
	Version string  `json:"version"`
	Scale   int64   `json:"scale"`
	Classes []Class `json:"classes"`
}

// Model is a validated artefact, safe for concurrent use.
type Model struct {
	a Artefact
}

// ErrRejected wraps every rejection of an artefact's contents.
var ErrRejected = errors.New("model: artefact rejected")

var classPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Parse validates an artefact as a whole.
func Parse(raw []byte) (*Model, error) {
	if len(raw) > MaxArtefactBytes {
		return nil, fmt.Errorf("%w: %d bytes, over the %d-byte limit", ErrRejected, len(raw), MaxArtefactBytes)
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
	if a.Version == "" || len(a.Version) > MaxVersionBytes {
		return nil, fmt.Errorf("%w: version is empty or over %d bytes", ErrRejected, MaxVersionBytes)
	}
	if a.Scale <= 0 || a.Scale > 1<<40 {
		return nil, fmt.Errorf("%w: scale %d is outside 1..2^40", ErrRejected, a.Scale)
	}
	if len(a.Classes) == 0 || len(a.Classes) > MaxClasses {
		return nil, fmt.Errorf("%w: %d classes, outside 1..%d", ErrRejected, len(a.Classes), MaxClasses)
	}
	seen := map[string]bool{}
	features := 0
	for i := range a.Classes {
		c := &a.Classes[i]
		if !classPattern.MatchString(c.Class) || seen[c.Class] {
			return nil, fmt.Errorf("%w: class %q is malformed or declared twice", ErrRejected, c.Class)
		}
		seen[c.Class] = true
		if c.Threshold < 0 || c.Threshold > a.Scale {
			return nil, fmt.Errorf("%w: class %q threshold %d is outside 0..scale", ErrRejected, c.Class, c.Threshold)
		}
		if len(c.Features) == 0 {
			return nil, fmt.Errorf("%w: class %q has no features", ErrRejected, c.Class)
		}
		features += len(c.Features)
		for j := range c.Features {
			f := &c.Features[j]
			switch {
			case f.Token == "" || len(f.Token) > MaxTokenBytes:
				return nil, fmt.Errorf("%w: class %q feature %d token is empty or over %d bytes", ErrRejected, c.Class, j, MaxTokenBytes)
			case f.Weight == 0 || f.Weight > a.Scale || f.Weight < -a.Scale:
				return nil, fmt.Errorf("%w: class %q feature %q weight %d is zero or exceeds the scale", ErrRejected, c.Class, f.Token, f.Weight)
			case f.MaxCount < 0 || f.MaxCount > 1024:
				return nil, fmt.Errorf("%w: class %q feature %q max_count %d is outside 0..1024", ErrRejected, c.Class, f.Token, f.MaxCount)
			}
			if f.MaxCount == 0 {
				f.MaxCount = defaultMaxCount
			}
			f.Token = strings.ToLower(f.Token)
		}
	}
	if features > MaxFeatures {
		return nil, fmt.Errorf("%w: %d features, over the %d limit", ErrRejected, features, MaxFeatures)
	}
	return &Model{a: a}, nil
}

// Version is the artefact's version.
func (m *Model) Version() string { return m.a.Version }

// Prediction is one class's score: Fixed divided by the artefact's scale.
type Prediction struct {
	Class string
	Score float64
	Fixed int64
}

// Score returns the classes whose score reaches their threshold, highest first, skipping the
// classes in skip. expired is checked between classes; when it reports true, scoring stops and
// the classes scored so far are returned.
func (m *Model) Score(text string, skip map[string]bool, expired func() bool) []Prediction {
	tokens := tokenise(text)
	counts := map[string]int{}
	for _, t := range tokens {
		counts[t]++
	}
	var out []Prediction
	for _, c := range m.a.Classes {
		if expired() {
			break
		}
		if skip[c.Class] {
			continue
		}
		sum := c.Bias
		for _, f := range c.Features {
			sum += f.Weight * int64(min(countFeature(f, tokens, counts), f.MaxCount))
		}
		sum = max(0, min(sum, m.a.Scale))
		if sum < c.Threshold {
			continue
		}
		out = append(out, Prediction{Class: c.Class, Score: float64(sum) / float64(m.a.Scale), Fixed: sum})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Fixed != out[j].Fixed {
			return out[i].Fixed > out[j].Fixed
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// countFeature counts a feature's occurrences: a map lookup for a token, a walk over the token
// stream for a phrase.
func countFeature(f Feature, tokens []string, counts map[string]int) int {
	parts := strings.Split(f.Token, " ")
	if len(parts) == 1 {
		return counts[f.Token]
	}
	n := 0
	for i := 0; i+len(parts) <= len(tokens) && n < f.MaxCount; i++ {
		match := true
		for j, p := range parts {
			if tokens[i+j] != p {
				match = false
				break
			}
		}
		if match {
			n++
			i += len(parts) - 1
		}
	}
	return n
}

// tokenise lowercases text and splits it on anything other than an ASCII letter, digit,
// underscore or hyphen, reading at most maxTextTokens tokens.
func tokenise(s string) []string {
	out := make([]string, 0, 256)
	start := -1
	for i := 0; i <= len(s); i++ {
		word := i < len(s) && (s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' ||
			s[i] >= '0' && s[i] <= '9' || s[i] == '_' || s[i] == '-')
		switch {
		case word && start < 0:
			start = i
		case !word && start >= 0:
			out = append(out, strings.ToLower(s[start:i]))
			start = -1
			if len(out) >= maxTextTokens {
				return out
			}
		}
	}
	return out
}
