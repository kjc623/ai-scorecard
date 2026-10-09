package merge_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/merge"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	fingerprint = "app:claude_code"
	session     = "8f2c1e4a-5b6d-4c7e-9f80-1a2b3c4d5e6f"
	canary      = "SAC-CANARY-40 check the medical records"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// submitted is one observation the pipeline received, with the text its reader held then.
type submitted struct {
	obs  core.Observation
	text string
	read bool
}

// fakePipeline resolves every prompt to mode and records what it is handed.
type fakePipeline struct {
	mode protocol.CollectionMode

	mu   sync.Mutex
	got  []submitted
	err  error
	fact []core.Fact
}

func (p *fakePipeline) ResolveMode(core.ScopeQuery) core.Resolution {
	return core.Resolution{Mode: p.mode}
}

func (p *fakePipeline) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	s := submitted{obs: obs}
	if p.mode.ReadsContent() && obs.Content != nil {
		b, err := obs.Content.Read(ctx)
		if err != nil {
			return core.Outcome{}, err
		}
		s.text, s.read = string(b), true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, s)
	return core.Outcome{Route: obs.Route, Mode: p.mode, Reason: core.ReasonEmitted, Emitted: p.err == nil}, p.err
}

func (p *fakePipeline) Record(_ context.Context, f core.Fact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fact = append(p.fact, f)
	return nil
}

func (p *fakePipeline) Identity() (core.Identity, bool) {
	return core.Identity{TenantID: "t", DeviceID: "d"}, true
}

func (p *fakePipeline) submitted() []submitted {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]submitted(nil), p.got...)
}

// textReader is a provider's prompt behind the content gate. A forbidden reader fails the test if
// it is read.
type textReader struct {
	text      string
	forbidden *testing.T
}

func (r textReader) Read(context.Context) ([]byte, error) {
	if r.forbidden != nil {
		r.forbidden.Error("the prompt was read at m0")
	}
	if r.text == "" {
		return nil, nil
	}
	return []byte(r.text), nil
}

func extract(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errors.New("no text")
	}
	return string(payload), nil, nil
}

// hookDecision is the hook's decision, told apart from the OTel side's by its rule id.
func hookDecision([]string, bool) protocol.Decision {
	return protocol.Decision{Action: protocol.ActionBlocked, RuleID: "hook.rule"}
}

func otelDecision([]string, bool) protocol.Decision {
	return protocol.Decision{Action: protocol.ActionLogged, RuleID: "otel.rule"}
}

var (
	hookPerson = &core.Person{UserRef: "u_hook"}
	otelPerson = &core.Person{UserRef: "u_otel"}
)

// hook is the hook relay's observation of text, at.
func hook(text string, at time.Time) core.Observation {
	return core.Observation{
		Route: protocol.RouteToolHook, Kind: protocol.KindPrompt, ToolFingerprint: fingerprint,
		MediaType: "text/plain", OccurredAt: at, SizeBytes: int64(len(text)),
		Enforce: hookDecision, Content: textReader{text: text}, Extract: core.ExtractorFunc(extract),
		Person: hookPerson, ClientID: session,
	}
}

// otel is the Claude Code normalizer's observation of text, at.
func otel(text string, at time.Time) core.Observation {
	return core.Observation{
		Route: protocol.RouteToolOTel, Kind: protocol.KindPrompt, ToolFingerprint: fingerprint,
		MediaType: "text/plain", OccurredAt: at, SizeBytes: int64(len(text)),
		Enforce: otelDecision, Content: textReader{text: text}, Extract: core.ExtractorFunc(extract),
		Person: otelPerson, ClientID: session,
	}
}

func process(t *testing.T, b *merge.Buffer, obs core.Observation) core.Outcome {
	t.Helper()
	out, err := b.Process(context.Background(), obs)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return out
}

