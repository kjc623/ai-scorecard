package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/protocol"
)

// The decision is a shape judgement over the payload and C1's output, so the table pins both the
// client patterns and the default: an unmatched prompt is a person's, never the client's.
func TestDecidePromptKind(t *testing.T) {
	titling := "<session>\nWhat is the capital of Australia\n</session>\n\n" +
		"Write the title in the predominant language of the session — a stray word or code token " +
		"in another language doesn't change it, and neither does the English of these instructions."
	input := []byte(`{"messages":[{"role":"user","content":"What is the capital of Australia"}]}`)
	cases := []struct {
		name      string
		payload   []byte
		extracted string
		want      protocol.PromptKind
	}{
		{"a typed question is the person's", input, "What is the capital of Australia", protocol.PromptKindUser},
		{"typed source code is still the person's", nil, "def add(a, b):\n    return a + b", protocol.PromptKindUser},
		{"a card number the person typed is the person's", nil, "my card is 4111 1111 1111 1111", protocol.PromptKindUser},
		{"a Claude Code titling request is the client's", input, titling, protocol.PromptKindClientGenerated},
		{"the next-action summary is the client's", nil, "Describe your most recent action in 3-5 words using present tense (-ing).", protocol.PromptKindClientGenerated},
		{"the cut-off retry is the client's", nil, "Your response above was cut off mid-stream and only your next message is delivered.", protocol.PromptKindClientGenerated},
		{"suggestion mode is the client's", nil, "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]", protocol.PromptKindClientGenerated},
		{"a telemetry body with no authored turn is the client's", []byte(`{"events":[{"name":"x"}]}`), "", protocol.PromptKindClientGenerated},
		{"a chat body with no user text is the client's", []byte(`{"messages":[{"role":"assistant","content":"hi"}]}`), "", protocol.PromptKindClientGenerated},
		{"an unreadable body is unknown, not the client's", []byte("not json"), "", protocol.PromptKindUnknown},
		{"no payload and no text is unknown", nil, "", protocol.PromptKindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decidePromptKind(tc.payload, tc.extracted); got != tc.want {
				t.Fatalf("decidePromptKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// The titling request is the reported defect: it is stored and indexed as if a person typed it,
// and its body classified. The device now marks it client_generated and classifies nothing, so no
// source_code label can be invented from the client's own instructions.
func TestPipelineMarksATitlingRequestClientGeneratedAndDoesNotClassify(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	classifier := &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "source_code", Score: 1.0}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
	}}
	p.Classifier = classifier
	titling := "<session>\nWhat is the capital of Australia\n</session>\n\n" +
		"Write the title in the predominant language of the session — a stray word or code token " +
		"in another language doesn't change it."
	body := []byte(`{"system":"You are Claude Code","messages":[{"role":"user","content":` + strconvQuote(titling) + `}],"tools":[{"name":"Read"}]}`)

	_, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       int64(len(body)),
		MediaType:       "application/json",
		Content:         &countingReader{body: body},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return titling, nil, nil
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(classifier.got) != 0 {
		t.Fatalf("the classifier ran %d times on a client-generated request; nothing a person typed exists to classify", len(classifier.got))
	}
	var env struct {
		PromptKind protocol.PromptKind `json:"prompt_kind"`
		Labels     *[]protocol.Label   `json:"labels"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.PromptKind != protocol.PromptKindClientGenerated {
		t.Fatalf("prompt_kind = %q, want client_generated", env.PromptKind)
	}
	if env.Labels == nil || len(*env.Labels) != 0 {
		t.Fatalf("labels = %v, want an empty set: a client request carries no user content to label", env.Labels)
	}
}

// The capital-of-Australia defect: the body carried a system prompt and tool definitions, and the
// classifier ran over all of it, so a plain question came back labelled source_code. The device
// now hands the classifier only the text C1 identified as authored.
func TestPipelineClassifiesTheAuthoredTextNotTheWholeBody(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	classifier := &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
	}}
	p.Classifier = classifier
	question := "What is the capital of Australia"
	// The body is a realistic Claude Code request: a system prompt and tool schemas that mention
	// code, which is where the spurious source_code label came from.
	body := []byte(`{"system":"You are Claude Code, a coding agent. Use the Read tool to read source code. def tool_schema(){}","messages":[{"role":"user","content":` + strconvQuote(question) + `}],"tools":[{"name":"Read","description":"read source code files"}]}`)

	_, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       int64(len(body)),
		MediaType:       "application/json",
		Content:         &countingReader{body: body},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return question, nil, nil
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(classifier.got) != 1 {
		t.Fatalf("classifier calls = %d, want 1", len(classifier.got))
	}
	if got := string(classifier.got[0].Content); got != question {
		t.Fatalf("classifier saw %q, want only the authored text %q", got, question)
	}
	var env struct {
		PromptKind protocol.PromptKind `json:"prompt_kind"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.PromptKind != protocol.PromptKindUser {
		t.Fatalf("prompt_kind = %q, want user", env.PromptKind)
	}
}

// A prompt that really contains source code is still classified, and the label is real because
// the classifier now sees the code rather than a mix of code and question.
func TestPipelineStillLabelsAuthoredSourceCode(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Classifier = &stubClassifier{resp: protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "source_code", Score: 0.9, RuleID: "R_SOURCE"}},
		ClassifierVersion: "rel-1",
		Confidence:        protocol.ConfidenceHigh,
	}}
	code := "def add(a, b):\n    return a + b"
	_, err := p.Process(context.Background(), Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       int64(len(code)),
		Content:         &countingReader{body: []byte(code)},
		Decision:        &protocol.Decision{RuleID: "R", Action: protocol.ActionLogged},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return code, nil, nil
		}),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	var env struct {
		PromptKind protocol.PromptKind `json:"prompt_kind"`
		Labels     []protocol.Label    `json:"labels"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.PromptKind != protocol.PromptKindUser {
		t.Fatalf("prompt_kind = %q, want user", env.PromptKind)
	}
	found := false
	for _, l := range env.Labels {
		if l.Class == "source_code" {
			found = true
		}
	}
	if !found {
		t.Fatalf("labels = %v, want a source_code label for authored code", env.Labels)
	}
}

// strconvQuote is a tiny JSON string quoter for the test bodies; the standard library's is not
// imported to keep the test's imports minimal.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
