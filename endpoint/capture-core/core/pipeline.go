package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// ErrModeForbidsRead is returned when a caller asks for content at a mode that forbids reading
// it. The M0 path is a refusal rather than a silent empty result: an empty body and a forbidden
// body are different facts, and a caller that confused them would emit a clean record for
// content it never read.
var ErrModeForbidsRead = errors.New("core: mode forbids reading content")

// ContentReader is the only route to payload bytes. It is an interface rather than a []byte
// field on purpose: a pipeline handed bytes has already read them, so the mode could no longer be
// applied before the read. A provider with a body to offer hands over a reader and lets the
// resolved mode decide whether it is ever called.
type ContentReader interface {
	Read(ctx context.Context) ([]byte, error)
}

// Classifier is the classifier host as the pipeline uses it: bytes in, labels out. The request
// type has no identity fields at all, so a provider cannot leak identity into a classification.
type Classifier interface {
	Classify(ctx context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error)
}

// ContentStore holds M3 content on the device, keyed by event, until a per-event grant uploads it
// or local retention removes it. Content never enters the envelope.
type ContentStore interface {
	Put(ctx context.Context, eventID string, content []byte, expiresAt time.Time) error
}

// ContentStateReporter is implemented by a content store that can say how much it holds, so the
// device's own health can show it. Nothing about it enters an envelope: a device claiming it
// holds content is not evidence that it does.
type ContentStateReporter interface {
	HeldObjects() int
	HeldBytes() int64
}

// Sink is the narrow view of the spool the pipeline needs.
type Sink interface {
	Append(e protocol.Entry) (protocol.Entry, error)
	Stats() protocol.SpoolStats
}

// Any protocol.Store is usable as a Sink, so the spool is handed to the pipeline directly.
var _ Sink = protocol.Store(nil)

// Extractor turns a route-specific payload into the user-authored text and the attachment list.
// Extraction is route-specific by nature: a proxy reading messages[] and an extension reading a
// compose box arrive at the same characters by different means.
type Extractor interface {
	Extract(payload []byte, mediaType string) (text string, attachments []dedup.Attachment, err error)
}

// ExtractorFunc adapts a function.
type ExtractorFunc func(payload []byte, mediaType string) (string, []dedup.Attachment, error)

// Extract calls f.
func (f ExtractorFunc) Extract(payload []byte, mediaType string) (string, []dedup.Attachment, error) {
	return f(payload, mediaType)
}

// Person is who an observation is attributed to: the pseudonymous user_ref and, while the
// tenant's device identity setting is clear, the account name.
type Person struct {
	UserRef     string
	SubjectName string
}

// Observation is what a provider hands the pipeline: metadata the provider can obtain without
// reading content, plus a lazy ContentReader. SizeBytes must be known at M0 (every mode records
// it), so a provider that learns the size only by reading counts bytes as it streams without
// retaining them.
type Observation struct {
	Route           protocol.Route
	Kind            protocol.Kind
	ToolFingerprint string
	Population      string
	MediaType       string

	OccurredAt        time.Time
	MonotonicOffsetMS int64
	SizeBytes         int64

	// Decision is what policy did about this observation. A prompt records one at every mode,
	// including M0: a tenant can block a tool outright without reading content.
	Decision *protocol.Decision

	// Content is nil when the provider has nothing to offer (M0, or a metadata-only
	// observation). A non-nil reader at M0 is never called.
	Content ContentReader

	// Extract is the route's text extraction. When it fails the observation takes the Tier S
	// surrogate key with confidence degraded; it is never dropped, because a route that cannot
	// identify the authored text must not guess.
	Extract Extractor

	// OverCap records that the body exceeded the cap and was not held in memory: it is sized and
	// a digest of the prefix is recorded, nothing is classified, and the record says so.
	OverCap bool

	// Attachments are descriptors the provider already has (file names from a multipart body,
	// for example). They feed the Tier S names digest at M0.
	Attachments []dedup.Attachment

	// Person, when set, attributes this one observation to a known person instead of the
	// pipeline's identity (the console user). The browser relay sets it from the OS account of
	// the browser process.
	Person *Person

	// AttachmentContent is the bytes of attachments the route read, for classification only. At a
	// mode that reads content each is classified and its labels join the event's; the bytes never
	// enter the envelope, the spool or the content store. Their descriptors travel in Attachments.
	AttachmentContent []AttachmentContent

	ClientID string
}

