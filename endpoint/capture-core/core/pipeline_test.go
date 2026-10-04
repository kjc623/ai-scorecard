package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

type recordingSink struct {
	mu      sync.Mutex
	entries []protocol.Entry
	err     error
}

func (s *recordingSink) Append(e protocol.Entry) (protocol.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return protocol.Entry{}, s.err
	}
	e.Seq = uint64(len(s.entries) + 1)
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *recordingSink) Stats() protocol.SpoolStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocol.SpoolStats{Depth: len(s.entries)}
}

func (s *recordingSink) last() protocol.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[len(s.entries)-1]
}

// tripwireReader fails the test if it is ever read. This is how "the M0 path never calls the
// content reader" is proved: not by inspecting the envelope, but by making the read itself
// fatal.
type tripwireReader struct {
	t     *testing.T
	reads int
	err   error
	body  []byte
}

func (r *tripwireReader) Read(context.Context) ([]byte, error) {
	r.reads++
	if r.err != nil {
		return nil, r.err
	}
	if r.t != nil {
		r.t.Error("content reader was called; the resolved mode did not permit reading content")
	}
	return r.body, nil
}

type countingReader struct {
	body  []byte
	reads int
}

func (r *countingReader) Read(context.Context) ([]byte, error) {
	r.reads++
	return r.body, nil
}

type stubClassifier struct {
	resp protocol.ClassifyResponse
	err  error
	got  []protocol.ClassifyRequest
}

func (c *stubClassifier) Classify(_ context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	c.got = append(c.got, req)
	if c.err != nil {
		return protocol.ClassifyResponse{}, c.err
	}
	return c.resp, nil
}

type stubContentStore struct {
	mu     sync.Mutex
	byID   map[string][]byte
	err    error
	putIDs []string
}

func (s *stubContentStore) Put(_ context.Context, id string, content []byte, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.byID == nil {
		s.byID = map[string][]byte{}
	}
	s.byID[id] = append([]byte(nil), content...)
	s.putIDs = append(s.putIDs, id)
	return nil
}

func (s *stubContentStore) HeldObjects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

func (s *stubContentStore) HeldBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, v := range s.byID {
		n += int64(len(v))
	}
	return n
}

// newTestPipeline installs a normaliser by default, because a pipeline *without* one takes the
// degraded path (that case has its own test).
func newTestPipeline(t *testing.T, sink Sink, bundle *policy.Bundle) *Pipeline {
	t.Helper()
	p, err := NewPipeline(sink, func() time.Time { return time.Unix(1_700_000_100, 0) }, func() string { return "11111111-2222-4333-8444-555555555555" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	p.SetIdentity(Identity{TenantID: "tenant-1", DeviceID: "device-1", UserRef: "user-1"})
	p.Bundles = func() *policy.Bundle { return bundle }
	p.Normalizer = dedup.IdentityNFC{}
	return p
}

func m0Bundle() *policy.Bundle {
	return testBundle("b1", protocol.ModeM0, map[string]protocol.CollectionMode{"tool": protocol.ModeM0})
}

// The tenant default is the tenant-wide permission ceiling (§11.3: the device resolves
// downward-only), so a bundle that grants a tool M1 states M3 as the default and M1 for the
// tool — which is how a scope entry lowers a mode rather than raising it.
func m1Bundle() *policy.Bundle {
	return testBundle("b2", protocol.ModeM3, map[string]protocol.CollectionMode{"tool": protocol.ModeM1})
}

func TestPipelineM0NeverCallsTheContentReader(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m0Bundle())
	reader := &tripwireReader{t: t}
	size := int64(4096)

	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       size,
		Content:         reader,
		Decision:        &protocol.Decision{RuleID: "R-1", Action: protocol.ActionLogged, DecidedLocally: true},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reader.reads != 0 {
		t.Fatalf("the reader was called %d times at M0", reader.reads)
	}
	if out.Mode != protocol.ModeM0 {
		t.Fatalf("mode = %q, want m0", out.Mode)
	}
	if !out.Emitted {
		t.Fatalf("M0 observation was not emitted: %+v", out)
	}

	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	for _, forbidden := range []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("M0 envelope carries %q", forbidden)
		}
	}
	for _, required := range []string{"size_bytes", "policy_decision", "dedup_key", "collection_mode", "tool_fingerprint", "occurred_at", "source"} {
		if _, ok := env[required]; !ok {
			t.Errorf("M0 envelope is missing %q", required)
		}
	}
	if got := string(env["collection_mode"]); got != `"m0"` {
		t.Errorf("collection_mode = %s", got)
	}
	// The dedup key is the weaker surrogate (Tier S) and is still a sha256: value, because the
	// key ladder always produces one.
	if !strings.HasPrefix(sink.last().DedupKey, "sha256:") {
		t.Fatalf("dedup key = %q, want a sha256: value", sink.last().DedupKey)
	}
	if c := p.Counters(protocol.RouteProxyTLS).Cumulative(); c[protocol.CounterObserved] != 1 || c[protocol.CounterEmitted] != 1 {
		t.Fatalf("counters = %v, want observed=1 emitted=1", c)
	}
}

