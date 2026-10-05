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

// ErrModeForbidsRead is returned when a caller asks for content at a mode that forbids
// reading it. It exists so the M0 path is a refusal rather than a silent empty result: an
// empty body and a forbidden body are different facts, and a caller that confused them would
// emit a `clean` record for content it never read.
var ErrModeForbidsRead = errors.New("core: mode forbids reading content")

// ContentReader is the only route to payload bytes. It is an interface rather than a []byte
// field on purpose: a pipeline that holds bytes has already read them, and §11.2's ordering
// cannot be enforced after the fact. A provider that has a body to offer must hand over a
// reader and let the resolved mode decide whether it is ever called.
type ContentReader interface {
	Read(ctx context.Context) ([]byte, error)
}

// Classifier is the classifier-host as this component uses it: bytes in, labels out. The
// request type has no identity fields at all (protocol.ClassifyRequest), so a provider
// cannot leak identity into a classification even by accident.
type Classifier interface {
	Classify(ctx context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error)
}

// ContentStore is the M3 local content store: content keyed by event, retained locally, never
// in the envelope and never leaving the device except through a per-event grant (§13.4).
type ContentStore interface {
	Put(ctx context.Context, eventID string, content []byte, expiresAt time.Time) error
}

// ContentStateReporter is the optional device-local half of M3 that ADR 0017 keeps off the
// wire: how many content objects are held and how many bytes they occupy. A store that
// implements it can have its state surfaced in the device's own coverage row; nothing about
// it enters an envelope, because a device *claiming* it holds content is not evidence that it
// does.
type ContentStateReporter interface {
	HeldObjects() int
	HeldBytes() int64
}

// Sink is the narrow view of the spool this package needs. It is deliberately a local
// interface rather than a copy of protocol.Store: capture-spool implements protocol.Store,
// and the assertion below proves the view is a subset, so a spool can be handed straight to
// the pipeline without an adapter. Nothing here re-implements the spool.
type Sink interface {
	Append(e protocol.Entry) (protocol.Entry, error)
	Stats() protocol.SpoolStats
}

// Compile-time proof that the narrow view is a subset of the spool contract: any
// protocol.Store value is usable as a Sink, and widening protocol.Store cannot break this
// package silently.
var _ Sink = protocol.Store(nil)

// Extractor turns a route-specific payload into C1's two outputs: the user-authored text and
// the attachment list. Extraction is explicitly *outside* the normative dedup contract
// (docs/02 §4.2), because a proxy reading `messages[]` and an extension reading a compose box
// arrive at the same characters by different means.
type Extractor interface {
	Extract(payload []byte, mediaType string) (text string, attachments []dedup.Attachment, err error)
}

// ExtractorFunc adapts a function.
type ExtractorFunc func(payload []byte, mediaType string) (string, []dedup.Attachment, error)

// Extract calls f.
func (f ExtractorFunc) Extract(payload []byte, mediaType string) (string, []dedup.Attachment, error) {
	return f(payload, mediaType)
}

// Observation is what a provider hands the pipeline. Everything here is metadata the
// provider can obtain *without* reading content, plus a lazy ContentReader. SizeBytes must be
// known at M0 (the schema requires it at every mode), and a provider that can only learn the
// size by reading the body counts bytes as it streams without retaining them.
type Observation struct {
	Route           protocol.Route
	Kind            protocol.Kind
	ToolFingerprint string
	Population      string
	MediaType       string

	OccurredAt        time.Time
	MonotonicOffsetMS int64
	SizeBytes         int64

	// Decision is what policy did about this observation. A prompt event requires one at every
	// mode, including M0: a tenant can block a tool outright without reading content.
	Decision *protocol.Decision

	// Content is nil when the provider has nothing to offer (M0, or a metadata-only
	// observation). A non-nil reader at M0 is a defect and is caught: the gate refuses to
	// call it.
	Content ContentReader

	// Extract is the route's C1 implementation. When it fails the observation degrades to the
	// Tier S surrogate key with `confidence: degraded` — it is never dropped, because a route
	// that cannot identify the authored boundary must not guess (docs/02 §4.5).
	Extract Extractor

	// OverCap records that the body exceeded the cap and was not held in memory (§5.3): the
	// proxy records size and a digest of the prefix, classifies nothing, and says so.
	OverCap bool

	// AttachmentsKnown are descriptors the provider already has (filenames from a multipart
	// body, for example). They feed the Tier S names digest at M0.
	Attachments []dedup.Attachment

	ClientID string
}