// AttachmentContent is one attachment's bytes behind a reader, so the mode decides whether they
// are read at all.
type AttachmentContent struct {
	MediaType string
	Content   ContentReader
}

// Outcome is what the pipeline did with an observation. It feeds the provider's coverage row,
// not the wire.
type Outcome struct {
	Route      protocol.Route
	Mode       protocol.CollectionMode
	EventID    string
	Seq        uint64
	Emitted    bool
	Reason     string
	Confidence protocol.Confidence
	Degraded   bool
}

// Reasons an outcome can carry. They are stable strings so a coverage report can group by them.
const (
	ReasonEmitted            = "emitted"
	ReasonSpoolUnavailable   = "spool_unavailable"
	ReasonClassifierDegraded = "classifier_degraded"
	ReasonExtractionDegraded = "extraction_degraded"
	ReasonOverCap            = "over_cap"
	ReasonInvalidEnvelope    = "envelope_refused"
	ReasonIdentityUnresolved = "identity_unresolved"
	// ReasonContentUnheld means an M3 observation's content was not held: the local write failed,
	// or the content is larger than one upload may be. The event is still emitted.
	ReasonContentUnheld = "content_unheld"
)

// DefaultRetention is the device-side retention when the policy bundle states none.
const DefaultRetention = 30 * 24 * time.Hour

// Pipeline resolves the mode, applies it before content is read, mints the envelope and writes
// it through the spool.
type Pipeline struct {
	// identity is the envelope identity, installed once enrolment resolves it and read once per
	// observation, so scope, envelope and dedup key always agree.
	identity identityHolder

	Clock func() time.Time
	NewID func() string

	// Bundles returns the bundle in force; nil means none, which resolves to M0.
	Bundles func() *policy.Bundle

	Classifier Classifier
	Content    ContentStore
	Sink       Sink

	// Retention is the device-side retention used when the bundle states none.
	Retention time.Duration

	// ClassifyBudget bounds one classification. A classification that would exceed it degrades
	// rather than blocking the submission.
	ClassifyBudget time.Duration

	// PromptKind decides the request kind from the captured payload and the extracted text. It
	// defaults to decidePromptKind; a test pins it to isolate the pipeline from a client's exact
	// wording.
	PromptKind promptKindFunc

	mu        sync.Mutex
	counters  map[protocol.Route]*CounterSet
	lastOK    map[protocol.Route]time.Time
	lastClass map[protocol.Route]string
	startedAt time.Time
}

// identityHolder is the race-free holder for the envelope identity, which enrolment sets on a
// different goroutine than the providers that mint envelopes. Until an identity is issued the
// pipeline refuses to mint rather than stamping a placeholder.
type identityHolder struct {
	mu     sync.RWMutex
	id     Identity
	issued bool
}

func (h *identityHolder) set(id Identity) {
	h.mu.Lock()
	h.id = id
	h.issued = true
	h.mu.Unlock()
}

func (h *identityHolder) get() (Identity, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.id, h.issued
}

// NewPipeline returns a pipeline. Sink must be non-nil: a pipeline with nowhere to write would
// either drop silently or block, and both are forbidden.
func NewPipeline(sink Sink, clock func() time.Time, newID func() string) (*Pipeline, error) {
	if sink == nil {
		return nil, errors.New("core: pipeline needs a spool sink; a provider with nowhere to write must not start")
	}
	if clock == nil {
		clock = time.Now
	}
	if newID == nil {
		newID = randomID
	}
	return &Pipeline{
		Sink:           sink,
		Clock:          clock,
		NewID:          newID,
		Retention:      DefaultRetention,
		ClassifyBudget: 2 * time.Second,
		PromptKind:     decidePromptKind,
		counters:       map[protocol.Route]*CounterSet{},
		lastOK:         map[protocol.Route]time.Time{},
		lastClass:      map[protocol.Route]string{},
		startedAt:      clock(),
	}, nil
}

