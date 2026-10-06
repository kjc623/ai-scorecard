package integration

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/shadow-ai-capture/contracts/envelope"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/protocol"
)

// Every envelope the device path emits is accepted exactly the way ingest-api accepts it: it
// validates against the embedded contract schema's deviceSubmission with format assertion on, and
// it decodes with envelope.DecodeDeviceSubmission, which refuses unknown fields. The envelopes come
// from the real pipeline and the real spool, driven by the extension's own golden frames at every
// collection mode, so a field the device mints and the contract does not define fails here rather
// than at ingest.

// deviceSubmissionSchema compiles the contract schema the way ingest-api does.
func deviceSubmissionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(envelope.Schema))
	if err != nil {
		t.Fatalf("the embedded schema is not JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(envelope.SchemaID, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(envelope.SchemaID + "#/$defs/deviceSubmission")
	if err != nil {
		t.Fatalf("compile the schema: %v", err)
	}
	return schema
}

// acceptLikeIngest applies ingest-api's two checks to one envelope.
func acceptLikeIngest(t *testing.T, schema *jsonschema.Schema, raw []byte) error {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := schema.Validate(doc); err != nil {
		return err
	}
	if _, err := envelope.DecodeDeviceSubmission(raw); err != nil {
		return err
	}
	return nil
}

func TestEveryEmittedEnvelopeIsAcceptedByTheContract(t *testing.T) {
	schema := deviceSubmissionSchema(t)
	frames, err := filepath.Glob(filepath.Join("testdata", "native", "*.json"))
	if err != nil || len(frames) == 0 {
		t.Fatalf("no golden frames: %v", err)
	}
	modes := []protocol.CollectionMode{protocol.ModeM0, protocol.ModeM1, protocol.ModeM2, protocol.ModeM3}
	classifiers := map[string]*recordingClassifier{
		"classified": {resp: protocol.ClassifyResponse{
			ClassifierVersion: "integration-rules-1",
			Confidence:        protocol.ConfidenceHigh,
			Labels:            []protocol.Label{{Class: "customer_pii", Score: 0.91, RuleID: "PII_1"}},
			Excerpt:           &protocol.Excerpt{Kind: protocol.ExcerptMatchSpan, Text: "[redacted]", MatchType: "email", OffsetStart: 0, OffsetEnd: 10, RedactionApplied: true},
		}},
		"classifier unavailable": {err: errors.New("classifier host unavailable")},
	}

	checked := 0
	for _, frame := range frames {
		obs := observationFromFrame(t, frame)
		for _, mode := range modes {
			for clName, cl := range classifiers {
				name := strings.TrimSuffix(filepath.Base(frame), ".json") + "/" + string(mode) + "/" + clName
				t.Run(name, func(t *testing.T) {
					sp := openSpool(t)
					p := newPipeline(t, sp, cl, mode)
					p.Content = &memoryContentStore{}
					reader := &countingReader{body: obs.Content}
					out, err := p.Process(context.Background(), toCoreObservation(obs, reader, &passthroughExtractor{}))
					if err != nil || !out.Emitted {
						t.Fatalf("Process: emitted=%v err=%v", out.Emitted, err)
					}
					entries, err := sp.Peek(10)
					if err != nil || len(entries) != 1 {
						t.Fatalf("spool holds %d entries (%v), want 1", len(entries), err)
					}
					if err := acceptLikeIngest(t, schema, entries[0].Payload); err != nil {
						t.Fatalf("ingest would refuse the envelope: %v\n%s", err, entries[0].Payload)
					}
				})
				checked++
			}
		}
	}
	if checked < len(frames)*len(modes) {
		t.Fatalf("checked %d envelopes, want at least %d", checked, len(frames)*len(modes))
	}
}

// The envelopes a person is attributed to carry the clear account name when the tenant allows it;
// that field must be one the contract defines too.
func TestAnAttributedEnvelopeIsAcceptedByTheContract(t *testing.T) {
	schema := deviceSubmissionSchema(t)
	sp := openSpool(t)
	cl := &recordingClassifier{resp: protocol.ClassifyResponse{ClassifierVersion: "integration-rules-1", Confidence: protocol.ConfidenceHigh}}
	p := newPipeline(t, sp, cl, protocol.ModeM1)
	obs := observationFromFrame(t, filepath.Join("testdata", "native", "text-ascii.json"))
	co := toCoreObservation(obs, &countingReader{body: obs.Content}, &passthroughExtractor{})
	co.Person = &core.Person{UserRef: "u_0123456789abcdef0123456789abcdef", SubjectName: "ada@contoso.com"}
	if _, err := p.Process(context.Background(), co); err != nil {
		t.Fatalf("Process: %v", err)
	}
	entries, _ := sp.Peek(1)
	if len(entries) != 1 || !bytes.Contains(entries[0].Payload, []byte(`"subject_name":"ada@contoso.com"`)) {
		t.Fatalf("the envelope does not carry the subject name: %s", entries[0].Payload)
	}
	if err := acceptLikeIngest(t, schema, entries[0].Payload); err != nil {
		t.Fatalf("ingest would refuse the attributed envelope: %v", err)
	}
}