// checkMerged checks one record of a pair: the hook's route, decision and person, the earlier
// occurred_at and the text.
func checkMerged(t *testing.T, s submitted, at time.Time, text string) {
	t.Helper()
	if s.obs.Route != protocol.RouteToolHook {
		t.Errorf("route %s, want tool.hook", s.obs.Route)
	}
	if s.obs.Enforce == nil || s.obs.Enforce(nil, false).RuleID != "hook.rule" {
		t.Error("the merged record does not carry the hook's decision")
	}
	if s.obs.Person != hookPerson {
		t.Errorf("person %+v, want the hook's", s.obs.Person)
	}
	if !s.obs.OccurredAt.Equal(at) {
		t.Errorf("occurred_at %s, want %s", s.obs.OccurredAt, at)
	}
	if s.text != text {
		t.Errorf("text %q, want %q", s.text, text)
	}
}

func TestHookFirstThenOTelMakesOneHookRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM1}
		b := merge.New(merge.Config{Pipeline: p})
		if out := process(t, b, hook(canary, t0)); out.Reason != merge.ReasonHeld {
			t.Fatalf("the first side was not held: %+v", out)
		}
		if len(p.submitted()) != 0 {
			t.Fatal("the first side reached the pipeline")
		}
		time.Sleep(5 * time.Second)
		out := process(t, b, otel(canary, t0.Add(-300*time.Millisecond)))
		if out.Route != protocol.RouteToolHook || !out.Emitted {
			t.Fatalf("the pair's outcome is %+v", out)
		}
		got := p.submitted()
		if len(got) != 1 {
			t.Fatalf("%d records, want 1", len(got))
		}
		checkMerged(t, got[0], t0.Add(-300*time.Millisecond), canary)
		time.Sleep(time.Minute)
		if len(p.submitted()) != 1 || b.Held() != 0 {
			t.Fatal("a merged prompt was released again")
		}
	})
}

func TestOTelFirstThenHookMakesOneHookRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM3}
		b := merge.New(merge.Config{Pipeline: p})
		process(t, b, otel(canary, t0))
		time.Sleep(9 * time.Second)
		process(t, b, hook(canary, t0.Add(time.Second)))
		got := p.submitted()
		if len(got) != 1 {
			t.Fatalf("%d records, want 1", len(got))
		}
		checkMerged(t, got[0], t0, canary)
	})
}

// A prompt only one path saw goes on alone, on its own route, once its hold ends.
func TestOnePathOnlyIsReleasedAloneAfterTheHold(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  core.Observation
	}{
		{"hook only", hook(canary, t0)},
		{"otel only", otel(canary, t0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &fakePipeline{mode: protocol.ModeM2}
				b := merge.New(merge.Config{Pipeline: p})
				process(t, b, tc.obs)
				time.Sleep(merge.HoldFor - time.Millisecond)
				synctest.Wait()
				if len(p.submitted()) != 0 {
					t.Fatal("released before the hold ended")
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				got := p.submitted()
				if len(got) != 1 {
					t.Fatalf("%d records after the hold, want 1", len(got))
				}
				s := got[0]
				if s.obs.Route != tc.obs.Route || s.obs.Person != tc.obs.Person || !s.obs.OccurredAt.Equal(t0) || s.text != canary {
					t.Fatalf("released %+v with %q, want the record unchanged", s.obs, s.text)
				}
				if s.obs.Enforce(nil, false).RuleID != tc.obs.Enforce(nil, false).RuleID {
					t.Fatal("the released record lost its own decision")
				}
				if b.Held() != 0 {
					t.Fatalf("%d still held", b.Held())
				}
			})
		})
	}
}

// Two prompts with the same text in one session pair in arrival order.
func TestSameTextTwicePairsInArrivalOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM1}
		b := merge.New(merge.Config{Pipeline: p})
		first, second := t0, t0.Add(4*time.Second)
		process(t, b, hook("yes", first))
		process(t, b, hook("yes", second))
		if b.Held() != 2 {
			t.Fatalf("two hook prompts held as %d", b.Held())
		}
		process(t, b, otel("yes", first.Add(-time.Second)))
		process(t, b, otel("yes", second.Add(-time.Second)))
		got := p.submitted()
		if len(got) != 2 {
			t.Fatalf("%d records, want 2", len(got))
		}
		checkMerged(t, got[0], first.Add(-time.Second), "yes")
		checkMerged(t, got[1], second.Add(-time.Second), "yes")
		if b.Held() != 0 {
			t.Fatalf("%d still held", b.Held())
		}
	})
}