// ErrIdentityUnresolved is returned when a provider asks the pipeline to mint an envelope before
// enrolment has issued an identity. Traffic is still carried; it is never attributed to a
// placeholder.
var ErrIdentityUnresolved = errors.New("core: identity is unresolved; refusing to mint an envelope without an issued identity")

// SetIdentity installs the envelope identity: the tenant and device enrolment issued, and the
// console user. Safe to call concurrently with Process.
func (p *Pipeline) SetIdentity(id Identity) { p.identity.set(id) }

// Identity returns the current identity and whether one has been issued.
func (p *Pipeline) Identity() (Identity, bool) { return p.identity.get() }

// Counters returns the counter set for a route, creating it on first use.
func (p *Pipeline) Counters(route protocol.Route) *CounterSet {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.counters[route]
	if !ok {
		c = NewCounterSet(p.startedAt)
		p.counters[route] = c
	}
	return c
}

// MarkSuccess records the last unit of work that completed for a route. Health derives from this
// positive observation, never from the absence of errors.
func (p *Pipeline) MarkSuccess(route protocol.Route, t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastOK[route] = t
}

// LastSuccess returns the last completed unit of work for a route, zero when none.
func (p *Pipeline) LastSuccess(route protocol.Route) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastOK[route]
}

// ContentState reports how much M3 content the device holds locally. A store that does not
// report state answers zero.
func (p *Pipeline) ContentState() (objects int, bytes int64) {
	if r, ok := p.Content.(ContentStateReporter); ok {
		return r.HeldObjects(), r.HeldBytes()
	}
	return 0, 0
}

// ResolveMode is how a provider asks for the effective mode before it reads anything. The device
// always comes from the issued identity; the user is the query's when it names one, else the
// identity's. Before enrolment the device is empty, which resolves to M0.
func (p *Pipeline) ResolveMode(q ScopeQuery) Resolution {
	id, _ := p.identity.get()
	return p.resolveWith(q, id)
}

func (p *Pipeline) resolveWith(q ScopeQuery, id Identity) Resolution {
	var b *policy.Bundle
	if p.Bundles != nil {
		b = p.Bundles()
	}
	q.DeviceID = id.DeviceID
	if q.UserRef == "" {
		q.UserRef = id.UserRef
	}
	return Resolve(b, q)
}

// retention is the device-side retention in force: the bundle's when it states one.
func (p *Pipeline) retention() time.Duration {
	if p.Bundles != nil {
		if b := p.Bundles(); b != nil && b.Spool.DeviceRetentionHours > 0 {
			return time.Duration(b.Spool.DeviceRetentionHours) * time.Hour
		}
	}
	if p.Retention > 0 {
		return p.Retention
	}
	return DefaultRetention
}