// Outcome is what the pipeline did with an observation. It is reported to the coverage row,
// not to the wire: coverage is derived from the collection path, never declared separately.
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

// Reasons a pipeline outcome can carry. They are stable strings so a coverage report can
// group by them.
const (
	ReasonEmitted            = "emitted"
	ReasonSpoolUnavailable   = "spool_unavailable"
	ReasonClassifierDegraded = "classifier_degraded"
	ReasonExtractionDegraded = "extraction_degraded"
	ReasonOverCap            = "over_cap"
	ReasonInvalidEnvelope    = "envelope_refused"
	// ReasonNoCanonicaliser means C3 cannot be performed, so the digest is not canonical and
	// the observation is reported degraded rather than passing an unnormalised digest off as
	// the versioned `sac-canon-1` value.
	ReasonNoCanonicaliser = "canonicaliser_absent"
)

// Pipeline resolves the mode, applies it before content is read, mints the envelope and
// writes it through the spool.
type Pipeline struct {
	// identity is the race-free holder for the identity the pipeline stamps on envelopes. It is
	// resolved by the startup identity step (or a later enrolment) and read once per observation,
	// so scope, envelope and dedup key can never disagree.
	identity identityHolder

	Clock func() time.Time
	NewID func() string

	// Bundles returns the bundle currently in force; nil means "none", which resolves to M0.
	Bundles func() *policy.Bundle

	Classifier Classifier
	Content    ContentStore
	Sink       Sink

	// Normalizer is C3 of docs/02 §4.2. It has no default on purpose: Go's standard library has
	// no Unicode normalisation and a wrong digest presented as canonical is corruption, so a
	// pipeline with no normaliser installed takes the degraded path (Tier S key, `confidence:
	// degraded`) instead of silently computing an unnormalised `sac-canon-1` value.
	Normalizer dedup.Normalizer

	// Retention is the device-side retention deadline for spooled observations.
	Retention time.Duration

	// ClassifyBudget bounds one classification. A stage that would exceed it degrades rather
	// than blocking the submission (C21).
	ClassifyBudget time.Duration

	// BodyCap is the fallback cap when the bundle does not set one.
	BodyCap int64

	// PromptKind is the request-kind decider (task 08). It defaults to decidePromptKind; a test
	// pins it to isolate the pipeline from a client's exact wording. It never sees identity, only
	// the captured payload and the text C1 extracted.
	PromptKind promptKindFunc

	mu        sync.Mutex
	counters  map[protocol.Route]*CounterSet
	lastOK    map[protocol.Route]time.Time
	lastClass map[protocol.Route]string
	startedAt time.Time
}

// identityHolder is the race-free holder for the envelope identity. It is resolved by the startup
// identity step (or a later enrolment) on a different goroutine than the providers that mint
// envelopes, so a bare field would race. `required` records whether the run demands an issued
// credential (a drain-configured device); `issued` records whether one has been installed. A
// required-but-not-issued holder is the fail-closed state: the pipeline refuses to mint rather than
// stamping a placeholder.
type identityHolder struct {
	mu       sync.RWMutex
	id       Identity
	issued   bool
	required bool
}

func (h *identityHolder) set(id Identity) {
	h.mu.Lock()
	h.id = id
	h.issued = true
	h.mu.Unlock()
}

func (h *identityHolder) require(b bool) {
	h.mu.Lock()
	h.required = b
	h.mu.Unlock()
}

// snapshot returns id, issued and required under one lock, so a single observation's scope, envelope
// and dedup key see one consistent identity and one consistent gate decision.
func (h *identityHolder) snapshot() (Identity, bool, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.id, h.issued, h.required
}

func (h *identityHolder) get() (Identity, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.id, h.issued
}

// NewPipeline returns a pipeline. Sink must be non-nil: a pipeline with nowhere to write
// would either drop silently (forbidden, C22) or block (forbidden).
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
		Retention:      30 * 24 * time.Hour,
		ClassifyBudget: 2 * time.Second,
		PromptKind:     decidePromptKind,
		mu:             sync.Mutex{},
		counters:       map[protocol.Route]*CounterSet{},
		lastOK:         map[protocol.Route]time.Time{},
		lastClass:      map[protocol.Route]string{},
		startedAt:      clock(),
	}, nil
}

