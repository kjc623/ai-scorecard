package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// These tests are the executable form of the seams: they are what the integration verifier
// re-runs. Every test asserts a property one of the documents states, and the test names carry
// the document reference so a failure points at the paragraph that is now false.

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte(`{"hello":"classifier"}`)
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrameChecked(&buf)
	if err != nil {
		t.Fatalf("ReadFrameChecked: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload round trip: got %q want %q", got, payload)
	}
}

// §3.4: a version handshake on connect; a mismatch is `degraded`, never a crash and never a
// misparse of the stream.
func TestFrameVersionMismatchIsDetectable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrameVersion(&buf, Version+1, []byte("future")); err != nil {
		t.Fatalf("WriteFrameVersion: %v", err)
	}
	if _, err := ReadFrameChecked(&buf); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("ReadFrameChecked on a mismatched version = %v, want ErrVersionMismatch", err)
	}
	// The unchecked reader still reports the version, so a peer can be answered politely
	// instead of being dropped.
	var buf2 bytes.Buffer
	_ = WriteFrameVersion(&buf2, Version+1, []byte("future"))
	v, body, err := ReadFrame(&buf2)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if v != Version+1 || string(body) != "future" {
		t.Fatalf("ReadFrame = (%d, %q), want (%d, %q)", v, body, Version+1, "future")
	}
}

// A declared length over the cap must be refused without allocating it.
func TestFrameTooLargeIsRefusedBeforeAllocation(t *testing.T) {
	hdr := []byte{Version, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadFrame with an oversized header = %v, want ErrFrameTooLarge", err)
	}
	if err := WriteFrame(io.Discard, make([]byte, MaxFrameBytes+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("WriteFrame over the cap = %v, want ErrFrameTooLarge", err)
	}
}

// A truncated stream is an error, not a zero-length frame: a torn write must never look like a
// valid empty message.
func TestFrameTruncatedPayloadIsAnError(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, []byte("abcdef"))
	_, _, err := ReadFrame(bytes.NewReader(buf.Bytes()[:7]))
	if err == nil {
		t.Fatal("ReadFrame on a truncated payload returned no error")
	}
}

// §7.1/§11.2: at M0 the device is not permitted to read content. A frame that carries content
// while claiming it has none is a defect and is refused at the door.
func TestObservationContradictionIsRefused(t *testing.T) {
	o := ObservationMessage{
		ClientID:        "c1",
		Route:           RouteExtWebRequest,
		ToolFingerprint: "fp-1",
		OccurredAt:      time.Now(),
		SizeBytes:       10,
		HasContent:      false,
		Content:         []byte("secret"),
	}
	err := o.Validate()
	var re *RefusalError
	if !errors.As(err, &re) || re.Reason != RefusalMalformed {
		t.Fatalf("Validate = %v, want a malformed refusal", err)
	}
}

func TestObservationRouteMustBeAnExtensionRoute(t *testing.T) {
	o := ObservationMessage{Route: RouteProxyTLS, ToolFingerprint: "fp"}
	if err := o.Validate(); err == nil {
		t.Fatal("an observation claiming the proxy.tls route was accepted; only the extension's three routes may arrive over native messaging")
	}
}

// §3.4: the manifest lets capture-core refuse an oversized upload before any byte moves.
func TestAttachmentOverCapIsRefusedAtTheManifest(t *testing.T) {
	o := ObservationMessage{
		ClientID:        "c2",
		Route:           RouteExtPageContext,
		ToolFingerprint: "fp-2",
		HasContent:      true,
		Attachments:     []AttachmentDescriptor{{Name: "big.pdf", SizeBytes: MaxAttachmentBytes + 1}},
	}
	err := o.Validate()
	var re *RefusalError
	if !errors.As(err, &re) || re.Reason != RefusalAttachmentTooLarge {
		t.Fatalf("Validate = %v, want attachment_too_large", err)
	}
}

// §3.2: `blocked`, `warned` and `logged` are never merged, and a value outside the set is not
// silently recorded as one of them.
func TestDecisionActionIsClosed(t *testing.T) {
	o := ObservationMessage{
		ClientID: "c3", Route: RouteExtDOM, ToolFingerprint: "fp-3",
		Decision: &Decision{RuleID: "R1", Action: "quarantined", DecidedLocally: true},
	}
	if err := o.Validate(); err == nil {
		t.Fatal("a decision action outside the closed set was accepted")
	}
	for _, a := range []string{ActionBlocked, ActionWarned, ActionLogged} {
		o.Decision.Action = a
		if err := o.Validate(); err != nil {
			t.Fatalf("action %q refused: %v", a, err)
		}
	}
}

