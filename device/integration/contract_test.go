package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// The device's discovery and agent_activity vocabularies are the contract's, in both directions:
// every contract value is one the device accepts, and every device constant is one the contract
// defines.
func TestDeviceFactVocabulariesAreTheContracts(t *testing.T) {
	for _, v := range envelope.AllDiscoveryTypes() {
		if !protocol.DiscoveryType(v).Valid() {
			t.Errorf("the device refuses discovery_type %q", v)
		}
	}
	for _, v := range envelope.AllDetectionBases() {
		if !protocol.DetectionBasis(v).Valid() {
			t.Errorf("the device refuses detection_basis %q", v)
		}
	}
	for _, v := range envelope.AllActivityTypes() {
		if !protocol.ActivityType(v).Valid() {
			t.Errorf("the device refuses activity_type %q", v)
		}
	}
	for _, v := range envelope.AllOutcomes() {
		if !protocol.ActivityOutcome(v).Valid() {
			t.Errorf("the device refuses outcome %q", v)
		}
	}
	for _, v := range []protocol.DiscoveryType{protocol.DiscoveryTypeAppInstalled, protocol.DiscoveryTypeAppRunning,
		protocol.DiscoveryTypeCLIInstalled, protocol.DiscoveryTypeIDEExtension, protocol.DiscoveryTypeLocalModel,
		protocol.DiscoveryTypeInferenceConnection} {
		if !envelope.DiscoveryType(v).Valid() {
			t.Errorf("the contract has no discovery_type %q", v)
		}
	}
	for _, v := range []protocol.DetectionBasis{protocol.DetectionBasisInstalledScan, protocol.DetectionBasisPackageScan,
		protocol.DetectionBasisExtensionScan, protocol.DetectionBasisProcessEvent, protocol.DetectionBasisModelStore,
		protocol.DetectionBasisPortListen, protocol.DetectionBasisFlowMetadata} {
		if !envelope.DetectionBasis(v).Valid() {
			t.Errorf("the contract has no detection_basis %q", v)
		}
	}
	for _, v := range []protocol.ActivityType{protocol.ActivityTypeModelRequest, protocol.ActivityTypeToolCall} {
		if !envelope.ActivityType(v).Valid() {
			t.Errorf("the contract has no activity_type %q", v)
		}
	}
	for _, v := range []protocol.ActivityOutcome{protocol.ActivityOutcomeSuccess, protocol.ActivityOutcomeError, protocol.ActivityOutcomeDenied} {
		if !envelope.Outcome(v).Valid() {
			t.Errorf("the contract has no outcome %q", v)
		}
	}
	for _, k := range []protocol.Kind{protocol.KindPrompt, protocol.KindUsageRollup, protocol.KindDiscovery, protocol.KindAgentActivity} {
		if !envelope.Kind(k).Valid() {
			t.Errorf("the contract has no kind %q", k)
		}
	}
	for _, r := range []protocol.Route{protocol.RouteToolHook, protocol.RouteToolOTel, protocol.RouteInvScan, protocol.RouteNetFlow} {
		if !envelope.Route(r).Valid() {
			t.Errorf("the contract has no route %q", r)
		}
	}
}

// discoveryFact is a discovery of one type, with the fields that type carries.
func discoveryFact(dt envelope.DiscoveryType, basis envelope.DetectionBasis, route protocol.Route, n int) core.Fact {
	f := core.Fact{
		Kind:            protocol.KindDiscovery,
		Route:           route,
		ToolFingerprint: "app:cursor",
		Person:          &core.Person{UserRef: "unattributed"},
		OccurredAt:      time.Now(),
		DedupKey:        "sha256:" + fmt.Sprintf("%064x", n+1),
		FactFields: core.FactFields{
			DiscoveryType:  protocol.DiscoveryType(dt),
			DetectionBasis: protocol.DetectionBasis(basis),
			AppVersion:     "0.48.1",
			Publisher:      "Anysphere, Inc.",
		},
	}
	switch dt {
	case envelope.DiscoveryTypeIdeExtension:
		f.ToolFingerprint, f.HostApp = "app:github_copilot", "app:vscode"
		f.Person = &core.Person{UserRef: testUser, SubjectName: "ada@contoso.com"}
	case envelope.DiscoveryTypeLocalModel:
		f.ToolFingerprint, f.ModelNames = "app:ollama", []string{"llama3.1:8b", "qwen2.5-coder:7b"}
	case envelope.DiscoveryTypeInferenceConnection:
		f.ToolFingerprint, f.DestinationHost = "app:openai_api", "api.openai.com"
		f.AppVersion, f.Publisher = "", ""
	}
	return f
}