// ErrIdentityUnresolved is returned when a provider asks the pipeline to mint an envelope but no
// identity has been resolved (a drain-configured device that has not enrolled). It is the
// fail-closed half of §3.5: traffic is carried (fail-open) but never attributed to a placeholder.
var ErrIdentityUnresolved = errors.New("core: identity is unresolved; refusing to mint an envelope without an issued identity")

// ReasonIdentityUnresolved is the outcome reason a refused mint carries, so a coverage row can
// group the failure rather than reporting it as a generic error.
const ReasonIdentityUnresolved = "identity_unresolved"

// SetIdentity installs the issued envelope identity. It is the seam the drain uses: enrolment mints
// device_id (and tenant_id/region) server-side, and the write path validates an envelope's
// tenant_id/device_id against the issued credential, so the flags supplied at construction are only
// the no-drain/local fallback. Safe to call concurrently with Process and EmitEnvelope.
func (p *Pipeline) SetIdentity(id Identity) { p.identity.set(id) }

// RequireIdentity records whether this run demands an issued credential. When required and no
// identity has been issued yet, Process/EmitEnvelope refuse to mint (ErrIdentityUnresolved) rather
// than stamping a placeholder — fail-open for traffic, fail-closed for identity.
func (p *Pipeline) RequireIdentity(required bool) { p.identity.require(required) }

// Identity returns the current issued identity and whether one has been issued.
func (p *Pipeline) Identity() (Identity, bool) { return p.identity.get() }

// currentIdentity returns the issued identity snapshot (id, issued, required) under one lock, so a
// provider minting an envelope sees one consistent identity and one consistent gate decision.
func (p *Pipeline) currentIdentity() (Identity, bool, bool) { return p.identity.snapshot() }

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

// MarkSuccess records the last unit of work that completed for a route. Health derives from
// this positive observation, never from the absence of errors (C25).
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

// ContentState reports how much content the device is holding locally for M3. This is
// ADR 0017's device-local content-state marker: it never enters an envelope, and it is what
// an operator needs when diagnosing a missing upload. A store that does not report state
// answers zero rather than being guessed at.
func (p *Pipeline) ContentState() (objects int, bytes int64) {
	if r, ok := p.Content.(ContentStateReporter); ok {
		return r.HeldObjects(), r.HeldBytes()
	}
	return 0, 0
}

// ResolveMode is how a provider asks for the effective mode *before* it reads anything. The
// loopback broker uses it to decide whether to buffer a request body while it streams it to
// the upstream; at M0 the answer is no, and the body is forwarded without being retained.
//
// The resolved identity is injected into the scope query here, so every provider resolves scope
// against the same identity the pipeline stamps — never against the construction-time flags, which
// are only the no-drain/local fallback. An unresolved identity resolves against an empty device,
// which yields M0 (and Process/EmitEnvelope then refuses to mint).
func (p *Pipeline) ResolveMode(q ScopeQuery) Resolution {
	id, _, _ := p.currentIdentity()
	return p.resolveWith(q, id)
}

// resolveWith resolves scope for an already-snapshotted identity, so a single observation's scope,
// envelope and dedup key all use the same identity value.
func (p *Pipeline) resolveWith(q ScopeQuery, id Identity) Resolution {
	var b *policy.Bundle
	if p.Bundles != nil {
		b = p.Bundles()
	}
	q.DeviceID = id.DeviceID
	q.UserRef = id.UserRef
	return Resolve(b, q)
}