// §4.3: health carries the closed counter set and a closed state set. An invented counter name
// would create a coverage path the reporting layer cannot group.
func TestHealthReportCountersAreClosed(t *testing.T) {
	h := NewHealthReport("d1", "proxy.tls", "1.0.0", time.Now())
	h.State = StateHealthy
	if err := h.Validate(); err != nil {
		t.Fatalf("a well-formed report was refused: %v", err)
	}
	if len(h.Counters) != len(AllCounters) {
		t.Fatalf("NewHealthReport populated %d counters, want %d so a zero counter is distinguishable from a missing one",
			len(h.Counters), len(AllCounters))
	}
	h.Counters["invented_counter"] = 1
	if err := h.Validate(); err == nil {
		t.Fatal("a counter outside the closed set was accepted")
	}
	delete(h.Counters, "invented_counter")
	h.State = CollectorState("unknown")
	if err := h.Validate(); err == nil {
		t.Fatal("a collector state outside the closed set was accepted")
	}
}

// §3.3: the classifier must be structurally unable to see identity. If someone adds an identity
// field to ClassifyRequest, this test fails before the code can ship.
func TestClassifyRequestCarriesNoIdentity(t *testing.T) {
	b, err := json.Marshal(ClassifyRequest{Mode: ModeM1, Content: []byte("x")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range IdentityFieldNames {
		if _, present := m[forbidden]; present {
			t.Fatalf("ClassifyRequest carries identity field %q (docs/01-collectors.md §3.3)", forbidden)
		}
	}
}

// §11.2: the mode is applied before content is read, and the classifier is the second gate.
func TestClassifyRequestRefusesContentAtM0(t *testing.T) {
	q := ClassifyRequest{Mode: ModeM0, Content: []byte("prompt text")}
	if err := q.Validate(); err == nil {
		t.Fatal("a request carrying content at M0 was accepted; M0 forbids reading content at all")
	}
	q.Mode = ""
	if err := q.Validate(); err == nil {
		t.Fatal("a request with no mode was accepted")
	}
	q = ClassifyRequest{Mode: ModeM1, Content: []byte("prompt text")}
	if err := q.Validate(); err != nil {
		t.Fatalf("a valid M1 request was refused: %v", err)
	}
}

// §9.7: `degraded` means classification was attempted and did not complete. §9.2 and §9.7's
// "not emitted when" column also fix the other half: an empty label set with `confidence: high`
// is a *legitimate* output meaning the classifier ran and found nothing, so it must validate.
func TestClassifyResponseConfidenceSemantics(t *testing.T) {
	ok := ClassifyResponse{
		ClassifierVersion: "c-1",
		Confidence:        ConfidenceHigh,
		Labels:            []Label{{Class: "payment_card", Score: 0.99}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a confident response was refused: %v", err)
	}
	noVersion := ok
	noVersion.ClassifierVersion = ""
	if err := noVersion.Validate(); err == nil {
		t.Fatal("a response with no classifier version was accepted; a label that cannot be attributed to a release is not evidence")
	}
	// The defect this test now pins: empty labels with high confidence is the classifier having
	// found nothing, which is a fact about the data, not a failure.
	empty := ClassifyResponse{ClassifierVersion: "c-1", Confidence: ConfidenceHigh}
	if err := empty.Validate(); err != nil {
		t.Fatalf("an empty label set with confidence high was refused: %v (docs/01-collectors.md §9.2)", err)
	}
	badScore := ok
	badScore.Labels = []Label{{Class: "x", Score: 1.5}}
	if err := badScore.Validate(); err == nil {
		t.Fatal("a label score outside [0,1] was accepted")
	}
	noClass := ok
	noClass.Labels = []Label{{Score: 0.5}}
	if err := noClass.Validate(); err == nil {
		t.Fatal("a label with no class was accepted")
	}
	// A degraded answer must name the stage that did not complete, and must not assert a
	// confident verdict at the same time.
	degraded := ClassifyResponse{ClassifierVersion: "c-1", Confidence: ConfidenceDegraded}
	if err := degraded.Validate(); err == nil {
		t.Fatal("a degraded response naming no failed stage was accepted; the cause would be unattributable")
	}
	degraded.Stages = []StageResult{{Stage: "model", Ran: true, Failed: true, Detail: DetailModelUnavailable}}
	if err := degraded.Validate(); err != nil {
		t.Fatalf("a well-formed degraded response was refused: %v", err)
	}
	degradedWithLabels := degraded
	degradedWithLabels.Labels = []Label{{Class: "credential", Score: 0.9}}
	if err := degradedWithLabels.Validate(); err == nil {
		t.Fatal("a degraded response carrying a confident label was accepted; every exhaustion path must emit degraded and no path may claim a verdict")
	}
	long := strings.Repeat("x", MaxExcerptChars+1)
	over := ok
	over.Excerpt = &Excerpt{Kind: ExcerptRedactedWindow, Text: long}
	if err := over.Validate(); err == nil {
		t.Fatal("an excerpt over the structural bound was accepted; 'minimised' is enforced, not advised")
	}
	badKind := ok
	badKind.Excerpt = &Excerpt{Kind: "full_payload", Text: "short"}
	if err := badKind.Validate(); err == nil {
		t.Fatal("an excerpt kind outside the closed set was accepted")
	}
	goodExcerpt := ok
	goodExcerpt.Excerpt = &Excerpt{Kind: ExcerptMatchSpan, Text: "4111 1111 1111 1111", OffsetStart: 10, OffsetEnd: 29}
	if err := goodExcerpt.Validate(); err != nil {
		t.Fatalf("a valid match_span excerpt was refused: %v", err)
	}
}

// The budget is on the wire in milliseconds. A time.Duration with a `budget_ms` JSON tag would
// marshal as nanoseconds and mean something different to every non-Go peer, which is the bug
// this test pins.
func TestClassifyRequestBudgetIsMillisecondsOnTheWire(t *testing.T) {
	q := ClassifyRequest{Mode: ModeM1, Content: []byte("x"), BudgetMS: 150}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"budget_ms":150`) {
		t.Fatalf("serialised request is %s, want budget_ms:150", b)
	}
	var back ClassifyRequest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d, ok := back.EffectiveBudget()
	if !ok || d != 150*time.Millisecond {
		t.Fatalf("round-tripped budget = %v (set=%v), want 150ms", d, ok)
	}
	// A duration set directly by a Go caller still works, so the two fields cannot disagree
	// about intent, and an unset budget is reported as absent rather than as zero.
	direct := ClassifyRequest{Mode: ModeM1, Budget: 2 * time.Second}
	if d, ok := direct.EffectiveBudget(); !ok || d != 2*time.Second {
		t.Fatalf("direct Budget = %v (set=%v), want 2s", d, ok)
	}
	if _, ok := (ClassifyRequest{Mode: ModeM1}).EffectiveBudget(); ok {
		t.Fatal("an unset budget reported itself as set; zero means 'no explicit budget', not 'fail now'")
	}
}

// The detail vocabulary is closed and every value the documents name must be in it, or a
// coverage report cannot group by cause.
func TestDetailVocabularyIsClosed(t *testing.T) {
	required := []Detail{
		DetailParserMemory, DetailParserTimeout, DetailParserCrash, DetailParserOutputCap,
		DetailModelUnavailable, DetailNormaliseTruncated, DetailContentOverCap,
		DetailUndecodableContent, DetailReleaseLoadFailed, DetailModeViolation,
		DetailBudgetExhausted, DetailHostUnreachable, DetailContentUnprocessable,
		DetailParserFailed, DetailPortHeldByOther, DetailKilled, DetailVersionMismatch,
		// §13.3 rule 4: the four causes behind the `tampered` state a policy-verification
		// failure produces. A closed vocabulary that cannot name them cannot report them.
		DetailBundleSignatureInvalid, DetailBundleSchemaInvalid,
		DetailBundleVersionRegression, DetailBundleArtefactMissing,
	}
	for _, d := range required {
		if !d.Valid() {
			t.Fatalf("detail %q is named in the documents but missing from the closed vocabulary", d)
		}
	}
	if Detail("parser_oom").Valid() {
		t.Fatal("an invented detail reported itself as valid; a locally invented cause cannot be grouped")
	}
	if !DetailNone.Valid() {
		t.Fatal("an empty detail must be valid: a healthy provider has no cause to report")
	}
	for _, d := range AllDetails {
		if !d.Valid() {
			t.Fatalf("%q is in AllDetails but not Valid", d)
		}
	}
}

// A health report carrying a detail outside the vocabulary is refused for the same reason.
func TestHealthReportDetailIsClosed(t *testing.T) {
	h := NewHealthReport("d1", "classifier-host", "1.0.0", time.Now())
	h.State = StateDegraded
	h.Detail = DetailParserMemory
	if err := h.Validate(); err != nil {
		t.Fatalf("a report with a valid detail was refused: %v", err)
	}
	h.Detail = Detail("something_invented")
	if err := h.Validate(); err == nil {
		t.Fatal("a report with a detail outside the closed vocabulary was accepted")
	}
	// The bundle-failure causes are the ones a `tampered` row uses, and they must survive
	// validation because dropping a tamper signal is worse than any other reporting defect.
	h.State = StateTampered
	for _, cause := range []Detail{
		DetailBundleSignatureInvalid, DetailBundleSchemaInvalid,
		DetailBundleVersionRegression, DetailBundleArtefactMissing,
	} {
		h.Detail = cause
		if err := h.Validate(); err != nil {
			t.Fatalf("a tampered report naming cause %q was refused: %v", cause, err)
		}
	}
}

// The attachment descriptor uses the contract's field names verbatim, because the contract is
// closed: a local `digest` forwarded into an envelope would be rejected by ingest.
func TestAttachmentDescriptorUsesContractFieldNames(t *testing.T) {
	b, err := json.Marshal(AttachmentDescriptor{Name: "q4.pdf", SizeBytes: 10, ContentDigest: "sha256:abc"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["content_digest"]; !ok {
		t.Fatalf("descriptor serialised as %s; the contract's $defs/attachment requires content_digest", b)
	}
	if _, ok := m["digest"]; ok {
		t.Fatalf("descriptor serialised as %s; `digest` is not a contract field and additionalProperties is false", b)
	}
	if _, ok := m["name"]; !ok {
		t.Fatalf("descriptor serialised as %s; it lost the contract's required `name`", b)
	}
}

// §5.3: a batch that parses always yields per-event outcomes, and a truncated or padded body is
// caught by the redundant event_count.
func TestEventBatchEnvelopeRules(t *testing.T) {
	good := EventBatch{SchemaVersion: "1.0", BatchID: "b1", EventCount: 1, Events: []json.RawMessage{json.RawMessage(`{}`)}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid batch was refused: %v", err)
	}
	mismatch := good
	mismatch.EventCount = 2
	if err := mismatch.Validate(); err == nil {
		t.Fatal("event_count disagreeing with len(events) was accepted")
	}
	empty := good
	empty.Events = nil
	empty.EventCount = 0
	if err := empty.Validate(); err == nil {
		t.Fatal("a batch with zero events was accepted; the minimum is 1")
	}
	noID := good
	noID.BatchID = ""
	if err := noID.Validate(); err == nil {
		t.Fatal("a batch with no batch_id was accepted; idempotency depends on it")
	}
}

// §7: the reason codes are a closed set and only `duplicate_batch` is retryable.
func TestReasonCodesAreClosedAndRetryability(t *testing.T) {
	for _, c := range AllReasonCodes {
		if !c.Valid() {
			t.Fatalf("%q is in AllReasonCodes but not Valid", c)
		}
		if c.Retryable() != (c == ReasonDuplicateBatch) {
			t.Fatalf("retryability of %q is wrong: every code is terminal for the event except duplicate_batch", c)
		}
	}
	if ReasonCode("content_leak").Valid() {
		t.Fatal("an unknown reason code reported itself as valid")
	}
}

// A device settles its spool on the strength of this response, so an inconsistent response must
// be rejected rather than parsed optimistically.
func TestEventBatchResponseValidation(t *testing.T) {
	now := time.Now()
	won := true
	resp := EventBatchResponse{
		SchemaVersion: "1.0",
		BatchID:       "b1",
		ReceivedAt:    now,
		ServerTime:    now,
		Counts:        BatchCounts{Accepted: 1, Duplicate: 0, Rejected: 1},
		Results: []EventResult{
			{EventID: "e1", Outcome: OutcomeAccepted, SubmissionID: "s1", WonFields: &won},
			{EventID: "e2", Outcome: OutcomeRejected, Reason: ReasonModeViolation},
		},
	}
	if err := resp.Validate([]string{"e1", "e2"}); err != nil {
		t.Fatalf("a valid response was refused: %v", err)
	}
	bad := resp
	bad.Counts = BatchCounts{Accepted: 2, Rejected: 0}
	if err := bad.Validate([]string{"e1", "e2"}); err == nil {
		t.Fatal("counts disagreeing with results were accepted")
	}
	outOfOrder := resp
	outOfOrder.Results = []EventResult{resp.Results[1], resp.Results[0]}
	if err := outOfOrder.Validate([]string{"e1", "e2"}); err == nil {
		t.Fatal("results out of request order were accepted; the device matches them positionally")
	}
	noReason := resp
	noReason.Results = []EventResult{resp.Results[0], {EventID: "e2", Outcome: OutcomeRejected}}
	if err := noReason.Validate([]string{"e1", "e2"}); err == nil {
		t.Fatal("a rejection with no reason code was accepted")
	}
	contradiction := resp
	contradiction.Results = []EventResult{{EventID: "e1", Outcome: OutcomeAccepted, Reason: ReasonOversize}, resp.Results[1]}
	contradiction.Counts = BatchCounts{Accepted: 1, Rejected: 1}
	if err := contradiction.Validate([]string{"e1", "e2"}); err == nil {
		t.Fatal("an accepted event carrying a rejection reason was accepted")
	}
}

// §8: the outcome-to-spool mapping lives in exactly one place, and a retryable rejection must
// stay pending rather than being dropped or settled as rejected.
func TestOutcomeSettlesSpoolCorrectly(t *testing.T) {
	cases := []struct {
		outcome Outcome
		reason  ReasonCode
		want    SpoolState
		settled bool
	}{
		{OutcomeAccepted, "", SpoolDelivered, true},
		{OutcomeDuplicate, "", SpoolDelivered, true},
		{OutcomeRejected, ReasonModeViolation, SpoolRejected, true},
		{OutcomeRejected, ReasonDuplicateBatch, SpoolPending, false},
		{Outcome("nonsense"), "", SpoolPending, false},
	}
	for _, c := range cases {
		got, settled := c.outcome.SettleState(c.reason)
		if got != c.want || settled != c.settled {
			t.Fatalf("SettleState(%q, %q) = (%q, %v), want (%q, %v)", c.outcome, c.reason, got, settled, c.want, c.settled)
		}
	}
}

// §12: a spool entry that cannot be delivered, or whose kind is outside the closed registry, is
// refused at the door rather than stored and retried forever.
func TestSpoolEntryValidation(t *testing.T) {
	good := Entry{
		Seq: 1, Kind: KindPrompt, Route: RouteExtWebRequest, CollectionMode: ModeM1,
		DedupKey: "sha256:" + strings.Repeat("a", 64), Payload: json.RawMessage(`{}`), State: SpoolPending,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid entry was refused: %v", err)
	}
	noPayload := good
	noPayload.Payload = nil
	if err := noPayload.Validate(); err == nil {
		t.Fatal("an entry with no payload was accepted")
	}
	badKind := good
	badKind.Kind = Kind("process_exec")
	if err := badKind.Validate(); err == nil {
		t.Fatal("an entry whose kind is outside the closed registry was accepted; R7 depends on this being impossible")
	}
	noDedup := good
	noDedup.DedupKey = ""
	if err := noDedup.Validate(); err == nil {
		t.Fatal("an entry with no dedup key was accepted")
	}
	badMode := good
	badMode.CollectionMode = CollectionMode("m9")
	if err := badMode.Validate(); err == nil {
		t.Fatal("an entry with a mode outside the closed set was accepted")
	}
	badState := good
	badState.State = SpoolState("queued")
	if err := badState.Validate(); err == nil {
		t.Fatal("an entry with a state outside the closed set was accepted")
	}
}

// §11.2: ReadsContent is the gate every content path consults. A default here would be a silent
// mode widening, so an unknown mode must not read.
func TestModeReadsContent(t *testing.T) {
	if ModeM0.ReadsContent() {
		t.Fatal("M0 reported that it permits reading content")
	}
	for _, m := range []CollectionMode{ModeM1, ModeM2, ModeM3} {
		if !m.ReadsContent() {
			t.Fatalf("%s reported that it forbids reading content", m)
		}
	}
	if CollectionMode("").ReadsContent() {
		t.Fatal("an unset mode reported that it permits reading content; an unknown mode must fail closed for reads")
	}
	if CollectionMode("m9").Valid() {
		t.Fatal("an unknown mode reported itself as valid")
	}
}
