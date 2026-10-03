package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/protocol"
)

// Corpus is the fixed corpus the equivalence test and the latency measurement run over.
//
// A corpus case is (mode, media type, bytes); it carries no identity, because the request type it
// becomes has no field that could carry one (§3.3).
type Corpus struct {
	Name     string `json:"name"`
	BudgetMS int64  `json:"budget_ms,omitempty"`
	Cases    []Case `json:"cases"`
}

// Case is one corpus entry. Expect is optional and is what makes the corpus a *test* rather than
// merely a fixture: the equivalence run must produce these classes on both targets.
type Case struct {
	ID         string       `json:"id"`
	Mode       string       `json:"mode"`
	MediaType  string       `json:"media_type,omitempty"`
	Content    string       `json:"content,omitempty"`
	ContentB64 string       `json:"content_base64,omitempty"`
	BudgetMS   int64        `json:"budget_ms,omitempty"`
	Expect     *Expectation `json:"expect,omitempty"`
}

// Expectation is the label shape a case must produce.
type Expectation struct {
	Confidence string   `json:"confidence,omitempty"`
	Classes    []string `json:"classes,omitempty"`
	// AbsentClasses must not appear, which is how the validators are tested: a 16-digit number
	// that fails Luhn must not produce a payment_card label.
	AbsentClasses []string `json:"absent_classes,omitempty"`
	Degraded      *bool    `json:"degraded,omitempty"`
}

// Record is the canonical, timing-free verdict the equivalence test diffs byte for byte.
//
// It deliberately excludes stage durations, counters and the release's model digests: none of
// them is a label, and including a duration would make the comparison a timing test on two
// different machines. §9.1's property is about labels.
type Record struct {
	ID                string              `json:"id"`
	Labels            []protocol.Label    `json:"labels"`
	Confidence        protocol.Confidence `json:"confidence"`
	ClassifierVersion string              `json:"classifier_version"`
	Action            string              `json:"action"`
	State             string              `json:"release_state"`
	Degraded          bool                `json:"degraded"`
	DegradedStages    []string            `json:"degraded_stages"`
	Digest            string              `json:"content_digest,omitempty"`

	// Excerpt is the M2 minimised excerpt. It is part of the canonical record because the
	// contract requires it at M2 and forbids it at M3, and that difference must be identical on
	// both targets.
	Excerpt *protocol.Excerpt `json:"content_excerpt,omitempty"`

	// partial_labels is §9.4's "emit labels found" when a stage exhausted its budget. The
	// protocol response carries none (a degraded response cannot carry confident verdicts), so
	// the audit path's labels are recorded here.
	PartialLabels []protocol.Label `json:"partial_labels,omitempty"`
}

// LoadCorpus reads and validates a corpus file.
func LoadCorpus(path string) (*Corpus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Corpus
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Cases) == 0 {
		return nil, fmt.Errorf("%s: corpus has no cases", path)
	}
	seen := map[string]bool{}
	for i, cs := range c.Cases {
		if cs.ID == "" {
			return nil, fmt.Errorf("%s: case %d has no id", path, i)
		}
		if seen[cs.ID] {
			return nil, fmt.Errorf("%s: case id %q appears twice", path, cs.ID)
		}
		seen[cs.ID] = true
		if cs.Mode == "" {
			return nil, fmt.Errorf("%s: case %q has no mode", path, cs.ID)
		}
		if cs.ContentB64 != "" && cs.Content != "" {
			return nil, fmt.Errorf("%s: case %q sets both content and content_base64", path, cs.ID)
		}
	}
	return &c, nil
}

// Bytes is the case's payload.
func (c Case) Bytes() ([]byte, error) {
	if c.ContentB64 != "" {
		return base64.StdEncoding.DecodeString(c.ContentB64)
	}
	return []byte(c.Content), nil
}

// Request builds the protocol request. It is the *only* request type the classifier accepts, and
// it has no identity field (§3.3).
func (c Case) Request(budgetMS int64) (protocol.ClassifyRequest, error) {
	content, err := c.Bytes()
	if err != nil {
		return protocol.ClassifyRequest{}, fmt.Errorf("case %q: content_base64: %w", c.ID, err)
	}
	if c.BudgetMS > 0 {
		budgetMS = c.BudgetMS
	}
	return protocol.ClassifyRequest{
		Content:   content,
		Mode:      protocol.CollectionMode(c.Mode),
		MediaType: c.MediaType,
		BudgetMS:  budgetMS,
	}, nil
}