// Process applies the mode, reads content only if the mode permits it, mints the envelope and
// spools it.
//
// The ordering in this function is the enforcement of §11.2:
//
//  1. resolve effective_mode from the signed bundle (no bundle and no previous → M0)
//  2. M0: build the metadata envelope, then drop the body reference — never hash it, never
//     label it, never excerpt it
//  3. M1+: compute the normalised digest, hand bytes to the classifier
//  4. M2: take the minimised excerpt
//  5. M3: write content to the local store; the envelope notes only that content is held
//     locally (carried by collection_mode — see BuildEnvelope's open decision)
func (p *Pipeline) Process(ctx context.Context, obs Observation) (Outcome, error) {
	c := p.Counters(obs.Route)
	c.Add(protocol.CounterObserved)

	id, issued, required := p.currentIdentity()
	res := p.resolveWith(ScopeQuery{
		ToolFingerprint: obs.ToolFingerprint,
		Population:      obs.Population,
	}, id)
	out := Outcome{Route: obs.Route, Mode: res.Mode, Reason: ReasonEmitted}
	if !obs.Kind.Valid() {
		return out, fmt.Errorf("core: observation has kind %q outside the closed registry", obs.Kind)
	}

	// The identity gate runs AFTER mode resolution and BEFORE any content read: a drain-configured
	// device that has not yet enrolled resolves M0 and refuses to mint, so it never stamps a
	// placeholder and never reads content it cannot attribute. An empty identity is refused with the
	// named reason here, rather than falling through to the generic dedup.Key error. Fail-open for
	// the user's traffic, fail-closed for identity.
	if (required && !issued) || id.TenantID == "" {
		c.Add(protocol.CounterErrors)
		return Outcome{Route: obs.Route, Mode: res.Mode, Degraded: true, Reason: ReasonIdentityUnresolved}, ErrIdentityUnresolved
	}

	eventID := p.NewID()
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
			// M0. The content reader is not called, not dereferenced, not hashed. The dedup
			// key is the weaker payload-shape surrogate, and that weakness is reported
			// (§4.5) rather than hidden.
			key, err := dedup.SurrogateKey(id.TenantID, id.DeviceID, obs.ToolFingerprint,
				string(obs.Kind), obs.OccurredAt, size, obs.Attachments, p.Normalizer)
			if err != nil {
				c.Add(protocol.CounterErrors)
				return out, err
			}
			in.DedupKey = key
			return p.finish(ctx, c, obs, in, out)
		}

		body, err := readContent(ctx, res.Mode, obs.Content)
		if err != nil {
			// The gate refused: this is a defect upstream, not an observation. Count it and
			// refuse to mint a record rather than emitting one that claims nothing was found.
			c.Add(protocol.CounterErrors)
			out.Reason = ReasonInvalidEnvelope
			return out, err
		}

		text, atts, xerr := p.extract(obs, body)
		canonical := xerr == nil && p.Normalizer != nil && !obs.OverCap
		var digest string
		switch {
		case obs.OverCap:
			// §5.3: an over-cap body is not read into memory. It is sized and a digest of the
			// first N bytes is recorded, nothing is classified, and the record says so rather
			// than implying "clean". This digest is explicitly *not* the canonical content
			// digest, so the key ladder stays on Tier S.
			digest = SHA256Hex(body)
		case canonical:
			digest = dedup.ContentDigest(text, atts, p.Normalizer)
		default:
			// The digest exists but is not canonical: either no C3 normaliser is installed or
			// the route could not identify the user-authored segment. It is emitted (the schema
			// requires content_digest at M1 and above) with `confidence: degraded`, and the key
			// ladder stays on Tier S. **The key claims what the device can prove**: §4.5's Tier T
			// premise is that two routes deriving the key independently agree, and an unnormalised
			// digest does not satisfy that premise. Emitting the exact key anyway would be a
			// silent over-merge; the weak key leaves two observations of one submission as two
			// rows, which is a visible undercount with `merge_confidence: low` rather than a count
			// the server and the device disagree about (R9).
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
		case p.Normalizer == nil:
			out.Degraded = true
			out.Reason = ReasonNoCanonicaliser
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
		// The request kind is decided once, here, from the captured payload and the text C1
		// identified (task 08). It gates the classification input below and, downstream, the
		// search index and the event-list default.
		decider := p.PromptKind
		if decider == nil {
			decider = decidePromptKind
		}
		in.PromptKind = decider(body, text)

		if obs.OverCap {
			// §5.3: classify nothing. The record still carries the classifier's attribution and
			// an empty label set with `confidence: degraded`, which is the explicit signal that
			// classification did not complete.
			in.ClassifierVersion = p.classifierVersion(obs.Route)
			in.Confidence = protocol.ConfidenceDegraded
			in.Labels = []protocol.Label{}
		} else if in.PromptKind == protocol.PromptKindClientGenerated {
			// The client made this request for itself (titling a session, a summary, telemetry).
			// There is no text a person authored to classify, so the classifier is not run and no
			// label can be invented from the client's own instructions. The record still carries a
			// classifier version so it is attributable, an empty label set, and high confidence:
			// the honest statement is "nothing a person typed", not "classification failed".
			in.ClassifierVersion = p.classifierVersion(obs.Route)
			in.Confidence = protocol.ConfidenceHigh
			in.Labels = []protocol.Label{}
		} else {
			// 3. hand the classifier the text a person authored, not the whole captured body: the
			// body also carries the client's system prompt and tool definitions, and classifying
			// those is what gave a plain question a source_code label (task 08). Where C1 found no
			// authored boundary the whole body is handed over and the record is already degraded,
			// which is the honest best effort.
			resp, cerr := p.classify(ctx, obs, res.Mode, classifyInput(text, body, xerr), digest)
			if cerr != nil {
				// §5.4: classifier unavailable or over budget → carry the request unclassified.
				// The record says classification was attempted and did not complete.
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
					// 4. the minimised excerpt, from the classifier's match span.
					in.Excerpt = excerptForM2(resp)
				}
				if !canonical {
					// Non-canonical: the labels are real, the confidence band is not.
					in.Confidence = protocol.ConfidenceDegraded
				}
			}
		}
		if res.Mode == protocol.ModeM2 && in.Excerpt == nil {
			in.Excerpt = degradedExcerpt()
		}

		if res.Mode == protocol.ModeM3 {
			// 5. content is held locally, keyed by event; the envelope carries no excerpt.
			if p.Content == nil {
				c.Add(protocol.CounterErrors)
				out.Reason = ReasonInvalidEnvelope
				return out, errors.New("core: M3 observation but no local content store is configured; refusing rather than emitting an M3 record whose content does not exist")
			}
			// What is held is the prompt text where the route could identify the user-authored
			// segment, and the body as observed where it could not.
			held := body
			if xerr == nil && text != "" {
				held = []byte(text)
			}
			if err := p.Content.Put(ctx, eventID, held, dedup.BucketStart(obs.OccurredAt).Add(p.Retention)); err != nil {
				// A failed local write is a named gap, not a silent one: the event still goes,
				// with the classifier's output, because dropping it would lose the metadata
				// too. The content state is reported through the provider's health counters.
				out.Degraded = true
				out.Reason = "content_store_unwritable"
				c.Add(protocol.CounterDropped)
			}
		}
	}

	out.Confidence = in.Confidence
	return p.finish(ctx, c, obs, in, out)
}

