package classify_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/classifier-host/parser/isolation"
	"github.com/shadow-ai-capture/device/protocol"
)

const cardBody = "Please charge card 4111 1111 1111 1111 for the invoice."

// fakeParser stands in for the parser child: these tests are about what the pipeline does with
// each parse outcome, and parser/isolation's tests run real children.
type fakeParser struct{ result isolation.Result }

func (f fakeParser) Parse(context.Context, string, []byte) isolation.Result { return f.result }

func newHost(t *testing.T, adjust ...func(*classify.Options)) *classify.Host {
	t.Helper()
	// A generous budget keeps these tests independent of machine speed; the budget tests set
	// their own.
	opts := classify.Options{Release: testrig.Release(t, "test-1"),
		Budget: classify.Budget{Rules: time.Minute, Validators: time.Minute, Model: time.Minute, Total: time.Minute}}
	for _, f := range adjust {
		f(&opts)
	}
	h, err := classify.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// tickingClock advances by a millisecond on every reading, so a nanosecond stage budget is
// exhausted on the first check whatever the platform clock's resolution.
func tickingClock() func() time.Time {
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Millisecond)
		return now
	}
}

func budget(rules, validators, model, total time.Duration) func(*classify.Options) {
	return func(o *classify.Options) {
		o.Budget = classify.Budget{Rules: rules, Validators: validators, Model: model, Total: total}
		o.Now = tickingClock()
	}
}

func withParser(r isolation.Result) func(*classify.Options) {
	return func(o *classify.Options) { o.Parser = fakeParser{r} }
}

func withLimits(l norm.Limits) func(*classify.Options) {
	return func(o *classify.Options) { o.Limits = l }
}

func req(mode protocol.CollectionMode, mediaType, body string) protocol.ClassifyRequest {
	return protocol.ClassifyRequest{Content: []byte(body), Mode: mode, MediaType: mediaType, BudgetMS: 5000}
}

func classes(resp protocol.ClassifyResponse) string {
	var out []string
	for _, l := range resp.Labels {
		out = append(out, l.Class)
	}
	return strings.Join(out, ",")
}

const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

