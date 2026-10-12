package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// classifierFunc adapts a function to the Classifier interface.
type classifierFunc func(protocol.ClassifyRequest) (protocol.ClassifyResponse, error)

func (f classifierFunc) Classify(_ context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	return f(req)
}

var invoice = []byte("Invoice 7. Please charge card 4111 1111 1111 1111 for the balance.")

// attachmentObservation is an M1 prompt that names one attachment and carries its bytes.
func attachmentObservation(reader ContentReader) Observation {
	desc := dedup.Attachment{Name: "invoice.txt", MediaType: "text/plain", SizeBytes: int64(len(invoice)), ContentDigest: SHA256Hex(invoice)}
	return Observation{
		Route:             protocol.RouteExtWebRequest,
		Kind:              protocol.KindPrompt,
		ToolFingerprint:   "tool",
		OccurredAt:        time.Unix(1_700_000_000, 0),
		SizeBytes:         40,
		MediaType:         "application/json",
		Content:           &countingReader{body: []byte("summarise the attached invoice")},
		Enforce:           decided(protocol.Decision{RuleID: "R", Action: protocol.ActionLogged}),
		Attachments:       []dedup.Attachment{desc},
		AttachmentContent: []AttachmentContent{{MediaType: "text/plain", Content: reader}},
		Extract: ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return "summarise the attached invoice", []dedup.Attachment{desc}, nil
		}),
	}
}

func TestPipelineClassifiesAttachmentBytesAndMergesTheLabels(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	var reqs []protocol.ClassifyRequest
	p.Classifier = classifierFunc(func(req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
		reqs = append(reqs, req)
		if bytes.Equal(req.Content, invoice) {
			return protocol.ClassifyResponse{ClassifierVersion: "rel-1", Confidence: protocol.ConfidenceMedium, Labels: []protocol.Label{
				{Class: "payment_card", Score: 0.97, RuleID: "pan"},
				{Class: "legal_commercial", Score: 0.95, RuleID: "R_LEGAL"},
			}}, nil
		}
		return protocol.ClassifyResponse{ClassifierVersion: "rel-1", Confidence: protocol.ConfidenceHigh,
			Labels: []protocol.Label{{Class: "legal_commercial", Score: 0.6, RuleID: "R_LEGAL"}}}, nil
	})
	reader := &countingReader{body: invoice}

	out, err := p.Process(context.Background(), attachmentObservation(reader))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reader.reads != 1 || len(reqs) != 2 {
		t.Fatalf("attachment reads = %d, classifier calls = %d; want 1 and 2", reader.reads, len(reqs))
	}
	if att := reqs[1]; att.MediaType != "text/plain" || att.Mode != protocol.ModeM1 {
		t.Fatalf("attachment request = media type %q mode %q, want text/plain under m1", att.MediaType, att.Mode)
	}
	if out.Confidence != protocol.ConfidenceMedium {
		t.Fatalf("confidence = %q, want the least confident of prompt and attachment", out.Confidence)
	}

	payload := sink.last().Payload
	var env struct {
		Labels      []protocol.Label                `json:"labels"`
		Attachments []protocol.AttachmentDescriptor `json:"attachments"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	scores := map[string]float64{}
	for _, l := range env.Labels {
		scores[l.Class] = l.Score
	}
	if len(env.Labels) != 2 || scores["payment_card"] != 0.97 || scores["legal_commercial"] != 0.95 {
		t.Fatalf("labels = %+v, want payment_card and one legal_commercial at the higher score", env.Labels)
	}
	want := protocol.AttachmentDescriptor{Name: "invoice.txt", SizeBytes: int64(len(invoice)), ContentDigest: SHA256Hex(invoice)}
	if len(env.Attachments) != 1 || env.Attachments[0] != want {
		t.Fatalf("attachments = %+v, want %+v", env.Attachments, want)
	}
	if bytes.Contains(payload, []byte("4111")) {
		t.Fatal("attachment bytes reached the envelope")
	}
}

func TestPipelineM0NeverReadsAttachmentBytes(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m0Bundle())
	p.Classifier = classifierFunc(func(protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
		t.Error("the classifier was called at M0")
		return protocol.ClassifyResponse{}, nil
	})
	obs := attachmentObservation(&tripwireReader{t: t, body: invoice})
	obs.Content = nil
	if _, err := p.Process(context.Background(), obs); err != nil {
		t.Fatalf("Process: %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"labels", "attachments", "content_digest"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("M0 envelope carries %q", forbidden)
		}
	}
}

// An attachment the classifier could not classify degrades the record; it never reads as "nothing
// found".
func TestPipelineUnclassifiedAttachmentDegradesTheRecord(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	p.Classifier = classifierFunc(func(req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
		if bytes.Equal(req.Content, invoice) {
			return protocol.ClassifyResponse{}, errors.New("parser child exhausted its budget")
		}
		return protocol.ClassifyResponse{ClassifierVersion: "rel-1", Confidence: protocol.ConfidenceHigh,
			Labels: []protocol.Label{{Class: "legal_commercial", Score: 0.6, RuleID: "R_LEGAL"}}}, nil
	})
	out, err := p.Process(context.Background(), attachmentObservation(&countingReader{body: invoice}))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !out.Degraded || out.Reason != ReasonClassifierDegraded || out.Confidence != protocol.ConfidenceDegraded {
		t.Fatalf("outcome = %+v, want degraded by the classifier", out)
	}
	var env struct {
		Labels []protocol.Label `json:"labels"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Labels) != 1 || env.Labels[0].Class != "legal_commercial" {
		t.Fatalf("labels = %+v, want the prompt's labels kept", env.Labels)
	}
}