// activityFact is an agent_activity of one type, with the fields that type carries.
func activityFact(at envelope.ActivityType, outcome envelope.Outcome, route protocol.Route, n int) core.Fact {
	ms := int64(850)
	f := core.Fact{
		Kind:            protocol.KindAgentActivity,
		Route:           route,
		ToolFingerprint: "app:claude_code",
		OccurredAt:      time.Now(),
		DedupKey:        "sha256:" + fmt.Sprintf("%064x", n+1),
		FactFields: core.FactFields{
			ActivityType: protocol.ActivityType(at),
			DurationMS:   &ms,
			Outcome:      protocol.ActivityOutcome(outcome),
		},
	}
	if at == envelope.ActivityTypeModelRequest {
		in, out := int64(1200), int64(0)
		f.Model, f.InputTokens, f.OutputTokens = "claude-sonnet-4-5", &in, &out
	} else {
		f.ToolName = "Bash"
	}
	return f
}

// Every discovery_type, detection_basis, activity_type and outcome the contract defines, minted
// through Pipeline.Record at every collection mode on the routes that emit them, is accepted the
// way ingest-api accepts it.
func TestEveryRecordedFactIsAcceptedByTheContract(t *testing.T) {
	schema := deviceSubmissionSchema(t)
	modes := []protocol.CollectionMode{protocol.ModeM0, protocol.ModeM1, protocol.ModeM2, protocol.ModeM3}
	bases := envelope.AllDetectionBases()
	discoveryRoutes := []protocol.Route{protocol.RouteInvScan, protocol.RouteProcDetect, protocol.RouteNetFlow}
	activityRoutes := []protocol.Route{protocol.RouteToolOTel, protocol.RouteToolHook}

	var facts []core.Fact
	for i, dt := range envelope.AllDiscoveryTypes() {
		facts = append(facts, discoveryFact(dt, bases[i%len(bases)], discoveryRoutes[i%len(discoveryRoutes)], len(facts)))
	}
	for i, basis := range bases {
		facts = append(facts, discoveryFact(envelope.DiscoveryTypeAppInstalled, basis, discoveryRoutes[i%len(discoveryRoutes)], len(facts)))
	}
	for i, at := range envelope.AllActivityTypes() {
		for j, outcome := range envelope.AllOutcomes() {
			facts = append(facts, activityFact(at, outcome, activityRoutes[(i+j)%len(activityRoutes)], len(facts)))
		}
	}
	bare := activityFact(envelope.ActivityTypeToolCall, "", protocol.RouteToolHook, len(facts))
	bare.DurationMS, bare.ToolName = nil, ""
	facts = append(facts, bare)

	for _, mode := range modes {
		for _, f := range facts {
			name := fmt.Sprintf("%s/%s/%s-%s/%s", mode, f.Kind, f.DiscoveryType, f.DetectionBasis, f.Route)
			if f.Kind == protocol.KindAgentActivity {
				name = fmt.Sprintf("%s/%s/%s-%s/%s", mode, f.Kind, f.ActivityType, f.Outcome, f.Route)
			}
			t.Run(name, func(t *testing.T) {
				sp := openSpool(t)
				p := newPipeline(t, sp, &recordingClassifier{}, mode)
				if err := p.Record(context.Background(), f); err != nil {
					t.Fatalf("Record: %v", err)
				}
				entries, err := sp.Peek(10)
				if err != nil || len(entries) != 1 {
					t.Fatalf("spool holds %d entries (%v), want 1", len(entries), err)
				}
				if entries[0].Kind != f.Kind || entries[0].Route != f.Route || entries[0].CollectionMode != mode {
					t.Fatalf("spool entry = %+v", entries[0])
				}
				if err := acceptLikeIngest(t, schema, entries[0].Payload); err != nil {
					t.Fatalf("ingest would refuse the envelope: %v\n%s", err, entries[0].Payload)
				}
				sub, err := envelope.DecodeDeviceSubmission(entries[0].Payload)
				if err != nil {
					t.Fatal(err)
				}
				switch v := sub.(type) {
				case *envelope.DeviceDiscovery:
					if string(v.DiscoveryType) != string(f.DiscoveryType) || string(v.DetectionBasis) != string(f.DetectionBasis) {
						t.Fatalf("decoded discovery = %+v", v)
					}
				case *envelope.DeviceAgentActivity:
					if string(v.ActivityType) != string(f.ActivityType) {
						t.Fatalf("decoded agent_activity = %+v", v)
					}
				default:
					t.Fatalf("decoded as %T", sub)
				}
			})
		}
	}
}