// Process applies the mode, reads content only if the mode permits it, mints the envelope and
// spools it. The order of the steps is the enforcement:
//
//  1. resolve the effective mode from the signed bundle (no bundle resolves to M0);
//  2. M0: build the metadata envelope and never touch the body: no hash, no label, no excerpt;
//  3. M1 and above: compute the canonical digest and classify the authored text;
//  4. M2: take the minimised excerpt;
//  5. M3: hold the content locally for a per-event grant; the envelope carries no content.
func (p *Pipeline) Process(ctx context.Context, obs Observation) (Outcome, error) {
	c := p.Counters(obs.Route)
	c.Add(protocol.CounterObserved)

	id, issued := p.identity.get()
	if obs.Person != nil {
		id.UserRef, id.SubjectName = obs.Person.UserRef, obs.Person.SubjectName
	}
	res := p.resolveWith(ScopeQuery{
		ToolFingerprint: obs.ToolFingerprint,
		Population:      obs.Population,
		UserRef:         id.UserRef,
	}, id)
	out := Outcome{Route: obs.Route, Mode: res.Mode, Reason: ReasonEmitted}
	if !obs.Kind.Valid() {
		return out, fmt.Errorf("core: observation has kind %q outside the closed registry", obs.Kind)
	}

	// The identity gate runs after mode resolution and before any content read: a device that
	// has not enrolled resolves M0 and refuses to mint, so it never reads content it cannot
	// attribute.
	if !issued || id.TenantID == "" {
		c.Add(protocol.CounterErrors)
		return Outcome{Route: obs.Route, Mode: res.Mode, Degraded: true, Reason: ReasonIdentityUnresolved}, ErrIdentityUnresolved
	}

	eventID := p.NewID()
	retention := p.retention()
	in := EnvelopeInput{
		Identity:          id,
		EventID:           eventID,
		Kind:              obs.Kind,
		Route:             obs.Route,
		Mode:              res.Mode,
		ToolFingerprint:   obs.ToolFingerprint,
		OccurredAt:        obs.OccurredAt,
		MonotonicOffsetMS: obs.MonotonicOffsetMS,
		Decision:          obs.Decision,
	}

	if obs.Kind == protocol.KindPrompt {
		size := obs.SizeBytes
		in.SizeBytes = &size

		if !res.ReadsContent() {
			// M0: the content reader is not called, not dereferenced, not hashed. The dedup key is
			// the weaker payload-shape surrogate, and the record carries that weakness.
			key, err := dedup.SurrogateKey(id.TenantID, id.DeviceID, obs.ToolFingerprint,
				string(obs.Kind), obs.OccurredAt, size, obs.Attachments)
			if err != nil {
				c.Add(protocol.CounterErrors)
				return out, err
			}
			in.DedupKey = key
			return p.finish(c, obs, in, out, retention)
		}

		body, err := readContent(ctx, res.Mode, obs.Content)
		if err != nil {
			// The gate refused: a defect upstream, not an observation. It is counted and no record
			// claiming nothing was found is minted.
			c.Add(protocol.CounterErrors)
			out.Reason = ReasonInvalidEnvelope
			return out, err
		}

		text, atts, xerr := p.extract(obs, body)
		canonical := xerr == nil && !obs.OverCap
		var digest string
		if canonical {
			digest = dedup.ContentDigest(text, atts)
		} else {
			// The digest is not canonical: an over-cap body is digested over its prefix, and a
			// route that could not identify the authored text digests the body as observed. The
			// digest is still emitted (M1 and above require one) with confidence degraded, and
			// the key stays on Tier S: two routes agree on a Tier T key only when both derived
			// it from canonical text, and claiming one anyway would silently over-merge.
			digest = SHA256Hex(body)
		}

		tier := dedup.TierT
		if !canonical {
			tier = dedup.TierS
		}
		switch {
		case obs.OverCap:
			out.Degraded = true
			out.Reason = ReasonOverCap
		case xerr != nil:
			out.Degraded = true
			out.Reason = ReasonExtractionDegraded
			c.Add(protocol.CounterErrors)
		}
		if out.Degraded {
			c.Add(protocol.CounterErrors)
		}

		key, err := p.dedupKey(id, tier, obs, digest, size, atts)
		if err != nil {
			c.Add(protocol.CounterErrors)
			return out, err
		}
		in.DedupKey = key
		in.ContentDigest = digest
		in.Attachments = wireAttachments(atts)
		// The request kind is decided once, here, from the captured payload and the extracted
		// text. It gates classification below and, downstream, the search index and the default
		// event list.
		decider := p.PromptKind
		if decider == nil {
			decider = decidePromptKind
		}
		in.PromptKind = decider(body, text)

		switch {
		case obs.OverCap:
			// Nothing is classified. The record still names a classifier version and carries an
			// empty label set with confidence degraded: classification did not complete.
			in.ClassifierVersion = p.classifierVersion(obs.Route)
			in.Confidence = protocol.ConfidenceDegraded
			in.Labels = []protocol.Label{}
		case in.PromptKind == protocol.PromptKindClientGenerated:
			// The client made this request for itself (titling, a summary, telemetry). Nobody
			// typed anything, so nothing is classified and no label is invented from the client's
			// own instructions; the record states "nothing a person typed", with high
			// confidence.
			in.ClassifierVersion = p.classifierVersion(obs.Route)
			in.Confidence = protocol.ConfidenceHigh
			in.Labels = []protocol.Label{}
		default:
			// The classifier sees the text a person authored, not the whole body: the body also
			// carries the client's system prompt and tool definitions, and classifying those
			// labels a plain question as source code. Where no authored text was found the whole
			// body is classified and the record is already degraded.
			resp, cerr := p.classify(ctx, res.Mode, obs.MediaType, classifyInput(text, body, xerr))
			if cerr != nil {
				// Classifier unavailable or over budget: the request is carried unclassified and
				// the record says classification was attempted and did not complete.
				out.Degraded = true
				if out.Reason == ReasonEmitted {
					out.Reason = ReasonClassifierDegraded
				}
				in.ClassifierVersion = p.classifierVersion(obs.Route)
				in.Confidence = protocol.ConfidenceDegraded
				in.Labels = []protocol.Label{}
				c.Add(protocol.CounterErrors)
			} else {
				in.ClassifierVersion = resp.ClassifierVersion
				in.Confidence = resp.Confidence
				in.Labels = resp.Labels
				if in.Labels == nil {
					in.Labels = []protocol.Label{}
				}
				p.setClassifierVersion(obs.Route, resp.ClassifierVersion)
				if res.Mode == protocol.ModeM2 {
					in.Excerpt = excerptForM2(resp)
				}
				if !canonical {
					// The labels are real; the confidence band is not.
					in.Confidence = protocol.ConfidenceDegraded
				}
			}
		}
		if len(obs.AttachmentContent) > 0 {
			p.classifyAttachments(ctx, c, obs.AttachmentContent, res.Mode, &in, &out)
		}
		if res.Mode == protocol.ModeM2 && in.Excerpt == nil {
			in.Excerpt = degradedExcerpt()
		}

		if res.Mode == protocol.ModeM3 {
			if p.Content == nil {
				c.Add(protocol.CounterErrors)
				out.Reason = ReasonInvalidEnvelope
				return out, errors.New("core: M3 observation but no local content store is configured")
			}
			// What is held is the prompt text where the route identified the authored text, and
			// the body as observed where it could not. It is the content object a grant uploads,
			// so it is held only when it fits in one upload.
			held := body
			if xerr == nil && text != "" {
				held = []byte(text)
			}
			if len(held) > protocol.MaxContentObjectBytes {
				out.Degraded = true
				out.Reason = ReasonContentUnheld
				c.Add(protocol.CounterDropped)
			} else if err := p.Content.Put(ctx, eventID, held, dedup.BucketStart(obs.OccurredAt).Add(retention)); err != nil {
				// A failed local write is a named gap: the event still goes, with the classifier's
				// output, because dropping it would lose the metadata too.
				out.Degraded = true
				out.Reason = ReasonContentUnheld
				c.Add(protocol.CounterDropped)
			}
		}
	}

	out.Confidence = in.Confidence
	return p.finish(c, obs, in, out, retention)
}

