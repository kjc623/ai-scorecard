// Package merge makes one record of a prompt that two paths report: a tool's hook (route
// tool.hook, which carries the decision) and its OpenTelemetry export (route tool.otel). It sits in
// front of core.Pipeline with the pipeline's own methods, so a provider hands it observations as it
// would the pipeline.
//
// A prompt that arrives first is held in memory until its partner arrives or the hold expires. Held
// prompt text is never written or logged, and is zeroed when the record is released.
package merge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	// HoldFor is how long a prompt waits for its partner before it goes on alone.
	HoldFor = 10 * time.Second
	// MaxHeld bounds the prompts held at once; past it the oldest goes on alone.
	MaxHeld = 1000
	// M0Window is how far apart the two sides' occurred_at may be when no text is compared.
	M0Window = 2 * time.Second
)

// ReasonHeld is the outcome of a prompt the buffer holds for its partner.
const ReasonHeld = "held_for_merge"

// Pipeline is the part of core.Pipeline the buffer submits to and passes through.
type Pipeline interface {
	ResolveMode(q core.ScopeQuery) core.Resolution
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
	Record(ctx context.Context, f core.Fact) error
	Identity() (core.Identity, bool)
}

// Config is the buffer's wiring.
type Config struct {
	Pipeline Pipeline
	Log      core.Logger
}

// Buffer is the hold-and-match buffer. It is safe for concurrent use.
type Buffer struct {
	cfg Config

	mu     sync.Mutex
	held   []*entry // in arrival order
	closed bool
	// releasing counts the records an expiry is handing to the pipeline, so Close can wait for them.
	releasing sync.WaitGroup
}

// entry is one held prompt.
type entry struct {
	obs     core.Observation
	session string
	// text is the prompt read at a mode that reads content; hasDigest says digest is its sha256.
	text      []byte
	digest    [sha256.Size]byte
	hasDigest bool
	timer     *time.Timer
	isHeld    bool
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// New returns an empty buffer in front of cfg.Pipeline.
func New(cfg Config) *Buffer {
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	return &Buffer{cfg: cfg}
}

// Identity passes through to the pipeline.
func (b *Buffer) Identity() (core.Identity, bool) { return b.cfg.Pipeline.Identity() }

// Record passes through to the pipeline: only prompts merge.
func (b *Buffer) Record(ctx context.Context, f core.Fact) error { return b.cfg.Pipeline.Record(ctx, f) }

// Process merges a prompt on tool.hook or tool.otel with its partner from the other route, or holds
// it until the partner arrives. Anything else, a prompt without a session id, and every prompt
// after Close go straight to the pipeline.
func (b *Buffer) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	if !mergeable(obs) {
		return b.cfg.Pipeline.Process(ctx, obs)
	}
	e := &entry{obs: obs, session: obs.ClientID}
	res := b.cfg.Pipeline.ResolveMode(scope(obs))
	if res.ReadsContent() && obs.Content != nil {
		if text, err := obs.Content.Read(ctx); err == nil && len(text) > 0 {
			e.text = text
			e.digest = sha256.Sum256(text)
			e.hasDigest = true
			e.obs.Content = heldText{e}
		}
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		defer e.clear()
		return b.cfg.Pipeline.Process(ctx, e.obs)
	}
	if partner := b.takePartner(e); partner != nil {
		b.mu.Unlock()
		defer e.clear()
		defer partner.clear()
		return b.cfg.Pipeline.Process(ctx, merged(e, partner))
	}
	var evicted *entry
	if len(b.held) >= MaxHeld {
		evicted = b.held[0]
		b.remove(0)
	}
	e.isHeld = true
	e.timer = time.AfterFunc(HoldFor, func() { b.expire(e) })
	b.held = append(b.held, e)
	b.mu.Unlock()

	if evicted != nil {
		b.release(evicted)
	}
	return core.Outcome{Route: obs.Route, Mode: res.Mode, Reason: ReasonHeld}, nil
}

// Close releases every held prompt unmerged and waits for releases already under way. From then on
// every prompt goes straight to the pipeline.
func (b *Buffer) Close() {
	b.mu.Lock()
	b.closed = true
	held := b.held
	b.held = nil
	for _, e := range held {
		e.isHeld = false
		e.timer.Stop()
	}
	b.mu.Unlock()
	for _, e := range held {
		b.release(e)
	}
	b.releasing.Wait()
}