// A custom label reaches the classifier as terms and an attached file's name as a label, and both
// are what the enforcement hook and the envelope see.
func TestPipelineAppliesTheCustomLabels(t *testing.T) {
	sink := &recordingSink{}
	b := m1Bundle()
	b.CustomLabels = []policy.CustomLabel{
		{Label: "custom_invoices", FileNames: []string{"invoice*"}},
		{Label: "custom_project_falcon", Words: []string{"falcon"}, CustomerNames: []string{"Acme Corp"}},
		{Label: "custom_cad", FileExtensions: []string{"dwg"}},
	}
	p := newTestPipeline(t, sink, b)
	var reqs []protocol.ClassifyRequest
	p.Classifier = classifierFunc(func(req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
		reqs = append(reqs, req)
		return protocol.ClassifyResponse{ClassifierVersion: "rel-1", Confidence: protocol.ConfidenceHigh,
			Labels: []protocol.Label{{Class: "custom_project_falcon", Score: protocol.CustomLabelScore}}}, nil
	})
	var calls []enforceCall
	obs := attachmentObservation(&countingReader{body: invoice})
	obs.Enforce = recordingHook(&calls)
	if _, err := p.Process(context.Background(), obs); err != nil {
		t.Fatalf("Process: %v", err)
	}
	want := []protocol.CustomLabelTerms{{Label: "custom_project_falcon", Terms: []string{"falcon", "Acme Corp"}}}
	for i, r := range reqs {
		if !reflect.DeepEqual(r.CustomLabels, want) {
			t.Errorf("classify request %d carries custom labels %+v, want %+v", i, r.CustomLabels, want)
		}
	}
	if len(calls) != 1 || !calls[0].known || !reflect.DeepEqual(calls[0].labels, []string{"custom_project_falcon", "custom_invoices"}) {
		t.Fatalf("the hook saw %+v, want the classifier's custom label and the file's", calls)
	}
	var env struct {
		Labels []protocol.Label `json:"labels"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Labels) != 2 || env.Labels[1] != (protocol.Label{Class: "custom_invoices", Score: protocol.CustomLabelScore}) {
		t.Fatalf("labels = %+v, want the file's custom label after the classifier's", env.Labels)
	}
}

func TestMergeLabelsStopsAtTheEnvelopeCap(t *testing.T) {
	var many []protocol.Label
	for i := 0; i < maxLabels+5; i++ {
		many = append(many, protocol.Label{Class: "c", RuleID: string(rune('A' + i))})
	}
	if got := mergeLabels(nil, many); len(got) != maxLabels {
		t.Fatalf("merged %d labels, want the cap %d", len(got), maxLabels)
	}
}

// A file a hook found beside the prompt, or one the prompt's text names, is recorded, and the
// record's digest and key are the ones a route that saw only the text computes, so the two records
// of one prompt agree.
func TestPipelineRecordsReferencedFilesOutsideTheDigest(t *testing.T) {
	text := []byte("explain @docs/plan.md")
	observe := func(set func(*Observation)) map[string]json.RawMessage {
		sink := &recordingSink{}
		b := m1Bundle()
		b.CustomLabels = []policy.CustomLabel{{Label: "custom_plans", FileNames: []string{"plan*"}}}
		p := newTestPipeline(t, sink, b)
		obs := Observation{
			Route:           protocol.RouteToolHook,
			Kind:            protocol.KindPrompt,
			ToolFingerprint: "tool",
			OccurredAt:      time.Unix(1_700_000_000, 0),
			SizeBytes:       int64(len(text)),
			MediaType:       "text/plain",
			Content:         &countingReader{body: text},
			Enforce:         decided(protocol.Decision{RuleID: "R", Action: protocol.ActionLogged}),
			Extract: ExtractorFunc(func(b []byte, _ string) (string, []dedup.Attachment, error) {
				return string(b), nil, nil
			}),
		}
		set(&obs)
		if _, err := p.Process(context.Background(), obs); err != nil {
			t.Fatalf("Process: %v", err)
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
			t.Fatal(err)
		}
		return env
	}
	alone := observe(func(*Observation) {})
	for name, c := range map[string]struct {
		set  func(*Observation)
		want protocol.AttachmentDescriptor
	}{
		"found by the hook": {
			func(o *Observation) {
				o.Referenced = []dedup.Attachment{{Name: "plan.md", Path: "/home/u/docs/plan.md", SizeBytes: 12}}
			},
			protocol.AttachmentDescriptor{Name: "plan.md", Path: "/home/u/docs/plan.md", SizeBytes: 12},
		},
		"named by the text": {
			func(o *Observation) {
				o.References = func(got string) []dedup.Attachment {
					if got != string(text) {
						t.Errorf("References was given %q, want the extracted text", got)
					}
					return []dedup.Attachment{{Name: "plan.md"}}
				}
			},
			protocol.AttachmentDescriptor{Name: "plan.md"},
		},
	} {
		env := observe(c.set)
		for _, f := range []string{"content_digest", "dedup_key"} {
			if len(alone[f]) == 0 || string(alone[f]) != string(env[f]) {
				t.Errorf("%s: %s differs: %s without the file, %s with it", name, f, alone[f], env[f])
			}
		}
		var atts []protocol.AttachmentDescriptor
		if err := json.Unmarshal(env["attachments"], &atts); err != nil {
			t.Fatal(err)
		}
		if len(atts) != 1 || atts[0] != c.want {
			t.Fatalf("%s: attachments = %+v, want %+v", name, atts, c.want)
		}
		if !bytes.Contains(env["labels"], []byte("custom_plans")) {
			t.Fatalf("%s: labels = %s, want the file's custom label", name, env["labels"])
		}
	}
}

// At M0 the text is never read, so nothing is asked of it.
func TestPipelineAsksNothingOfTheTextAtM0(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m0Bundle())
	_, err := p.Process(context.Background(), Observation{
		Route: protocol.RouteToolOTel, Kind: protocol.KindPrompt, ToolFingerprint: "tool",
		OccurredAt: time.Unix(1_700_000_000, 0), SizeBytes: 3, MediaType: "text/plain",
		Content: &countingReader{body: []byte("@a.md")},
		Enforce: decided(protocol.Decision{RuleID: "R", Action: protocol.ActionLogged}),
		Extract: ExtractorFunc(func(b []byte, _ string) (string, []dedup.Attachment, error) { return string(b), nil, nil }),
		References: func(string) []dedup.Attachment {
			t.Fatal("References was called at M0")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A request whose text could not be read may still name its files, and the record keeps them.
func TestPipelineKeepsTheNamesOfABodyWithoutText(t *testing.T) {
	sink := &recordingSink{}
	p := newTestPipeline(t, sink, m1Bundle())
	obs := attachmentObservation(&countingReader{body: invoice})
	obs.Attachments, obs.AttachmentContent = nil, nil
	obs.Extract = ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
		return "", []dedup.Attachment{{Name: "q3.xlsx", SizeBytes: 9}}, errors.New("no text in this body")
	})
	out, err := p.Process(context.Background(), obs)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !out.Degraded {
		t.Fatal("a body without text was not recorded as degraded")
	}
	var env struct {
		Attachments []protocol.AttachmentDescriptor `json:"attachments"`
	}
	if err := json.Unmarshal(sink.last().Payload, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Attachments) != 1 || env.Attachments[0].Name != "q3.xlsx" {
		t.Fatalf("attachments = %+v, want the file the body named", env.Attachments)
	}
}

func TestWireAttachmentsKeepsTheRecordValid(t *testing.T) {
	long := strings.Repeat("é", protocol.MaxAttachmentNameRunes+10)
	var many []dedup.Attachment
	for range protocol.MaxAttachments + 3 {
		many = append(many, dedup.Attachment{Name: "a.txt", Path: "/" + strings.Repeat("p", 2000)})
	}
	many[0] = dedup.Attachment{Name: long, Path: "/" + strings.Repeat("p", protocol.MaxAttachmentPathRunes), SizeBytes: -4}
	many[1] = dedup.Attachment{Name: ""}
	got := wireAttachments(many)
	if len(got) != protocol.MaxAttachments {
		t.Fatalf("%d attachments, want the cap %d", len(got), protocol.MaxAttachments)
	}
	if first := got[0]; utf8.RuneCountInString(first.Name) != protocol.MaxAttachmentNameRunes || !strings.HasPrefix(long, first.Name) || first.Path != "" || first.SizeBytes != 0 {
		t.Fatalf("first = %d-character name, path %q, size %d; want the name cut to the limit, no path, size 0", utf8.RuneCountInString(first.Name), first.Path, first.SizeBytes)
	}
	total := 0
	for _, a := range got {
		if a.Name == "" {
			t.Fatal("an unnamed attachment was recorded")
		}
		total += len(a.Path)
	}
	if total == 0 || total > maxPathBytes {
		t.Fatalf("the record carries %d bytes of paths, want some and at most %d", total, maxPathBytes)
	}
}