func (p *Pipeline) finish(c *CounterSet, obs Observation, in EnvelopeInput, out Outcome, retention time.Duration) (Outcome, error) {
	raw, err := BuildEnvelope(in)
	if err != nil {
		c.Add(protocol.CounterErrors)
		out.Reason = ReasonInvalidEnvelope
		return out, err
	}
	entry := protocol.Entry{
		ClientID:          obs.ClientID,
		Kind:              in.Kind,
		Route:             in.Route,
		CollectionMode:    in.Mode,
		ToolFingerprint:   in.ToolFingerprint,
		OccurredAt:        in.OccurredAt,
		MonotonicOffsetMS: in.MonotonicOffsetMS,
		DedupKey:          in.DedupKey,
		Payload:           raw,
		SizeBytes:         int64(len(raw)),
		ExpiresAt:         dedup.BucketStart(in.OccurredAt).Add(retention),
		State:             protocol.SpoolPending,
	}
	if err := entry.Validate(); err != nil {
		c.Add(protocol.CounterErrors)
		out.Reason = ReasonInvalidEnvelope
		return out, err
	}
	stored, err := p.Sink.Append(entry)
	if err != nil {
		// The collector keeps collecting and the loss is counted at the provider; the request
		// itself is carried by the caller.
		c.Add(protocol.CounterDropped)
		out.Reason = ReasonSpoolUnavailable
		return out, fmt.Errorf("core: spool append failed for %s: %w", in.Route, err)
	}
	c.Add(protocol.CounterEmitted)
	p.MarkSuccess(in.Route, p.Clock())
	out.Emitted = true
	out.EventID = in.EventID
	out.Seq = stored.Seq
	return out, nil
}