// A prompt pairs only with the other path's record of the same tool, session and text.
func TestDifferentPromptsDoNotPair(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM1}
		b := merge.New(merge.Config{Pipeline: p})
		process(t, b, hook(canary, t0))
		process(t, b, hook(canary, t0)) // the same path twice is two prompts
		other := otel(canary, t0)
		other.ClientID = "another-session"
		process(t, b, other)
		other = otel(canary, t0)
		other.ToolFingerprint = "app:cursor"
		process(t, b, other)
		process(t, b, otel(canary+".", t0))
		if len(p.submitted()) != 0 || b.Held() != 5 {
			t.Fatalf("%d records and %d held, want none and 5", len(p.submitted()), b.Held())
		}
		time.Sleep(merge.HoldFor)
		synctest.Wait()
		if len(p.submitted()) != 5 {
			t.Fatalf("%d released, want 5", len(p.submitted()))
		}
	})
}

// At m0 nothing is read: the key is the length in bytes, and the two occurred_at values must be
// within 2 s.
func TestM0KeyIsLengthWithinTwoSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM0}
		b := merge.New(merge.Config{Pipeline: p})
		at0 := func(obs core.Observation) core.Observation {
			obs.Content = textReader{forbidden: t}
			return obs
		}
		// Same length, 2 s apart: one record.
		process(t, b, at0(hook("abcdef", t0)))
		process(t, b, at0(otel("ghijkl", t0.Add(merge.M0Window))))
		got := p.submitted()
		if len(got) != 1 {
			t.Fatalf("%d records, want 1", len(got))
		}
		checkMerged(t, got[0], t0, "")

		// Same length, more than 2 s apart, and a different length: no pair.
		process(t, b, at0(hook("abcdef", t0.Add(time.Minute))))
		process(t, b, at0(otel("abcdef", t0.Add(time.Minute+merge.M0Window+time.Millisecond))))
		process(t, b, at0(otel("abcdefg", t0.Add(time.Minute))))
		if len(p.submitted()) != 1 || b.Held() != 3 {
			t.Fatalf("%d records and %d held, want 1 and 3", len(p.submitted()), b.Held())
		}
		time.Sleep(merge.HoldFor)
		synctest.Wait()
		if len(p.submitted()) != 4 {
			t.Fatalf("%d records after the hold, want 4", len(p.submitted()))
		}
	})
}

// At m1+ a side without text (the hook's over-cap prompt, or OTel with prompt logging off) pairs on
// the length, and the record takes the text from the side that holds it.
func TestTheTextComesFromWhicheverSideHoldsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM1}
		b := merge.New(merge.Config{Pipeline: p})
		overCap := hook("", t0)
		overCap.SizeBytes = int64(len(canary))
		overCap.OverCap = true
		process(t, b, overCap)
		process(t, b, otel(canary, t0.Add(time.Second)))
		got := p.submitted()
		if len(got) != 1 {
			t.Fatalf("%d records, want 1", len(got))
		}
		checkMerged(t, got[0], t0, canary)
		if got[0].obs.OverCap {
			t.Error("the record holds OTel's whole text but says over cap")
		}

		redacted := otel("", t0.Add(time.Minute))
		redacted.SizeBytes = int64(len(canary))
		process(t, b, redacted)
		process(t, b, hook(canary, t0.Add(time.Minute)))
		got = p.submitted()
		if len(got) != 2 {
			t.Fatalf("%d records, want 2", len(got))
		}
		checkMerged(t, got[1], t0.Add(time.Minute), canary)
	})
}

// Past 1,000 held records the oldest goes on alone, without counting anything dropped.
func TestTheCapReleasesTheOldest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM1}
		b := merge.New(merge.Config{Pipeline: p})
		for i := range merge.MaxHeld {
			process(t, b, hook(fmt.Sprintf("prompt %d", i), t0))
		}
		if b.Held() != merge.MaxHeld || len(p.submitted()) != 0 {
			t.Fatalf("%d held and %d released, want %d and 0", b.Held(), len(p.submitted()), merge.MaxHeld)
		}
		process(t, b, hook("one more", t0))
		got := p.submitted()
		if b.Held() != merge.MaxHeld || len(got) != 1 || got[0].text != "prompt 0" || got[0].obs.Route != protocol.RouteToolHook {
			t.Fatalf("%d held, released %d, want the oldest alone", b.Held(), len(got))
		}
		// The next oldest still pairs; the released prompt no longer does, and its partner is held.
		process(t, b, otel("prompt 1", t0))
		process(t, b, otel("prompt 0", t0))
		got = p.submitted()
		if len(got) != 2 || b.Held() != merge.MaxHeld {
			t.Fatalf("%d records and %d held, want 2 and %d", len(got), b.Held(), merge.MaxHeld)
		}
		checkMerged(t, got[1], t0, "prompt 1")
		b.Close()
	})
}