// EmitEnvelope is the path for kinds a provider mints itself (a usage_rollup or a
// model_detection from proc.detect). It applies the same spool contract: validate, then
// append; a refused envelope is counted, never silently dropped.
func (p *Pipeline) EmitEnvelope(ctx context.Context, in EnvelopeInput) (Outcome, error) {
	c := p.Counters(in.Route)
	c.Add(protocol.CounterObserved)
	out := Outcome{Route: in.Route, Mode: in.Mode, Reason: ReasonEmitted}
	if in.EventID == "" {
		in.EventID = p.NewID()
	}
	if in.Identity.TenantID == "" || in.Identity.DeviceID == "" {
		id, issued, required := p.currentIdentity()
		if required && !issued {
			c.Add(protocol.CounterErrors)
			return Outcome{Route: in.Route, Mode: in.Mode, Degraded: true, Reason: ReasonIdentityUnresolved}, ErrIdentityUnresolved
		}
		if in.Identity == (Identity{}) {
			in.Identity = id
		} else {
			// Complete a partially-populated identity from the issued one; BuildEnvelope's field
			// policy checks presence, not emptiness, so an empty tenant/device must not reach it.
			if in.Identity.TenantID == "" {
				in.Identity.TenantID = id.TenantID
			}
			if in.Identity.DeviceID == "" {
				in.Identity.DeviceID = id.DeviceID
			}
			if in.Identity.UserRef == "" {
				in.Identity.UserRef = id.UserRef
			}
			// The clear account name travels the same way as the pseudonymous ref: a provider that
			// did not attribute a person inherits the pipeline's (ADR 0021).
			if in.Identity.SubjectName == "" {
				in.Identity.SubjectName = id.SubjectName
			}
		}
	}
	return p.finish(ctx, c, Observation{Route: in.Route, Kind: in.Kind}, in, out)
}

func (p *Pipeline) finish(ctx context.Context, c *CounterSet, obs Observation, in EnvelopeInput, out Outcome) (Outcome, error) {
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
		ExpiresAt:         dedup.BucketStart(in.OccurredAt).Add(p.Retention),
		State:             protocol.SpoolPending,
	}
	if err := entry.Validate(); err != nil {
		c.Add(protocol.CounterErrors)
		out.Reason = ReasonInvalidEnvelope
		return out, err
	}
	stored, err := p.Sink.Append(entry)
	if err != nil {
		// §5.4: spool unavailable or full → the collector keeps collecting and the loss is
		// counted at the provider. The request itself is carried by the caller.
		c.Add(protocol.CounterDropped)
		out.Reason = ReasonSpoolUnavailable
		return out, fmt.Errorf("core: spool append failed for %s: %w", in.Route, err)
	}
	c.Add(protocol.CounterEmitted)
	p.MarkSuccess(in.Route, p.Clock())
	out.Emitted = true
	out.EventID = in.EventID
	out.Seq = stored.Seq
	_ = ctx
	return out, nil
}