// readContent is the single call site of ContentReader.Read in this package, and it refuses to
// read when the mode does not permit it. Being the only call site is what makes a read at M0
// impossible to express without deleting the gate.
func readContent(ctx context.Context, mode protocol.CollectionMode, r ContentReader) ([]byte, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("core: mode %q outside the closed set", mode)
	}
	if !mode.ReadsContent() {
		return nil, fmt.Errorf("%w: mode %s", ErrModeForbidsRead, mode)
	}
	if r == nil {
		return nil, nil
	}
	return r.Read(ctx)
}

// extract runs the route's text extraction. The attachment list falls back to the descriptors the
// provider already had, which is what the Tier S names digest is built from.
func (p *Pipeline) extract(obs Observation, body []byte) (string, []dedup.Attachment, error) {
	if obs.Extract == nil {
		return "", obs.Attachments, errors.New("core: no extractor for this route, so no canonical text exists")
	}
	text, atts, err := obs.Extract.Extract(body, obs.MediaType)
	if err != nil {
		return "", obs.Attachments, err
	}
	if len(atts) == 0 {
		atts = obs.Attachments
	}
	return text, atts, nil
}

func (p *Pipeline) dedupKey(id Identity, tier string, obs Observation, digest string, size int64, atts []dedup.Attachment) (string, error) {
	if tier == dedup.TierT {
		return dedup.ContentKey(id.TenantID, id.DeviceID, obs.ToolFingerprint, string(obs.Kind), obs.OccurredAt, digest)
	}
	return dedup.SurrogateKey(id.TenantID, id.DeviceID, obs.ToolFingerprint, string(obs.Kind), obs.OccurredAt, size, atts)
}

// classifyInput is what the classifier sees: the authored text when extraction found it,
// otherwise the body as observed.
func classifyInput(text string, body []byte, xerr error) []byte {
	if xerr == nil {
		return []byte(text)
	}
	return body
}

func (p *Pipeline) classify(ctx context.Context, mode protocol.CollectionMode, mediaType string, body []byte) (protocol.ClassifyResponse, error) {
	if p.Classifier == nil {
		return protocol.ClassifyResponse{}, errors.New("core: no classifier configured")
	}
	budget := p.ClassifyBudget
	req := protocol.ClassifyRequest{
		Content:   body,
		Mode:      mode,
		MediaType: mediaType,
		// BudgetMS is the wire field (an explicit integer, so a JavaScript host cannot read a
		// nanosecond Duration as milliseconds); Budget carries the same value for Go peers.
		BudgetMS: int64(budget / time.Millisecond),
		Budget:   budget,
	}
	if err := req.Validate(); err != nil {
		return protocol.ClassifyResponse{}, err
	}
	cctx := ctx
	cancel := func() {}
	if budget > 0 {
		cctx, cancel = context.WithTimeout(ctx, budget)
	}
	defer cancel()
	resp, err := p.Classifier.Classify(cctx, req)
	if err != nil {
		return protocol.ClassifyResponse{}, err
	}
	if err := resp.Validate(); err != nil {
		return protocol.ClassifyResponse{}, fmt.Errorf("core: classifier response refused: %w", err)
	}
	return resp, nil
}

// maxLabels is the envelope's cap on one event's labels.
const maxLabels = 64