// Run classifies every case and returns the canonical records.
func (c *Corpus) Run(host *classify.Host) ([]Record, error) {
	out := make([]Record, 0, len(c.Cases))
	for _, cs := range c.Cases {
		req, err := cs.Request(c.BudgetMS)
		if err != nil {
			return nil, err
		}
		v := host.Classify(context.Background(), req)
		out = append(out, canonical(cs.ID, v))
	}
	return out, nil
}

func canonical(id string, v classify.Verdict) Record {
	labels := append([]protocol.Label(nil), v.Response.Labels...)
	if labels == nil {
		labels = []protocol.Label{}
	}
	sort.SliceStable(labels, func(i, j int) bool {
		if labels[i].Class != labels[j].Class {
			return labels[i].Class < labels[j].Class
		}
		return labels[i].RuleID < labels[j].RuleID
	})
	stages := make([]string, 0, 4)
	for _, s := range v.Response.Stages {
		if s.Failed || s.Truncated {
			stages = append(stages, s.Stage)
		}
	}
	sort.Strings(stages)
	partial := append([]protocol.Label(nil), v.PartialLabels...)
	sort.SliceStable(partial, func(i, j int) bool { return partial[i].Class < partial[j].Class })
	return Record{
		ID:                id,
		Labels:            labels,
		Confidence:        v.Response.Confidence,
		ClassifierVersion: v.Response.ClassifierVersion,
		Action:            v.Action,
		State:             string(v.ReleaseState),
		Degraded:          v.Response.Confidence == protocol.ConfidenceDegraded,
		DegradedStages:    stages,
		Digest:            v.Digest,
		PartialLabels:     partial,
		Excerpt:           v.Response.Excerpt,
	}
}

// CheckExpectations reports every case whose record does not match its declared expectation.
func (c *Corpus) CheckExpectations(records []Record) []string {
	byID := map[string]Record{}
	for _, r := range records {
		byID[r.ID] = r
	}
	var problems []string
	for _, cs := range c.Cases {
		if cs.Expect == nil {
			continue
		}
		r, ok := byID[cs.ID]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: no record", cs.ID))
			continue
		}
		classes := map[string]bool{}
		for _, l := range r.Labels {
			classes[l.Class] = true
		}
		for _, want := range cs.Expect.Classes {
			if !classes[want] {
				problems = append(problems, fmt.Sprintf("%s: expected class %q, got %v", cs.ID, want, r.Labels))
			}
		}
		for _, absent := range cs.Expect.AbsentClasses {
			if classes[absent] {
				problems = append(problems, fmt.Sprintf("%s: class %q must not appear, got %v", cs.ID, absent, r.Labels))
			}
		}
		if cs.Expect.Confidence != "" && string(r.Confidence) != cs.Expect.Confidence {
			problems = append(problems, fmt.Sprintf("%s: expected confidence %q, got %q", cs.ID, cs.Expect.Confidence, r.Confidence))
		}
		if cs.Expect.Degraded != nil && r.Degraded != *cs.Expect.Degraded {
			problems = append(problems, fmt.Sprintf("%s: expected degraded=%v, got %v", cs.ID, *cs.Expect.Degraded, r.Degraded))
		}
	}
	return problems
}

func writeJSON(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if path == "" || path == "-" {
		_, err = os.Stdout.Write(body)
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// runClassify is `classifier-host classify`. It is the subcommand both targets run for the
// equivalence test.
func runClassify(args []string) int {
	fs := flag.NewFlagSet("classify", flag.ContinueOnError)
	releaseDir := fs.String("release", "", "signed release directory")
	pubkey := fs.String("pubkey", "", "hex ed25519 release-signing public key")
	corpusPath := fs.String("corpus", "", "corpus JSON file")
	out := fs.String("out", "-", "output path for the canonical records ('-' for stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *corpusPath == "" {
		return fatalf("classify: --corpus is required")
	}
	r, err := loadRig(*releaseDir, *pubkey)
	if err != nil {
		return fatalf("classify: %v", err)
	}
	c, err := LoadCorpus(*corpusPath)
	if err != nil {
		return fatalf("classify: %v", err)
	}
	records, err := c.Run(r.host)
	if err != nil {
		return fatalf("classify: %v", err)
	}
	if err := writeJSON(*out, records); err != nil {
		return fatalf("classify: writing %s: %v", *out, err)
	}
	if problems := c.CheckExpectations(records); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "classify: expectation: "+p)
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "classify: %d cases, release %s, target %s/%s\n",
		len(records), records[0].ClassifierVersion, osName(), archName())
	return 0
}