// readContent is the single call site of ContentReader.Read in this package, and it refuses
// to read when the mode does not permit it. Making it the only call site is what makes "a
// defect that reads content at M0" impossible to express without deleting the gate.
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

// extract runs the route's C1 extraction. The attachment list falls back to the descriptors
// the provider already had (filenames from a multipart body, for example), which is what the
// Tier S names digest is built from.
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
	switch tier {
	case dedup.TierT:
		return dedup.ContentKey(id.TenantID, id.DeviceID, obs.ToolFingerprint, string(obs.Kind), obs.OccurredAt, digest)
	default:
		return dedup.SurrogateKey(id.TenantID, id.DeviceID, obs.ToolFingerprint, string(obs.Kind), obs.OccurredAt, size, atts, p.Normalizer)
	}
}

// classifyInput is the bytes handed to the classifier: the text C1 identified as authored when it
// could (task 08), otherwise the body as observed. Handing over the whole body when an authored
// turn exists is the defect this exists to prevent: a question whose body carried a tool schema
// came back labelled source_code.
func classifyInput(text string, body []byte, xerr error) []byte {
	if xerr == nil {
		return []byte(text)
	}
	return body
}

func (p *Pipeline) classify(ctx context.Context, obs Observation, mode protocol.CollectionMode, body []byte, digest string) (protocol.ClassifyResponse, error) {
	if p.Classifier == nil {
		return protocol.ClassifyResponse{}, errors.New("core: no classifier configured")
	}
	budget := p.ClassifyBudget
	req := protocol.ClassifyRequest{
		Content:       body,
		Mode:          mode,
		MediaType:     obs.MediaType,
		ContentDigest: digest,
		// BudgetMS is the wire field (an explicit integer, so a JavaScript host cannot read a
		// nanosecond Duration as milliseconds); Budget carries the same value for the Go side.
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

// RulesOnlyVersion is the classifier version recorded when the classifier could not answer.
// §13.3 rule 6: with no loadable release, classification falls back to rules-only from the
// baseline with `confidence: degraded` — never unclassified-and-silent. The schema requires a
// version at M1 and above, so the fallback is named rather than left empty.
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

// excerptForM2 takes the classifier's minimised excerpt, enforcing the 2048-character cap
// structurally: an over-cap excerpt is truncated to the cap rather than emitted whole, since
// "minimised" is a property of the record, not a hope about the classifier.
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

// degradedExcerpt is what an M2 record carries when classification did not complete: a
// redacted window with everything redacted. It is the honest shape — the schema requires an
// excerpt at M2, and the alternative (inventing one from the payload) would be the device
// deciding on its own to retain content it could not classify.
func degradedExcerpt() *protocol.Excerpt {
	return &protocol.Excerpt{Kind: "redacted_window", Text: "", RedactionApplied: true}
}

// wireAttachments converts canonicalisation inputs to the contract's `$defs/attachment`
// shape. It uses protocol.AttachmentDescriptor directly — one shape, one name — so a
// descriptor forwarded from the extension through native messaging and one minted here cannot
// disagree about a field name.
func wireAttachments(atts []dedup.Attachment) []protocol.AttachmentDescriptor {
	if len(atts) == 0 {
		return nil
	}
	out := make([]protocol.AttachmentDescriptor, 0, len(atts))
	for _, a := range atts {
		w := protocol.AttachmentDescriptor{Name: a.Name, MediaType: a.MediaType, SizeBytes: a.SizeBytes}
		if a.ContentDigest != "" && a.ContentDigest != dedup.Unreadable {
			w.ContentDigest = a.ContentDigest
		}
		out = append(out, w)
	}
	return out
}

// SHA256Hex is the plain digest used for an over-cap prefix: §5.3 requires a digest of the
// first N bytes, which is deliberately not the canonical content digest (that would claim a
// canonicalisation that never happened).
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func randomID() string {
	// A UUIDv4-shaped identifier from crypto/rand, minted by the provider as the schema
	// requires. It is not a secret; it is an identity for one observation.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on any platform this ships to; a timestamp fallback keeps the
		// collector running rather than dropping the observation.
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