func TestDegradedResponses(t *testing.T) {
	second := time.Second
	cases := []struct {
		name   string
		adjust []func(*classify.Options)
		req    protocol.ClassifyRequest
		stage  string
		detail protocol.Detail
	}{
		{"total budget exhausted", []func(*classify.Options){budget(second, second, second, time.Millisecond)},
			req(protocol.ModeM1, "text/plain", cardBody), classify.StageRules, protocol.DetailBudgetExhausted},
		{"rules budget exhausted", []func(*classify.Options){budget(time.Nanosecond, second, second, second)},
			req(protocol.ModeM1, "text/plain", cardBody), classify.StageRules, protocol.DetailBudgetExhausted},
		{"validators budget exhausted", []func(*classify.Options){budget(second, time.Nanosecond, second, second)},
			req(protocol.ModeM1, "text/plain", cardBody), classify.StageValidators, protocol.DetailBudgetExhausted},
		{"model budget exhausted", []func(*classify.Options){budget(second, second, time.Nanosecond, second)},
			req(protocol.ModeM1, "text/plain", "the physician reviewed the dosage"), classify.StageModel, protocol.DetailBudgetExhausted},
		{"normalisation dropped matchable text", []func(*classify.Options){withLimits(norm.Limits{MaxInputBytes: 1 << 16, MaxTextBytes: 64, MaxTokens: 100, MaxDepth: 8})},
			req(protocol.ModeM1, "text/plain", strings.Repeat("filler ", 40)+cardBody), classify.StageNormalise, protocol.DetailNormaliseTruncated},
		{"body over the input limit", []func(*classify.Options){withLimits(norm.Limits{MaxInputBytes: 16, MaxTextBytes: 16, MaxTokens: 8, MaxDepth: 4})},
			req(protocol.ModeM1, "text/plain", strings.Repeat("x", 64)), classify.StageNormalise, protocol.DetailContentOverCap},
		{"undecodable body", nil,
			req(protocol.ModeM1, "text/plain", "a\xffb"), classify.StageNormalise, protocol.DetailUndecodableContent},
		{"no content at a mode that reads content", nil,
			req(protocol.ModeM1, "text/plain", ""), classify.StageNormalise, protocol.DetailContentUnprocessable},
		{"unsupported document", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseUnsupported})},
			req(protocol.ModeM1, "application/pdf", "%PDF-1.7"), classify.StageParse, protocol.DetailParserFailed},
		{"parser timed out", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseTimeout})},
			req(protocol.ModeM1, docx, "PK"), classify.StageParse, protocol.DetailParserTimeout},
		{"parser killed at its memory limit", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseMemory})},
			req(protocol.ModeM1, docx, "PK"), classify.StageParse, protocol.DetailParserMemory},
		{"parser crashed", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseCrash})},
			req(protocol.ModeM1, docx, "PK"), classify.StageParse, protocol.DetailParserCrash},
		{"parser output limit", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseOutputCap})},
			req(protocol.ModeM1, "application/zip", "PK"), classify.StageParse, protocol.DetailParserOutputCap},
		{"document over the parser's size limit", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseInputCap})},
			req(protocol.ModeM1, "application/zip", "PK"), classify.StageParse, protocol.DetailContentOverCap},
		{"format suspended", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseSuspended})},
			req(protocol.ModeM1, "application/zip", "PK"), classify.StageParse, protocol.DetailParserFailed},
		{"no parser configured", nil,
			req(protocol.ModeM1, "application/zip", "PK"), classify.StageParse, protocol.DetailParserFailed},
		{"request names another release", nil,
			protocol.ClassifyRequest{Content: []byte(cardBody), Mode: protocol.ModeM1, ReleaseID: "other"}, classify.StageRelease, protocol.DetailReleaseLoadFailed},
		{"content at M0", nil,
			req(protocol.ModeM0, "text/plain", cardBody), classify.StageMode, protocol.DetailModeViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := newHost(t, tc.adjust...).Classify(context.Background(), tc.req)
			if err := resp.Validate(); err != nil {
				t.Fatalf("the response violates the contract: %v (%+v)", err, resp)
			}
			if resp.Confidence != protocol.ConfidenceDegraded || len(resp.Labels) != 0 {
				t.Fatalf("confidence %q with %d labels, want degraded with none", resp.Confidence, len(resp.Labels))
			}
			last := resp.Stages[len(resp.Stages)-1]
			if last.Stage != tc.stage || last.Detail != tc.detail || !last.Failed && !last.Truncated || !last.Detail.Valid() {
				t.Errorf("last stage %+v, want %s failed with %s", last, tc.stage, tc.detail)
			}
		})
	}
}