// Close releases everything held, unmerged, and from then on nothing is held.
func TestCloseReleasesEverythingUnmerged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM1}
		b := merge.New(merge.Config{Pipeline: p})
		process(t, b, hook("first", t0))
		process(t, b, otel("second", t0))
		b.Close()
		got := p.submitted()
		if len(got) != 2 || got[0].obs.Route != protocol.RouteToolHook || got[1].obs.Route != protocol.RouteToolOTel || b.Held() != 0 {
			t.Fatalf("Close released %d, %d held", len(got), b.Held())
		}
		process(t, b, otel("first", t0))
		if len(p.submitted()) != 3 || b.Held() != 0 {
			t.Fatal("a prompt after Close was held")
		}
		time.Sleep(merge.HoldFor)
		synctest.Wait()
		if len(p.submitted()) != 3 {
			t.Fatal("a closed buffer released again")
		}
	})
}

// What is not a prompt from the two paths goes straight through, as does a prompt without a
// session id (the generic normalizer's).
func TestOtherRecordsPassStraightThrough(t *testing.T) {
	p := &fakePipeline{mode: protocol.ModeM1}
	b := merge.New(merge.Config{Pipeline: p})
	noSession := otel(canary, t0)
	noSession.ClientID = ""
	process(t, b, noSession)
	proxied := hook(canary, t0)
	proxied.Route = protocol.RouteProxyTLS
	process(t, b, proxied)
	if len(p.submitted()) != 2 || b.Held() != 0 {
		t.Fatalf("%d passed, %d held", len(p.submitted()), b.Held())
	}
	f := core.Fact{Kind: protocol.KindAgentActivity, Route: protocol.RouteToolOTel, ToolFingerprint: fingerprint}
	if err := b.Record(context.Background(), f); err != nil || len(p.fact) != 1 {
		t.Fatal("an agent_activity record did not pass through")
	}
	if id, ok := b.Identity(); !ok || id.TenantID != "t" {
		t.Fatal("Identity is not the pipeline's")
	}
}

type captureLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// Held text is cleared when it is released, and a failed release logs no text.
func TestHeldTextIsClearedAndNeverLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakePipeline{mode: protocol.ModeM3, err: errors.New("spool refused " + canary)}
		log := &captureLog{}
		b := merge.New(merge.Config{Pipeline: p, Log: log})
		// One pair, three prompts whose hold ends, and one Close releases.
		_, _ = b.Process(context.Background(), hook(canary, t0))
		_, _ = b.Process(context.Background(), otel(canary, t0))
		_, _ = b.Process(context.Background(), hook(canary+"!", t0))
		_, _ = b.Process(context.Background(), otel(canary+"?", t0))
		_, _ = b.Process(context.Background(), hook(canary+"#", t0))
		time.Sleep(merge.HoldFor)
		synctest.Wait()
		_, _ = b.Process(context.Background(), otel(canary+"?", t0))
		b.Close()
		got := p.submitted()
		if len(got) != 5 {
			t.Fatalf("%d records, want 5", len(got))
		}
		for i, s := range got {
			if !strings.HasPrefix(s.text, canary) {
				t.Fatalf("record %d carried %q", i, s.text)
			}
			if b, err := s.obs.Content.Read(context.Background()); err != nil || len(b) != 0 {
				t.Errorf("record %d's text is still held after its release", i)
			}
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		if len(log.lines) == 0 {
			t.Fatal("a failed release was not logged")
		}
		for _, l := range log.lines {
			if strings.Contains(l, "CANARY") || strings.Contains(l, "medical") {
				t.Fatalf("the log quotes the prompt: %s", l)
			}
		}
	})
}
