package integration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// The real device components drive one observation from an extension frame to a record in the
// spool. The one fake is the classifier host behind its process boundary. Proven
// here: the pieces compose, M0 reads no content, a refused spool write is reported, and the record
// that reaches the spool is a contract-shaped envelope.

const (
	testTenant = "44444444-4444-4444-8444-444444444444"
	testDevice = "aaaaaaaa-0000-7000-8000-000000000001"
	testUser   = "u_integration"
)

// recordingClassifier counts what it was asked, so a test can assert the pipeline did NOT call it -
// which is how "M0 reads nothing" is checked rather than asserted.
type recordingClassifier struct {
	calls   int
	lastReq protocol.ClassifyRequest
	resp    protocol.ClassifyResponse
	err     error
}

func (c *recordingClassifier) Classify(_ context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	c.calls++
	c.lastReq = req
	if c.err != nil {
		return protocol.ClassifyResponse{}, c.err
	}
	return c.resp, nil
}

// countingReader reports whether the pipeline ever asked for content bytes.
type countingReader struct {
	reads int
	body  []byte
}

func (r *countingReader) Read(context.Context) ([]byte, error) {
	r.reads++
	if r.body == nil {
		return []byte("this must never be read at M0"), nil
	}
	return r.body, nil
}

type passthroughExtractor struct{ calls int }

func (e *passthroughExtractor) Extract(payload []byte, _ string) (string, []dedup.Attachment, error) {
	e.calls++
	return string(payload), nil, nil
}

type memoryContentStore struct{ puts int }

func (s *memoryContentStore) Put(context.Context, string, []byte, time.Time) error {
	s.puts++
	return nil
}