// The gate is a refusal, not a silent empty read: asking for content at M0 is an error, so a
// defect cannot be expressed as "the reader returned nothing".
func TestReadContentRefusesAtM0(t *testing.T) {
	reader := &tripwireReader{t: t, body: []byte("secret")}
	if _, err := readContent(context.Background(), protocol.ModeM0, reader); !errors.Is(err, ErrModeForbidsRead) {
		t.Fatalf("err = %v, want ErrModeForbidsRead", err)
	}
	if reader.reads != 0 {
		t.Fatal("the gate called the reader before refusing")
	}
	if _, err := readContent(context.Background(), protocol.ModeM1, &countingReader{body: []byte("x")}); err != nil {
		t.Fatalf("M1 read refused: %v", err)
	}
	if reader.reads != 0 {
		t.Fatalf("the tripwire reader was called %d times", reader.reads)
	}
}

func TestBuildEnvelopeRefusesContentDerivedFieldsAtM0(t *testing.T) {
	size := int64(12)
	_, err := BuildEnvelope(EnvelopeInput{
		Identity:        Identity{TenantID: "t", DeviceID: "d", UserRef: "u"},
		EventID:         "e",
		Kind:            protocol.KindPrompt,
		Route:           protocol.RouteProxyTLS,
		Mode:            protocol.ModeM0,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(0, 0),
		DedupKey:        "sha256:" + strings.Repeat("a", 64),
		SizeBytes:       &size,
		ContentDigest:   "sha256:" + strings.Repeat("b", 64),
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
	})
	if !errors.Is(err, ErrContentAtM0) {
		t.Fatalf("err = %v, want ErrContentAtM0", err)
	}
}