func TestCompletedResponses(t *testing.T) {
	cases := []struct {
		name    string
		adjust  []func(*classify.Options)
		req     protocol.ClassifyRequest
		classes string
		conf    protocol.Confidence
	}{
		{"nothing found", nil, req(protocol.ModeM1, "text/plain", "The weather in Lisbon is mild in October."), "", protocol.ConfidenceHigh},
		{"checksum-confirmed card", nil, req(protocol.ModeM1, "text/plain", cardBody), "payment_card", protocol.ConfidenceHigh},
		{"model only, low score", nil, req(protocol.ModeM1, "text/plain", "the physician reviewed the dosage and the symptoms"), "health", protocol.ConfidenceLow},
		{"model only, high score", nil, req(protocol.ModeM1, "text/plain", "physician dosage symptoms treatment therapy clinical"), "health", protocol.ConfidenceMedium},
		{"large body fully processed", nil, req(protocol.ModeM1, "text/plain", strings.Repeat("filler ", 2000)+cardBody), "payment_card", protocol.ConfidenceHigh},
		{"card in a JSON body", nil, req(protocol.ModeM1, "application/json", `{"messages":[{"content":"`+cardBody+`"}]}`), "payment_card", protocol.ConfidenceHigh},
		{"document parsed, nothing found", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseOK, Text: "nothing to see"})},
			req(protocol.ModeM1, "application/zip", "PK"), "", protocol.ConfidenceHigh},
		{"document parsed, card found", []func(*classify.Options){withParser(isolation.Result{Cause: isolation.CauseOK, Text: cardBody})},
			req(protocol.ModeM1, docx, "PK"), "payment_card", protocol.ConfidenceHigh},
		{"UTF-16 body", nil, protocol.ClassifyRequest{Mode: protocol.ModeM1, MediaType: "text/plain",
			Content: []byte{0xFF, 0xFE, '4', 0, '1', 0, '1', 0, '1', 0, ' ', 0, '1', 0, '1', 0, '1', 0, '1', 0, ' ', 0, '1', 0, '1', 0, '1', 0, '1', 0, ' ', 0, '1', 0, '1', 0, '1', 0, '1', 0}},
			"payment_card", protocol.ConfidenceHigh},
		{"rules and model on different classes", nil, req(protocol.ModeM1, "text/plain", "card 4111 1111 1111 1111, the physician reviewed the dosage and the symptoms"),
			"payment_card,health", protocol.ConfidenceHigh},
		{"one label per class, highest score kept", nil, req(protocol.ModeM1, "text/plain", "ssn 123-45-6788 and nino AB123456C"), "government_id", protocol.ConfidenceHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := newHost(t, tc.adjust...).Classify(context.Background(), tc.req)
			if err := resp.Validate(); err != nil {
				t.Fatalf("the response violates the contract: %v", err)
			}
			for _, st := range resp.Stages {
				if st.Failed || st.Truncated || st.Detail != protocol.DetailNone {
					t.Errorf("a completed classification has stage %+v", st)
				}
			}
			if got := classes(resp); got != tc.classes || resp.Confidence != tc.conf {
				t.Errorf("classes %q confidence %q, want %q %q (%+v)", got, resp.Confidence, tc.classes, tc.conf, resp.Labels)
			}
		})
	}
}

func TestRuleLabelsCarryTheRuleAndTheModelSkipsResolvedClasses(t *testing.T) {
	resp := newHost(t).Classify(context.Background(), req(protocol.ModeM1, "text/plain",
		"diagnosis and prescription reviewed by the physician; the dosage and symptoms were noted"))
	if len(resp.Labels) != 1 {
		t.Fatalf("labels %+v", resp.Labels)
	}
	if l := resp.Labels[0]; l.Class != "health" || l.RuleID != "HEALTH_CLINICAL_VOCABULARY" || l.Score != 0.7 {
		t.Errorf("label %+v: the rule's label must stand rather than a model score for the same class", l)
	}
	resp = newHost(t).Classify(context.Background(), req(protocol.ModeM1, "text/plain", "ssn 123-45-6788 and nino AB123456C"))
	if l := resp.Labels[0]; l.RuleID != "US_SSN_STRUCTURE" || l.Score != 0.9 {
		t.Errorf("label %+v, want the higher-scoring rule", l)
	}
}