// Held is the number of prompts waiting for a partner.
func (b *Buffer) Held() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.held)
}

// mergeable reports whether an observation is a prompt one of the two paths reports with the
// session it belongs to.
func mergeable(obs core.Observation) bool {
	return obs.Kind == protocol.KindPrompt &&
		(obs.Route == protocol.RouteToolHook || obs.Route == protocol.RouteToolOTel) &&
		obs.ClientID != ""
}

// scope is the query the pipeline resolves the observation's mode with.
func scope(obs core.Observation) core.ScopeQuery {
	q := core.ScopeQuery{ToolFingerprint: obs.ToolFingerprint, Population: obs.Population}
	if obs.Person != nil {
		q.UserRef = obs.Person.UserRef
	}
	return q
}

// takePartner removes and returns the earliest held prompt e pairs with, nil when none. b.mu is
// held.
func (b *Buffer) takePartner(e *entry) *entry {
	for i, h := range b.held {
		if pairs(h, e) {
			b.remove(i)
			return h
		}
	}
	return nil
}

// remove takes held[i] out of the buffer and stops its expiry. b.mu is held.
func (b *Buffer) remove(i int) {
	h := b.held[i]
	h.isHeld = false
	h.timer.Stop()
	b.held = append(b.held[:i], b.held[i+1:]...)
}

// pairs reports whether a and b are the same prompt seen by the two paths. With both texts they
// match on its sha256; otherwise on the length in bytes, with occurred_at within M0Window.
func pairs(a, b *entry) bool {
	if a.obs.Route == b.obs.Route || a.obs.ToolFingerprint != b.obs.ToolFingerprint || a.session != b.session {
		return false
	}
	if a.hasDigest && b.hasDigest {
		return a.digest == b.digest
	}
	d := a.obs.OccurredAt.Sub(b.obs.OccurredAt)
	return a.obs.SizeBytes == b.obs.SizeBytes && d <= M0Window && d >= -M0Window
}

// merged is the one observation for a pair: the hook's route, decision and person, the earlier
// occurred_at, and the content of whichever side holds the text.
func merged(a, b *entry) core.Observation {
	hook, otel := a, b
	if hook.obs.Route != protocol.RouteToolHook {
		hook, otel = b, a
	}
	obs := hook.obs
	if otel.obs.OccurredAt.Before(obs.OccurredAt) {
		obs.OccurredAt = otel.obs.OccurredAt
		obs.MonotonicOffsetMS = otel.obs.MonotonicOffsetMS
	}
	if hook.text == nil && otel.text != nil {
		obs.Content = otel.obs.Content
		obs.Extract = otel.obs.Extract
		obs.MediaType = otel.obs.MediaType
		obs.OverCap = otel.obs.OverCap
	}
	return obs
}

// expire sends a prompt whose hold ended on alone, unless it was paired or released meanwhile.
func (b *Buffer) expire(e *entry) {
	b.mu.Lock()
	if !e.isHeld {
		b.mu.Unlock()
		return
	}
	for i, h := range b.held {
		if h == e {
			b.remove(i)
			break
		}
	}
	b.releasing.Add(1)
	b.mu.Unlock()
	defer b.releasing.Done()
	b.release(e)
}

// release sends a held prompt on alone, on its own route, and clears it.
func (b *Buffer) release(e *entry) {
	defer e.clear()
	out, err := b.cfg.Pipeline.Process(context.Background(), e.obs)
	// Before enrolment every record is refused; the pipeline counts those.
	if err != nil && !errors.Is(err, core.ErrIdentityUnresolved) {
		b.cfg.Log.Printf("merge: a held %s prompt on %s was not recorded (%s)", e.obs.ToolFingerprint, e.obs.Route, out.Reason)
	}
}

// clear zeroes the held text and drops every reference the entry kept to the prompt.
func (e *entry) clear() {
	clear(e.text)
	e.text = nil
	e.obs = core.Observation{}
}

// heldText is the held prompt behind the pipeline's content gate. Each read is a copy, so zeroing
// the held text never reaches what the pipeline kept.
type heldText struct{ e *entry }

func (h heldText) Read(context.Context) ([]byte, error) {
	if len(h.e.text) == 0 {
		return nil, nil
	}
	return bytes.Clone(h.e.text), nil
}