func openSpool(t *testing.T) *capturespool.Spool {
	t.Helper()
	key := make([]byte, capturespool.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sp, err := capturespool.Open(capturespool.Config{
		Dir:       t.TempDir(),
		Key:       key,
		SyncEvery: -1, // a process kill is not under test here; -1 skips fsync and is test-only
	})
	if err != nil {
		t.Fatalf("open spool: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func bundleWith(mode protocol.CollectionMode) *policy.Bundle {
	return &policy.Bundle{
		Version:         "integration-1",
		EffectiveAt:     time.Now().Add(-time.Hour),
		TenantDefault:   mode,
		ToolModes:       map[string]protocol.CollectionMode{"genai.web.chat.v1:chatgpt": mode},
		PopulationModes: map[string]protocol.CollectionMode{},
		DeviceModes:     map[string]protocol.CollectionMode{},
		ClassModes:      map[string]protocol.CollectionMode{},
	}
}

func newPipeline(t *testing.T, sp *capturespool.Spool, cl *recordingClassifier, mode protocol.CollectionMode) *core.Pipeline {
	t.Helper()
	p, err := core.NewPipeline(sp, time.Now, func() string { return "5f0f0c1e-0000-4000-8000-000000000001" })
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}
	p.SetIdentity(core.Identity{TenantID: testTenant, DeviceID: testDevice, UserRef: testUser})
	p.Bundles = func() *policy.Bundle { return bundleWith(mode) }
	p.Classifier = cl
	p.Retention = 24 * time.Hour
	return p
}

// observationFromFrame turns a golden extension frame into what a provider would hand the pipeline.
// This is the conversion the native-messaging host performs, and it is the step where the content
// encoding either survives or does not.
func observationFromFrame(t *testing.T, path string) protocol.ObservationMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var caseFile struct {
		Frame json.RawMessage `json:"frame"`
	}
	if err := json.Unmarshal(raw, &caseFile); err != nil {
		t.Fatalf("frame case file: %v", err)
	}
	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(caseFile.Frame, &envelope); err != nil {
		t.Fatalf("frame: %v", err)
	}
	var obs protocol.ObservationMessage
	if err := json.Unmarshal(envelope.Body, &obs); err != nil {
		t.Fatalf("body: %v", err)
	}
	if err := obs.Validate(); err != nil {
		t.Fatalf("the extension produced a frame the protocol refuses: %v", err)
	}
	return obs
}

// toCoreObservation is the native-messaging host's conversion step: a decoded frame becomes what
// the pipeline consumes. It is a function rather than a method because protocol is not this
// package's type - and it is here rather than in a component because it IS the seam between them.
func toCoreObservation(o protocol.ObservationMessage, reader core.ContentReader, ex core.Extractor) core.Observation {
	decision := o.Decision
	if decision == nil {
		decision = &protocol.Decision{RuleID: "INTEGRATION-1", Action: protocol.ActionLogged, DecidedLocally: true}
	}
	obs := core.Observation{
		Route:             o.Route,
		Kind:              protocol.KindPrompt,
		ToolFingerprint:   o.ToolFingerprint,
		OccurredAt:        o.OccurredAt,
		MonotonicOffsetMS: o.MonotonicOffsetMS,
		SizeBytes:         o.SizeBytes,
		Decision:          decision,
		Extract:           ex,
		ClientID:          o.ClientID,
		OverCap:           o.OverCap,
	}
	if o.HasContent {
		// Hand over a reader, never the bytes: a pipeline that is given bytes has already read
		// them, and the mode could no longer be applied before the read.
		obs.Content = reader
	}
	return obs
}

// The full path: an extension frame becomes a spooled envelope, and the envelope is a contract
// record rather than a private struct.
func TestDevicePath_ExtensionFrameBecomesASpooledContractEnvelope(t *testing.T) {
	sp := openSpool(t)
	cl := &recordingClassifier{resp: protocol.ClassifyResponse{
		ClassifierVersion: "integration-rules-1",
		Confidence:        protocol.ConfidenceHigh,
		Labels:            []protocol.Label{{Class: "customer_pii", Score: 0.91, RuleID: "PII_1"}},
	}}
	p := newPipeline(t, sp, cl, protocol.ModeM1)

	obs := observationFromFrame(t, filepath.Join("testdata", "native", "text-ascii.json"))
	reader := &countingReader{body: obs.Content}
	ex := &passthroughExtractor{}
	out, err := p.Process(context.Background(), toCoreObservation(obs, reader, ex))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !out.Emitted {
		t.Fatalf("observation was not emitted: reason=%q outcome=%+v", out.Reason, out)
	}
	if cl.calls != 1 {
		t.Fatalf("classifier called %d times, want 1 at M1", cl.calls)
	}

	entries, err := sp.Peek(10)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool holds %d entries, want 1", len(entries))
	}
	e := entries[0]
	if err := e.Validate(); err != nil {
		t.Fatalf("the spooled entry does not satisfy the protocol's own record rules: %v", err)
	}

	// The payload must be a contract envelope: the fields the contract requires, at the mode the
	// policy resolved, with the content facts the classifier produced.
	var env map[string]json.RawMessage
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	required := []string{
		"schema_version", "event_id", "tenant_id", "device_id", "user_ref",
		"tool_fingerprint", "direction", "kind", "occurred_at", "monotonic_offset_ms",
		"source", "collection_mode", "dedup_key",
	}
	for _, f := range required {
		if _, ok := env[f]; !ok {
			t.Fatalf("spooled envelope is missing contract-required field %q", f)
		}
	}
	// A device must never send received_at: the server assigns it, and a device-supplied receive
	// time would be neither device time nor server time.
	if _, ok := env["received_at"]; ok {
		t.Fatal("spooled envelope carries received_at; the contract forbids it on a device submission")
	}
	if string(env["collection_mode"]) != `"m1"` {
		t.Fatalf("collection_mode is %s, want \"m1\"", env["collection_mode"])
	}
	if string(env["tenant_id"]) != `"`+testTenant+`"` {
		t.Fatalf("tenant_id is %s, want the pipeline identity", env["tenant_id"])
	}
}