func TestExcerptIsTheFirstConfirmedMatchAtM2Only(t *testing.T) {
	h := newHost(t)
	body := "reference 4111 1111 1111 1112, then card 4111 1111 1111 1111"
	m2 := h.Classify(context.Background(), req(protocol.ModeM2, "text/plain", body))
	if m2.Excerpt == nil || m2.Excerpt.Text != "4111 1111 1111 1111" || m2.Excerpt.Kind != protocol.ExcerptMatchSpan || m2.Excerpt.MatchType != "payment_card" {
		t.Fatalf("M2 excerpt %+v, want the confirmed card and not the number that failed its check digit", m2.Excerpt)
	}
	if got := body[m2.Excerpt.OffsetStart:m2.Excerpt.OffsetEnd]; got != m2.Excerpt.Text {
		t.Errorf("the offsets select %q", got)
	}
	short := newHost(t, func(o *classify.Options) { o.MaxExcerptChars = 8 }).Classify(context.Background(), req(protocol.ModeM2, "text/plain", cardBody))
	if short.Excerpt == nil || len([]rune(short.Excerpt.Text)) != 8 {
		t.Errorf("the excerpt was not cut to 8 characters: %+v", short.Excerpt)
	}
	for _, mode := range []protocol.CollectionMode{protocol.ModeM1, protocol.ModeM3} {
		if resp := h.Classify(context.Background(), req(mode, "text/plain", body)); resp.Excerpt != nil {
			t.Errorf("%s carries an excerpt", mode)
		}
	}
	if resp := h.Classify(context.Background(), req(protocol.ModeM2, "text/plain", "the physician reviewed the dosage and the symptoms")); resp.Excerpt != nil {
		t.Errorf("a model-only label produced an excerpt: %+v", resp.Excerpt)
	}
}

func TestHandshake(t *testing.T) {
	h := newHost(t)
	if ok := h.Handshake(protocol.HandshakeRequest{CoreVersion: "core", ProtocolVersion: protocol.Version}); !ok.OK || ok.ClassifierVersion != "test-1" {
		t.Errorf("a matching handshake: %+v", ok)
	}
	for name, hs := range map[string]protocol.HandshakeRequest{
		"protocol version": {CoreVersion: "core", ProtocolVersion: protocol.Version + 1},
		"no core version":  {ProtocolVersion: protocol.Version},
		"another release":  {CoreVersion: "core", ProtocolVersion: protocol.Version, ClassifierVersion: "test-0"},
	} {
		if resp := h.Handshake(hs); resp.OK || resp.Reason != protocol.DetailVersionMismatch {
			t.Errorf("%s: %+v", name, resp)
		}
	}
}

func TestEveryResponseSatisfiesTheContract(t *testing.T) {
	h := newHost(t)
	for i, r := range []protocol.ClassifyRequest{
		req(protocol.ModeM1, "text/plain", cardBody),
		req(protocol.ModeM2, "text/plain", cardBody),
		req(protocol.ModeM3, "text/plain", cardBody),
		req(protocol.ModeM0, "text/plain", ""),
		req(protocol.ModeM0, "text/plain", cardBody),
		req(protocol.CollectionMode("nonsense"), "text/plain", cardBody),
		req(protocol.ModeM1, "application/json", `{}`),
		req(protocol.ModeM1, "text/plain", strings.Repeat("x", 3<<20)),
		{Mode: protocol.ModeM1, BudgetMS: 1},
	} {
		resp := h.Classify(context.Background(), r)
		if err := resp.Validate(); err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
}

// TestTheClassifierCannotSeeIdentity checks that the package consumes protocol.ClassifyRequest,
// which has no identity field, rather than a request type of its own, and that no identity field
// appears in what it returns.
func TestTheClassifierCannotSeeIdentity(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			src, err := os.ReadFile(filepath.Join(".", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(src), "Request struct") {
				t.Errorf("%s declares a request type of its own", e.Name())
			}
		}
	}
	blob, err := json.Marshal(newHost(t).Classify(context.Background(), req(protocol.ModeM2, "text/plain", cardBody)))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range protocol.IdentityFieldNames {
		if strings.Contains(string(blob), `"`+name+`"`) {
			t.Errorf("the response carries identity field %q", name)
		}
	}
}

func TestClassifyIsSafeForConcurrentUse(t *testing.T) {
	h := newHost(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if resp := h.Classify(context.Background(), req(protocol.ModeM2, "text/plain", cardBody)); classes(resp) != "payment_card" {
					t.Errorf("a concurrent classification returned %+v", resp)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewRequiresARelease(t *testing.T) {
	if _, err := classify.New(classify.Options{}); err == nil {
		t.Fatal("a host was built without a release")
	}
}