func TestPipelineM1ReadsAndCarriesClassifierOutput(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	reader := &countingReader{body: []byte("Summarise this contract.  \r\n\r\n")}
	classifier := &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "legal_commercial", Score: 0.91, RuleID: "R_LEGAL"}},
		ClassifierVersion: "rel-2026-10-01",
		Confidence:        protocol.ConfidenceHigh,
	}}
	p.Classifier = classifier
	p.Normalizer = dedup.IdentityNFC{}

	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       32,
		MediaType:       "application/json",
		Content:         reader,
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return "Summarise this contract.", nil, nil
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reader.reads != 1 {
		t.Fatalf("reads = %d, want 1 at M1", reader.reads)
	}
	if out.Mode != protocol.ModeM1 || out.Confidence != protocol.ConfidenceHigh {
		t.Fatalf("outcome = %+v", out)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if _, ok := env["content_digest"]; !ok {
		t.Fatal("M1 envelope has no content_digest")
	}
	if _, ok := env["classifier_version"]; !ok {
		t.Fatal("M1 envelope has no classifier_version")
	}
	if _, ok := env["content_excerpt"]; ok {
		t.Fatal("M1 envelope carries an excerpt")
	}
	// The classifier request must not carry identity, and must carry the wire budget.
	if len(classifier.got) != 1 {
		t.Fatalf("classifier calls = %d", len(classifier.got))
	}
	req := classifier.got[0]
	if req.BudgetMS <= 0 {
		t.Fatalf("BudgetMS = %d, want a positive millisecond budget on the wire", req.BudgetMS)
	}
	for _, forbidden := range protocol.IdentityFieldNames {
		if strings.Contains(string(mustJSON(t, req)), forbidden) {
			t.Errorf("classifier request carries %q", forbidden)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestPipelineM2RequiresAnExcerptAndCapsIt(t *testing.T) {
	b := testBundle("b3", protocol.ModeM3, map[string]protocol.CollectionMode{"tool": protocol.ModeM2})
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, b)
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "customer_pii", Score: 0.5}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceMedium,
		Excerpt:           &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: "4111 1111 1111 1111"},
	}}
	_, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       10,
		Content:         &countingReader{body: []byte("hello")},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return "hello", nil, nil
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	var env struct {
		Excerpt *protocol.Excerpt `json:"content_excerpt"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Excerpt == nil {
		t.Fatal("M2 envelope has no excerpt")
	}
	if env.Excerpt.Text != "4111 1111 1111 1111" {
		t.Fatalf("excerpt = %q, want the classifier's minimised span", env.Excerpt.Text)
	}

	// The cap is structural: an over-cap excerpt is truncated at the boundary, so a defective
	// classifier cannot put a whole payload on the wire at M2.
	over := &protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "customer_pii", Score: 0.5}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceMedium,
		Excerpt:           &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: strings.Repeat("x", protocol.MaxExcerptChars+100)},
	}
	got := excerptForM2(*over)
	if got == nil || len([]rune(got.Text)) != protocol.MaxExcerptChars {
		t.Fatalf("over-cap excerpt was not truncated to %d characters", protocol.MaxExcerptChars)
	}
}

func TestPipelineM3HoldsContentLocallyAndCarriesNoExcerpt(t *testing.T) {
	b := testBundle("b4", protocol.ModeM3, map[string]protocol.CollectionMode{"tool": protocol.ModeM3})
	sink := &recordingSink{}
	store := &stubContentStore{}
	p := newTestPipeline(t, sink, b)
	p.Content = store
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "source_code", Score: 0.7}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
	}}

	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       11,
		Content:         &countingReader{body: []byte("hello world")},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(store.putIDs) != 1 || store.putIDs[0] != out.EventID {
		t.Fatalf("content store ids = %v, want the event id", store.putIDs)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if _, ok := env["content_excerpt"]; ok {
		t.Fatal("M3 envelope carries an excerpt; the schema forbids it")
	}
	if string(env["collection_mode"]) != `"m3"` {
		t.Fatalf("collection_mode = %s", env["collection_mode"])
	}
	// ADR 0017: the content-state marker is device-local and never in the envelope.
	for _, forbidden := range []string{"content_local", "content_state", "content_held", "local_content"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("M3 envelope carries a device-local marker field %q", forbidden)
		}
	}
	if obj, bytesHeld := p.ContentState(); obj != 1 || bytesHeld != 11 {
		t.Fatalf("content state = (%d, %d), want (1, 11) for the coverage row", obj, bytesHeld)
	}
}

func TestPipelineClassifierUnavailableStillEmitsDegraded(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Classifier = &stubClassifier{err: errors.New("host unreachable")}

	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       5,
		Content:         &countingReader{body: []byte("hello")},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
	})
	if err != nil {
		t.Fatalf("Process returned an error; a classifier outage must not fail the submission: %v", err)
	}
	if !out.Emitted || !out.Degraded {
		t.Fatalf("outcome = %+v, want emitted and degraded", out)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if string(env["confidence"]) != `"degraded"` {
		t.Fatalf("confidence = %s, want \"degraded\"", env["confidence"])
	}
	if _, ok := env["labels"]; !ok {
		t.Fatal("M1+ envelope must carry labels, even as an empty array")
	}
	if string(env["classifier_version"]) == `""` {
		t.Fatal("classifier_version is empty; the fallback must be named")
	}
}

// §5.4's spool trigger: the request is carried, and the loss is counted at the provider rather
// than being invisible.
func TestPipelineSpoolFailureCountsDroppedAndCarriesTheRequest(t *testing.T) {
	sink := &recordingSink{err: errors.New("spool full")}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "source_code", Score: 0.4}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceLow,
	}}
	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       5,
		Content:         &countingReader{body: []byte("hello")},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
	})
	if err == nil {
		t.Fatal("expected the spool failure to be reported to the caller")
	}
	if out.Emitted {
		t.Fatal("outcome claims the observation was emitted when the spool refused it")
	}
	c := p.Counters(protocol.RouteProxyTLS).Cumulative()
	if c[protocol.CounterDropped] != 1 {
		t.Fatalf("dropped = %d, want 1", c[protocol.CounterDropped])
	}
}

// §5.3: an over-cap body is not held in memory, is sized, is not classified, and is reported
// as degraded — never as a silent "clean".
func TestPipelineOverCapDegradesAndDoesNotClaimACanonicalDigest(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	classifier := &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "source_code", Score: 0.2}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceLow,
	}}
	p.Classifier = classifier

	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       9 << 20,
		OverCap:         true,
		Content:         &countingReader{body: []byte("prefix-only")},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !out.Degraded || out.Reason != ReasonOverCap {
		t.Fatalf("outcome = %+v, want degraded with %s", out, ReasonOverCap)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	// §5.3 records "a digest of the first N bytes" — deliberately not the canonical content
	// digest, and the key ladder must not claim Tier T for it.
	digest := unquoted(env["content_digest"])
	if digest != SHA256Hex([]byte("prefix-only")) {
		t.Fatalf("content_digest = %q, want the prefix digest %q", digest, SHA256Hex([]byte("prefix-only")))
	}
	wantKey, err := dedup.SurrogateKey("tenant-1", "device-1", "tool", string(protocol.KindPrompt), time.Unix(1_700_000_000, 0), 9<<20, nil, p.Normalizer)
	if err != nil {
		t.Fatalf("SurrogateKey: %v", err)
	}
	if sink.last().DedupKey != wantKey {
		t.Fatalf("dedup key = %q, want the Tier S surrogate %q (an over-cap body must not claim Tier T)", sink.last().DedupKey, wantKey)
	}
	if string(env["confidence"]) != `"degraded"` {
		t.Fatalf("confidence = %s, want degraded", env["confidence"])
	}
}

// C3 (NFC) is normative and feeds dedup_key, so a device that cannot perform it must say so
// rather than compute an unnormalised digest as if it were `sac-canon-1`: the observable
// consequence of getting this wrong is two rows for one submission, which is silent.
func TestPipelineWithoutCanonicaliserIsDegradedAndNeverClaimsTierT(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Normalizer = nil // no C3 implementation installed
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "source_code", Score: 0.3}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceLow,
	}}
	body := []byte("hello")
	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       int64(len(body)),
		Content:         &countingReader{body: body},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return "hello", nil, nil
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !out.Degraded || out.Reason != ReasonNoCanonicaliser {
		t.Fatalf("outcome = %+v, want degraded with %s", out, ReasonNoCanonicaliser)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if string(env["confidence"]) != `"degraded"` {
		t.Fatalf("confidence = %s, want degraded: an unnormalised digest must not be presented as canonical", env["confidence"])
	}
	wantKey, err := dedup.SurrogateKey("tenant-1", "device-1", "tool", string(protocol.KindPrompt), time.Unix(1_700_000_000, 0), int64(len(body)), nil, nil)
	if err != nil {
		t.Fatalf("SurrogateKey: %v", err)
	}
	if sink.last().DedupKey != wantKey {
		t.Fatalf("dedup key = %q, want the Tier S surrogate %q", sink.last().DedupKey, wantKey)
	}

	// The negative half of the ruling: the device *has* a digest, and must still not claim the
	// exact key built from it. If these two ever coincide the premise of §4.5's Tier T has been
	// asserted for bytes that were never canonically normalised — a silent over-merge whose two
	// rows the server and the device would count differently (R9).
	digest := unquoted(env["content_digest"])
	if digest == "" {
		t.Fatal("no content_digest was emitted; the schema requires one at M1 and above")
	}
	exactKey, err := dedup.ContentKey("tenant-1", "device-1", "tool", string(protocol.KindPrompt), time.Unix(1_700_000_000, 0), digest)
	if err != nil {
		t.Fatalf("ContentKey: %v", err)
	}
	if sink.last().DedupKey == exactKey {
		t.Fatal("a non-canonical digest produced the exact Tier T key; the key must claim only what the device can prove")
	}
}

func TestPipelineExtractionFailureDegradesToTheSurrogateTier(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
	}}
	out, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       5,
		Content:         &countingReader{body: []byte("hello")},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return "", nil, errors.New("no user-authored segment identifiable")
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !out.Degraded || out.Reason != ReasonExtractionDegraded {
		t.Fatalf("outcome = %+v, want degraded with %s", out, ReasonExtractionDegraded)
	}
	// A route that could not identify the authored segment has no canonical text either, so the
	// key must be the weak one for the same reason the missing-normaliser case is.
	wantKey, err := dedup.SurrogateKey("tenant-1", "device-1", "tool", string(protocol.KindPrompt), time.Unix(1_700_000_000, 0), 5, nil, dedup.IdentityNFC{})
	if err != nil {
		t.Fatalf("SurrogateKey: %v", err)
	}
	if sink.last().DedupKey != wantKey {
		t.Fatalf("dedup key = %q, want the Tier S surrogate %q", sink.last().DedupKey, wantKey)
	}
}

// TestPipelineSetIdentityAdoptsIssuedIdentity proves the seam the drain uses: before SetIdentity
// the pipeline stamps the construction-time flags (the fallback when there is no drain), and after
// SetIdentity it stamps the server-minted identity.
func TestPipelineSetIdentityAdoptsIssuedIdentity(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m0Bundle())

	processM0 := func() (tenantID, deviceID string) {
		t.Helper()
		size := int64(10)
		out, err := p.Process(context.Background(), Observation{
			Route:           protocol.RouteProxyTLS,
			Kind:            protocol.KindPrompt,
			ToolFingerprint: "tool",
			OccurredAt:      time.Unix(1_700_000_000, 0),
			SizeBytes:       size,
			Decision:        &protocol.Decision{RuleID: "r", Action: protocol.ActionLogged},
		})
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		if !out.Emitted {
			t.Fatalf("not emitted: %+v", out)
		}
		var env struct {
			TenantID string `json:"tenant_id"`
			DeviceID string `json:"device_id"`
		}
		if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		return env.TenantID, env.DeviceID
	}

	// No drain (no SetIdentity): the flags are the identity.
	if tenantID, deviceID := processM0(); tenantID != "tenant-1" || deviceID != "device-1" {
		t.Fatalf("before SetIdentity, envelope identity = (%q, %q), want flags (tenant-1, device-1)", tenantID, deviceID)
	}

	// Enrolment completes: the issued identity is adopted.
	p.SetIdentity(Identity{TenantID: "issued-tenant", DeviceID: "issued-device", UserRef: "user-1"})
	if tenantID, deviceID := processM0(); tenantID != "issued-tenant" || deviceID != "issued-device" {
		t.Fatalf("after SetIdentity, envelope identity = (%q, %q), want issued (issued-tenant, issued-device)", tenantID, deviceID)
	}
}

// TestPipelineUnresolvedRefusesToMint proves the fail-closed half of §3.5: a pipeline with no
// resolved identity refuses to mint rather than stamping a placeholder, and resumes once identity is
// resolved.
func TestPipelineUnresolvedRefusesToMint(t *testing.T) {
	sink := &recordingSink{}
	p, err := NewPipeline(sink, time.Now, func() string { return "evt-1" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	p.Normalizer = dedup.IdentityNFC{}
	p.Bundles = func() *policy.Bundle { return m0Bundle() }
	// A drain-configured device demands an issued identity.
	p.RequireIdentity(true)

	obs := Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       10,
		Decision:        &protocol.Decision{RuleID: "r", Action: protocol.ActionLogged},
		Content:         &tripwireReader{t: t}, // the gate must refuse BEFORE the content is read
	}

	out, err := p.Process(context.Background(), obs)
	if !errors.Is(err, ErrIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrIdentityUnresolved", err)
	}
	if out.Emitted {
		t.Fatal("an unresolved pipeline emitted an envelope")
	}
	if out.Reason != ReasonIdentityUnresolved {
		t.Fatalf("reason = %q, want %q", out.Reason, ReasonIdentityUnresolved)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("the sink holds %d entries, want 0", len(sink.entries))
	}

	// Resolving the identity resumes minting.
	p.SetIdentity(Identity{TenantID: "tenant-1", DeviceID: "device-1", UserRef: "user-1"})
	out, err = p.Process(context.Background(), obs)
	if err != nil {
		t.Fatalf("Process after resolve: %v", err)
	}
	if !out.Emitted {
		t.Fatalf("not emitted after identity resolved: %+v", out)
	}
}

func TestPipelineUsageRollupCarriesNoContentFields(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	start := time.Unix(1_699_999_800, 0)
	end := start.Add(24 * time.Hour)
	count := 3
	bytesTotal := int64(900)
	key, err := dedup.RollupKey("tenant-1", "device-1", "tool", string(protocol.KindUsageRollup), start, end)
	if err != nil {
		t.Fatalf("RollupKey: %v", err)
	}
	_, err = p.EmitEnvelope(context.Background(), EnvelopeInput{
		Kind:            protocol.KindUsageRollup,
		Route:           protocol.RouteProcDetect,
		Mode:            protocol.ModeM1,
		ToolFingerprint: "tool",
		OccurredAt:      end,
		DedupKey:        key,
		WindowStart:     &start,
		WindowEnd:       &end,
		SubmissionCount: &count,
		BytesTotal:      &bytesTotal,
	})
	if err != nil {
		t.Fatalf("EmitEnvelope: %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	for _, forbidden := range []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments", "size_bytes", "policy_decision"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("usage_rollup envelope carries %q, which the schema forbids for this kind", forbidden)
		}
	}
	if string(env["direction"]) != `"none"` {
		t.Fatalf("direction = %s, want \"none\" for a rollup", env["direction"])
	}
}

// A partially-populated identity (empty tenant/device, non-empty user_ref) must not bypass the
// fail-closed gate on a drain run: BuildEnvelope's field policy checks presence, not emptiness.
func TestPipelineEmitEnvelopePartialIdentityDoesNotBypassTheGate(t *testing.T) {
	sink := &recordingSink{}
	p, err := NewPipeline(sink, time.Now, func() string { return "evt-1" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	p.RequireIdentity(true)

	_, err = p.EmitEnvelope(context.Background(), EnvelopeInput{
		Kind:            protocol.KindUsageRollup,
		Route:           protocol.RouteProcDetect,
		Mode:            protocol.ModeM1,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		Identity:        Identity{UserRef: "user-x"}, // empty tenant/device
	})
	if !errors.Is(err, ErrIdentityUnresolved) {
		t.Fatalf("EmitEnvelope err = %v, want ErrIdentityUnresolved (a partial identity bypassed the gate)", err)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("the sink holds %d entries, want 0", len(sink.entries))
	}
}
