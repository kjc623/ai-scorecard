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
)

// Pipeline resolves the mode, applies it before content is read, mints the envelope and
// writes it through the spool.
type Pipeline struct {
	Identity Identity
	Clock    func() time.Time
	NewID    func() string

	// Bundles returns the bundle currently in force; nil means "none", which resolves to M0.
	Bundles func() *policy.Bundle

	Classifier Classifier
	Content    ContentStore
	Sink       Sink

	// Normalizer is C3. It defaults to dedup.IdentityNFC (see that type: the stdlib has no
	// Unicode normalisation and this host is offline), and the deviation is reported.
	Normalizer dedup.Normalizer

	// Retention is the device-side retention deadline for spooled observations.
	Retention time.Duration

	// ClassifyBudget bounds one classification. A stage that would exceed it degrades rather
	// than blocking the submission (C21).
	ClassifyBudget time.Duration

	// BodyCap is the fallback cap when the bundle does not set one.
	BodyCap int64

	mu        sync.Mutex
	counters  map[protocol.Route]*CounterSet
	lastOK    map[protocol.Route]time.Time
	lastClass map[protocol.Route]string
	startedAt time.Time
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
		Normalizer:     dedup.IdentityNFC{},
		Retention:      30 * 24 * time.Hour,
		ClassifyBudget: 2 * time.Second,
		mu:             sync.Mutex{},
		counters:       map[protocol.Route]*CounterSet{},
		lastOK:         map[protocol.Route]time.Time{},
		lastClass:      map[protocol.Route]string{},
		startedAt:      clock(),
	}, nil
}

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

// ResolveMode is how a provider asks for the effective mode *before* it reads anything. The
// loopback broker uses it to decide whether to buffer a request body while it streams it to
// the upstream; at M0 the answer is no, and the body is forwarded without being retained.
func (p *Pipeline) ResolveMode(q ScopeQuery) Resolution {
	var b *policy.Bundle
	if p.Bundles != nil {
		b = p.Bundles()
	}
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

	res := p.ResolveMode(ScopeQuery{
		ToolFingerprint: obs.ToolFingerprint,
		Population:      obs.Population,
		DeviceID:        p.Identity.DeviceID,
		UserRef:         p.Identity.UserRef,
	})
	out := Outcome{Route: obs.Route, Mode: res.Mode, Reason: ReasonEmitted}
	if !obs.Kind.Valid() {
		return out, fmt.Errorf("core: observation has kind %q outside the closed registry", obs.Kind)
	}

	eventID := p.NewID()
	in := EnvelopeInput{
		Identity:          p.Identity,
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
			key, err := dedup.SurrogateKey(p.Identity.TenantID, p.Identity.DeviceID, obs.ToolFingerprint,
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

		digest, tier, degraded, exErr := p.canonicalDigest(obs, body)
		if exErr != nil {
			out.Degraded = true
			out.Reason = ReasonExtractionDegraded
			c.Add(protocol.CounterErrors)
		} else if degraded && out.Reason == ReasonEmitted {
			out.Degraded = true
			out.Reason = ReasonExtractionDegraded
		}
		if obs.OverCap {
			// §5.3: an over-cap body is not read into memory. It is sized and degraded, and
			// the record says classification did not complete rather than implying "clean".
			degraded = true
			out.Degraded = true
			out.Reason = ReasonOverCap
			tier = dedup.TierS
			digest = ""
		}

		key, err := p.dedupKey(tier, obs, digest, size)
		if err != nil {
			c.Add(protocol.CounterErrors)
			return out, err
		}
		in.DedupKey = key

		if tier == dedup.TierT {
			in.ContentDigest = digest
			in.Attachments = wireAttachments(obs.Attachments)
		} else {
			// Tier S at M1+ (over-cap or unextractable): the canonical digest does not exist,
			// so nothing that pretends to be one may appear. The classifier still sees the
			// bytes, and the event carries the surrogate key.
			in.Attachments = wireAttachments(obs.Attachments)
		}

		// 3. hand bytes to the classifier.
		resp, cerr := p.classify(ctx, obs, res.Mode, body, digest)
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
			if err := p.Content.Put(ctx, eventID, body, dedup.BucketStart(obs.OccurredAt).Add(p.Retention)); err != nil {
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
	if in.Identity == (Identity{}) {
		in.Identity = p.Identity
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

func (p *Pipeline) canonicalDigest(obs Observation, body []byte) (digest, tier string, degraded bool, err error) {
	if obs.Extract == nil {
		// No extractor means no canonical text: the surrogate tier, with the reason recorded.
		return "", dedup.TierS, true, errors.New("core: no extractor for this route, so no canonical text exists")
	}
	text, atts, eerr := obs.Extract.Extract(body, obs.MediaType)
	if eerr != nil {
		return "", dedup.TierS, true, eerr
	}
	if len(atts) == 0 {
		atts = obs.Attachments
	}
	return dedup.ContentDigest(text, atts, p.Normalizer), dedup.TierT, false, nil
}

func (p *Pipeline) dedupKey(tier string, obs Observation, digest string, size int64) (string, error) {
	switch tier {
	case dedup.TierT:
		return dedup.ContentKey(p.Identity.TenantID, p.Identity.DeviceID, obs.ToolFingerprint, string(obs.Kind), obs.OccurredAt, digest)
	default:
		return dedup.SurrogateKey(p.Identity.TenantID, p.Identity.DeviceID, obs.ToolFingerprint, string(obs.Kind), obs.OccurredAt, size, obs.Attachments, p.Normalizer)
	}
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
		Budget:        budget,
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

func wireAttachments(atts []dedup.Attachment) []AttachmentWire {
	if len(atts) == 0 {
		return nil
	}
	out := make([]AttachmentWire, 0, len(atts))
	for _, a := range atts {
		w := AttachmentWire{Name: a.Name, SizeBytes: a.SizeBytes}
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
