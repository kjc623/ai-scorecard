package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
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

func TestMergeLabelsStopsAtTheEnvelopeCap(t *testing.T) {
	var many []protocol.Label
	for i := 0; i < maxLabels+5; i++ {
		many = append(many, protocol.Label{Class: "c", RuleID: string(rune('A' + i))})
	}
	if got := mergeLabels(nil, many); len(got) != maxLabels {
		t.Fatalf("merged %d labels, want the cap %d", len(got), maxLabels)
	}
}