// classifyAttachments classifies each attachment under the observation's mode and merges its
// labels into the event's classification. An attachment the classifier could not answer for makes
// the event degraded: something it carried was not classified.
func (p *Pipeline) classifyAttachments(ctx context.Context, c *CounterSet, atts []AttachmentContent, mode protocol.CollectionMode, in *EnvelopeInput, out *Outcome) {
	for _, a := range atts {
		body, err := readContent(ctx, mode, a.Content)
		if err != nil || len(body) == 0 {
			continue
		}
		resp, err := p.classify(ctx, mode, a.MediaType, body)
		if err != nil || resp.Confidence == protocol.ConfidenceDegraded {
			in.Confidence = protocol.ConfidenceDegraded
			out.Degraded = true
			if out.Reason == ReasonEmitted {
				out.Reason = ReasonClassifierDegraded
			}
			c.Add(protocol.CounterErrors)
			continue
		}
		in.Labels = mergeLabels(in.Labels, resp.Labels)
		in.Confidence = leastConfident(in.Confidence, resp.Confidence)
		if in.ClassifierVersion == "" || in.ClassifierVersion == RulesOnlyVersion {
			in.ClassifierVersion = resp.ClassifierVersion
		}
	}
}

// mergeLabels adds the labels of b to a, keeping the higher score where both carry the same class
// and rule, and stops at the envelope's cap.
func mergeLabels(a, b []protocol.Label) []protocol.Label {
	out := append([]protocol.Label{}, a...)
	for _, l := range b {
		merged := false
		for i := range out {
			if out[i].Class == l.Class && out[i].RuleID == l.RuleID {
				out[i].Score = max(out[i].Score, l.Score)
				merged = true
				break
			}
		}
		if !merged && len(out) < maxLabels {
			out = append(out, l)
		}
	}
	return out
}

// leastConfident is the lower of two confidence bands; an empty band is no information.
func leastConfident(a, b protocol.Confidence) protocol.Confidence {
	rank := map[protocol.Confidence]int{protocol.ConfidenceDegraded: 0, protocol.ConfidenceLow: 1, protocol.ConfidenceMedium: 2, protocol.ConfidenceHigh: 3}
	if a == "" {
		return b
	}
	if rb, ok := rank[b]; ok && rb < rank[a] {
		return b
	}
	return a
}

// RulesOnlyVersion is the classifier version recorded when the classifier could not answer. A
// version is required at M1 and above, so the fallback is named rather than left empty.
const RulesOnlyVersion = "rules-only"

func (p *Pipeline) classifierVersion(route protocol.Route) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.lastClass[route]; v != "" {
		return v
	}
	return RulesOnlyVersion
}

func (p *Pipeline) setClassifierVersion(route protocol.Route, v string) {
	if v == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastClass[route] = v
}

// excerptForM2 takes the classifier's minimised excerpt and enforces the character cap
// structurally: an over-cap excerpt is truncated rather than emitted whole.
func excerptForM2(resp protocol.ClassifyResponse) *protocol.Excerpt {
	if resp.Excerpt == nil {
		return nil
	}
	ex := *resp.Excerpt
	r := []rune(ex.Text)
	if len(r) > protocol.MaxExcerptChars {
		ex.Text = string(r[:protocol.MaxExcerptChars])
	}
	return &ex
}

// degradedExcerpt is what an M2 record carries when classification did not complete: an empty,
// fully redacted window. An excerpt is required at M2, and inventing one from the payload would
// retain content nothing classified.
func degradedExcerpt() *protocol.Excerpt {
	return &protocol.Excerpt{Kind: "redacted_window", Text: "", RedactionApplied: true}
}

// wireAttachments converts canonicalisation inputs to the envelope's attachment shape: name, size
// and, where the bytes were read, their digest. The contract's attachment carries no media type, so
// the media type serves classification only and does not enter the envelope.
func wireAttachments(atts []dedup.Attachment) []protocol.AttachmentDescriptor {
	if len(atts) == 0 {
		return nil
	}
	out := make([]protocol.AttachmentDescriptor, 0, len(atts))
	for _, a := range atts {
		w := protocol.AttachmentDescriptor{Name: a.Name, SizeBytes: a.SizeBytes}
		if a.ContentDigest != "" && a.ContentDigest != dedup.Unreadable {
			w.ContentDigest = a.ContentDigest
		}
		out = append(out, w)
	}
	return out
}

// SHA256Hex is the plain digest used for an over-cap prefix or a body with no extracted text. It
// is deliberately not the canonical content digest, which would claim a canonicalisation that
// never happened.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// randomID mints a UUIDv4-shaped event id from crypto/rand. It is not a secret; it identifies one
// observation.
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