// M0 is the invariant the product rests on, and the check that matters is not "the field is
// absent" - it is "nothing asked for the bytes". A pipeline that reads and then discards would
// pass a field-level check and fail this one.
func TestDevicePath_M0NeverReadsContent(t *testing.T) {
	sp := openSpool(t)
	cl := &recordingClassifier{}
	p := newPipeline(t, sp, cl, protocol.ModeM0)

	obs := observationFromFrame(t, filepath.Join("testdata", "native", "m0-no-content.json"))
	reader := &countingReader{}
	ex := &passthroughExtractor{}
	out, err := p.Process(context.Background(), toCoreObservation(obs, reader, ex))
	if err != nil {
		t.Fatalf("Process at M0: %v", err)
	}
	if reader.reads != 0 {
		t.Fatalf("M0 read the payload %d time(s); the mode is applied before content is read", reader.reads)
	}
	if cl.calls != 0 {
		t.Fatalf("M0 called the classifier %d time(s); there is no content to classify", cl.calls)
	}
	if !out.Emitted {
		t.Fatalf("an M0 observation must still be recorded (tool identity and volume are still reported): reason=%q", out.Reason)
	}

	entries, _ := sp.Peek(10)
	if len(entries) != 1 {
		t.Fatalf("spool holds %d entries, want 1", len(entries))
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(entries[0].Payload, &env); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, forbidden := range []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments"} {
		if _, ok := env[forbidden]; ok {
			t.Fatalf("M0 envelope carries %q; the contract forbids every content-derived field at M0, and its "+
				"presence is evidence the device read something it was not permitted to read", forbidden)
		}
	}
}

// The endpoint's worst failure is not losing data, it is breaking the user or lying about coverage.
// A spool that refuses writes must be visible in the coverage row, never silently swallowed.
func TestDevicePath_SpoolRefusalIsReportedNotSwallowed(t *testing.T) {
	cl := &recordingClassifier{resp: protocol.ClassifyResponse{
		ClassifierVersion: "integration-rules-1",
		Confidence:        protocol.ConfidenceHigh,
		Labels:            []protocol.Label{{Class: "credential", Score: 0.8}},
	}}
	p, err := core.NewPipeline(failingSink{}, time.Now, func() string { return "5f0f0c1e-0000-4000-8000-000000000002" })
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}
	p.SetIdentity(core.Identity{TenantID: testTenant, DeviceID: testDevice, UserRef: testUser})
	p.Bundles = func() *policy.Bundle { return bundleWith(protocol.ModeM1) }
	p.Classifier = cl

	obs := observationFromFrame(t, filepath.Join("testdata", "native", "text-ascii.json"))
	reader := &countingReader{body: obs.Content}
	ex := &passthroughExtractor{}
	out, err := p.Process(context.Background(), toCoreObservation(obs, reader, ex))
	if err == nil && out.Emitted {
		t.Fatalf("a spool that refuses every write produced a successful outcome: %+v", out)
	}
	if out.Reason != core.ReasonSpoolUnavailable {
		t.Fatalf("outcome reason is %q, want %q - a refusal must be attributable, not generic", out.Reason, core.ReasonSpoolUnavailable)
	}

	// And the coverage row must exist for the route, because an absent path that reports nothing is
	// indistinguishable from a path with no data. The counter set is the
	// device's own record that a path was in the product and did not work.
	if cs := p.Counters(protocol.RouteExtWebRequest); cs == nil {
		t.Fatal("no counter set for the route after a refused write, so the refusal would be invisible in the coverage row")
	}
}

// failingSink is a spool that cannot accept a record, used to drive the refusal path.
type failingSink struct{}

func (failingSink) Append(protocol.Entry) (protocol.Entry, error) {
	return protocol.Entry{}, errors.New("spool: unwritable (integration harness)")
}

func (failingSink) Stats() protocol.SpoolStats { return protocol.SpoolStats{} }
