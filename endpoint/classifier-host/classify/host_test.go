package classify_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/classifier-host/docparse"
	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/model"
	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
	"github.com/shadow-ai-capture/device/protocol"
)

const cardBody = "Please charge card 4111 1111 1111 1111 for the invoice."

// fakeParser is the §10 child as the host sees it, without a process: the host-level tests are
// about what the pipeline does with each parser outcome, and parser/isolation's own tests run the
// real child.
type fakeParser struct {
	available bool
	allow     bool
	result    docparse.Result
	format    string
}

func (f *fakeParser) Available() bool { return f.available }
func (f *fakeParser) Parse(_ context.Context, mediaType string, _ []byte) docparse.Result {
	f.format = mediaType
	return f.result
}
func (f *fakeParser) AllowFormat(string) bool   { return f.allow }
func (f *fakeParser) RecordFormat(string, bool) {}

func req(mode protocol.CollectionMode, mediaType, body string) protocol.ClassifyRequest {
	return protocol.ClassifyRequest{Content: []byte(body), Mode: mode, MediaType: mediaType, BudgetMS: 5000}
}

func anyFailed(resp protocol.ClassifyResponse) bool {
	for _, s := range resp.Stages {
		if s.Failed || s.Truncated {
			return true
		}
	}
	return false
}

func stageNamed(resp protocol.ClassifyResponse, name string) (protocol.StageResult, bool) {
	for _, s := range resp.Stages {
		if s.Stage == name {
			return s, true
		}
	}
	return protocol.StageResult{}, false
}

// TestDegradedIsEmittedExactlyFor enumerates §9.7's "emitted when" column and asserts the exact
// shape of a degraded answer: confidence degraded, **no labels**, and the stage that did not
// complete named with a detail from the closed vocabulary.
func TestDegradedIsEmittedExactlyFor(t *testing.T) {
	badRules := `{"version":"bad","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"` +
		strings.Repeat("a", 600) + `"}]}]}`

	cases := []struct {
		name   string
		stage  string
		detail protocol.Detail
		build  func(t *testing.T) (*classify.Host, protocol.ClassifyRequest)
		extra  func(t *testing.T, v classify.Verdict)
	}{
		{
			name: "stage skipped: whole pipeline budget exhausted", stage: classify.StageRules,
			detail: protocol.DetailBudgetExhausted,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Budget = classify.Budget{Normalise: time.Second, Rules: time.Nanosecond,
						Validators: time.Second, Model: time.Second, Total: time.Millisecond}
				})
				return h, req(protocol.ModeM1, "text/plain", cardBody)
			},
		},
		{
			name: "stage skipped: rules budget exhausted", stage: classify.StageRules, detail: protocol.DetailBudgetExhausted,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Budget = classify.Budget{Normalise: time.Second, Rules: time.Nanosecond,
						Validators: time.Second, Model: time.Second, Total: time.Second}
				})
				return h, req(protocol.ModeM1, "text/plain", cardBody)
			},
		},
		{
			name: "stage skipped: validators budget exhausted", stage: classify.StageValidators, detail: protocol.DetailBudgetExhausted,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Budget = classify.Budget{Normalise: time.Second, Rules: time.Second,
						Validators: time.Nanosecond, Model: time.Second, Total: time.Second}
				})
				return h, req(protocol.ModeM1, "text/plain", cardBody)
			},
		},
		{
			name: "stage skipped: model budget exhausted", stage: classify.StageModel, detail: protocol.DetailBudgetExhausted,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Budget = classify.Budget{Normalise: time.Second, Rules: time.Second,
						Validators: time.Second, Model: time.Nanosecond, Total: time.Second}
				})
				return h, req(protocol.ModeM1, "text/plain", "the icd-10 code was recorded by the physician")
			},
		},
		{
			name: "model artefact missing", stage: classify.StageModel, detail: protocol.DetailModelUnavailable,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{NoModel: true})
				return testrig.Host(t, store), req(protocol.ModeM1, "text/plain", cardBody)
			},
		},
		{
			name: "normalisation truncated what a rule could see", stage: classify.StageNormalise, detail: protocol.DetailNormaliseTruncated,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Limits = norm.Limits{MaxInputBytes: 1 << 16, MaxTextBytes: 64, MaxJSONTokens: 100, MaxDepth: 8}
				})
				return h, req(protocol.ModeM1, "text/plain", strings.Repeat("filler ", 40)+cardBody)
			},
		},
		{
			name: "content the provider handed over that cannot be processed: over cap", stage: classify.StageNormalise, detail: protocol.DetailContentOverCap,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Limits = norm.Limits{MaxInputBytes: 16, MaxTextBytes: 16, MaxJSONTokens: 8, MaxDepth: 4}
				})
				return h, req(protocol.ModeM1, "text/plain", strings.Repeat("x", 64))
			},
		},
		{
			name: "content the provider handed over that cannot be processed: undecodable", stage: classify.StageNormalise, detail: protocol.DetailUndecodableContent,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store)
				return h, protocol.ClassifyRequest{Content: []byte("a\xffb"), Mode: protocol.ModeM1, MediaType: "text/plain", BudgetMS: 5000}
			},
		},
		{
			name: "content the provider handed over that cannot be processed: no bytes at all", stage: classify.StageNormalise, detail: protocol.DetailContentUnprocessable,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				return testrig.Host(t, store), req(protocol.ModeM1, "text/plain", "")
			},
		},
		{
			name: "the document parser failed", stage: classify.StageParse, detail: protocol.DetailParserFailed,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Parser = &fakeParser{available: true, allow: true, result: docparse.Result{
						Cause: docparse.CauseUnsupported, Detail: protocol.DetailParserFailed, Err: "no parser for that format"}}
				})
				return h, req(protocol.ModeM1, "application/pdf", "%PDF-1.7")
			},
		},
		{
			name: "the document parser timed out", stage: classify.StageParse, detail: protocol.DetailParserTimeout,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Parser = &fakeParser{available: true, allow: true, result: docparse.Result{
						Cause: docparse.CauseTimeout, Detail: protocol.DetailParserTimeout, Err: "killed after 250ms"}}
				})
				return h, req(protocol.ModeM1, "application/pdf", "%PDF-1.7")
			},
		},
		{
			name: "the document parser was killed on its memory cap", stage: classify.StageParse, detail: protocol.DetailParserMemory,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Parser = &fakeParser{available: true, allow: true, result: docparse.Result{
						Cause: docparse.CauseMemory, Detail: protocol.DetailParserMemory, PeakBytes: 96 << 20, Err: "over the residency cap"}}
				})
				return h, req(protocol.ModeM1, "application/pdf", "%PDF-1.7")
			},
		},
		{
			name: "the document parser crashed", stage: classify.StageParse, detail: protocol.DetailParserCrash,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Parser = &fakeParser{available: true, allow: true, result: docparse.Result{
						Cause: docparse.CauseCrash, Detail: protocol.DetailParserCrash, Err: "exit status 3"}}
				})
				return h, req(protocol.ModeM1, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "PK\x03\x04")
			},
		},
		{
			name: "the parser output cap truncated the result", stage: classify.StageParse, detail: protocol.DetailParserOutputCap,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Parser = &fakeParser{available: true, allow: true, result: docparse.Result{
						Cause: docparse.CauseOK, Truncated: true, Text: "some text"}}
				})
				return h, req(protocol.ModeM1, "application/zip", "PK\x03\x04")
			},
		},
		{
			name: "the per-format circuit breaker disabled parsing", stage: classify.StageParse, detail: protocol.DetailParserFailed,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				h := testrig.Host(t, store, func(o *classify.Options) {
					o.Parser = &fakeParser{available: true, allow: false}
				})
				return h, req(protocol.ModeM1, "application/zip", "PK\x03\x04")
			},
		},
		{
			name: "a document that this target cannot parse at all", stage: classify.StageParse, detail: protocol.DetailParserFailed,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				return testrig.Host(t, store), req(protocol.ModeM1, "application/pdf", "%PDF-1.7")
			},
		},
		{
			name: "a release failed to load and the retained release classified", stage: classify.StageRelease, detail: protocol.DetailReleaseLoadFailed,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				priv, pub := testrig.Key(t)
				trust := release.NewTrust(pub)
				store := release.NewStore()
				good := filepath.Join(t.TempDir(), "v1")
				testrig.WriteRelease(t, good, priv, testrig.ReleaseOptions{Version: "v1", State: release.StateShadow})
				if _, err := store.Apply(good, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
					t.Fatal(err)
				}
				bad := filepath.Join(t.TempDir(), "v2")
				testrig.WriteRelease(t, bad, priv, testrig.ReleaseOptions{Version: "v2", State: release.StateShadow, Rules: []byte(badRules)})
				if _, err := store.Apply(bad, trust, rules.DefaultCaps(), model.DefaultCaps()); err == nil {
					t.Fatal("the bad release loaded")
				}
				h := testrig.Host(t, store)
				return h, req(protocol.ModeM1, "text/plain", cardBody)
			},
			extra: func(t *testing.T, v classify.Verdict) {
				if len(v.PartialLabels) == 0 {
					t.Error("§9.4's rules-only labels from the retained release were not recorded")
				}
			},
		},
		{
			name: "no release is loaded at all", stage: classify.StageRelease, detail: protocol.DetailReleaseLoadFailed,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				return testrig.Host(t, release.NewStore()), req(protocol.ModeM1, "text/plain", cardBody)
			},
		},
		{
			name: "the caller tried to select a release", stage: classify.StageRelease, detail: protocol.DetailReleaseLoadFailed,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				r := req(protocol.ModeM1, "text/plain", cardBody)
				r.ReleaseID = "some-other-release"
				return testrig.Host(t, store), r
			},
		},
		{
			name: "content at M0, where reading content is forbidden", stage: classify.StageMode, detail: protocol.DetailModeViolation,
			build: func(t *testing.T) (*classify.Host, protocol.ClassifyRequest) {
				store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
				return testrig.Host(t, store), req(protocol.ModeM0, "text/plain", cardBody)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, r := tc.build(t)
			v := h.Classify(context.Background(), r)
			resp := v.Response

			if err := resp.Validate(); err != nil {
				t.Fatalf("a degraded response violates protocol.ClassifyResponse.Validate: %v (%+v)", err, resp)
			}
			if resp.Confidence != protocol.ConfidenceDegraded {
				t.Fatalf("confidence is %q, want degraded", resp.Confidence)
			}
			if len(resp.Labels) != 0 {
				t.Fatalf("a degraded response carried %d labels: %+v", len(resp.Labels), resp.Labels)
			}
			if !anyFailed(resp) {
				t.Fatal("a degraded response names no failed or truncated stage, so the cause is unattributable")
			}
			st, ok := stageNamed(resp, tc.stage)
			if !ok {
				t.Fatalf("the %s stage is not recorded at all: %+v", tc.stage, resp.Stages)
			}
			if !st.Failed && !st.Truncated {
				t.Errorf("the %s stage is recorded as neither failed nor truncated: %+v", tc.stage, st)
			}
			if st.Detail != tc.detail {
				t.Errorf("%s stage detail is %q, want %q", tc.stage, st.Detail, tc.detail)
			}
			if !st.Detail.Valid() {
				t.Errorf("detail %q is outside protocol's closed vocabulary", st.Detail)
			}
			if v.Action != protocol.ActionLogged {
				t.Errorf("a degraded classification produced action %q; a classification that did not complete may not act", v.Action)
			}
			if !v.EnforcementSuppressed {
				t.Error("enforcement was not marked suppressed on a degraded classification")
			}
			if tc.extra != nil {
				tc.extra(t, v)
			}
		})
	}
}

// TestCompletedClassificationsAreNotDegraded is §9.7's "not emitted when" column.
func TestCompletedClassificationsAreNotDegraded(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	cases := []struct {
		name      string
		host      *classify.Host
		req       protocol.ClassifyRequest
		wantClass string
		wantConf  protocol.Confidence
	}{
		{
			name: "the classifier ran and found nothing", host: testrig.Host(t, store),
			req:      req(protocol.ModeM1, "text/plain", "The weather in Lisbon is mild in October."),
			wantConf: protocol.ConfidenceHigh,
		},
		{
			name: "a checksum-confirmed label", host: testrig.Host(t, store),
			req: req(protocol.ModeM1, "text/plain", cardBody), wantClass: "payment_card", wantConf: protocol.ConfidenceHigh,
		},
		{
			name: "the model ran and returned a low score", host: testrig.Host(t, store),
			req:       req(protocol.ModeM1, "text/plain", "the icd-10 code was recorded after the physician reviewed the dosage"),
			wantClass: "health", wantConf: protocol.ConfidenceLow,
		},
		{
			name: "the payload was large but fully processed", host: testrig.Host(t, store),
			req:       req(protocol.ModeM1, "text/plain", strings.Repeat("filler ", 2000)+cardBody),
			wantClass: "payment_card", wantConf: protocol.ConfidenceHigh,
		},
		{
			name: "a document was parsed and contained no matching content", host: testrig.Host(t, store, func(o *classify.Options) {
				o.Parser = &fakeParser{available: true, allow: true, result: docparse.Result{Cause: docparse.CauseOK, Text: "nothing sensitive here"}}
			}),
			req:      req(protocol.ModeM1, "application/zip", "PK\x03\x04"),
			wantConf: protocol.ConfidenceHigh,
		},
		{
			name: "a document was parsed and did contain matching content", host: testrig.Host(t, store, func(o *classify.Options) {
				o.Parser = &fakeParser{available: true, allow: true, result: docparse.Result{Cause: docparse.CauseOK, Text: cardBody}}
			}),
			req: req(protocol.ModeM1, "application/zip", "PK\x03\x04"), wantClass: "payment_card", wantConf: protocol.ConfidenceHigh,
		},
		{
			name: "the body was decoded and processed", host: testrig.Host(t, store),
			req: protocol.ClassifyRequest{Content: []byte{0xFF, 0xFE, 'c', 0, 'a', 0, 'r', 0, 'd', 0, ' ', 0, '4', 0, '1', 0, '1', 0, '1', 0, ' ', 0, '1', 0, '1', 0, '1', 0, '1', 0, ' ', 0, '1', 0, '1', 0, '1', 0, '1', 0, ' ', 0, '1', 0, '1', 0, '1', 0, '1', 0},
				Mode: protocol.ModeM1, MediaType: "text/plain", BudgetMS: 5000},
			wantClass: "payment_card", wantConf: protocol.ConfidenceHigh,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.host.Classify(context.Background(), tc.req)
			resp := v.Response
			if err := resp.Validate(); err != nil {
				t.Fatalf("the response violates the protocol contract: %v", err)
			}
			if resp.Confidence == protocol.ConfidenceDegraded {
				t.Fatalf("§9.7 says degraded is NOT emitted here, but it was: stages=%+v", resp.Stages)
			}
			if anyFailed(resp) {
				t.Fatalf("a confident response has a failed or truncated stage: %+v", resp.Stages)
			}
			for _, st := range resp.Stages {
				if st.Detail != protocol.DetailNone {
					t.Errorf("stage %q carries detail %q on a completed classification", st.Stage, st.Detail)
				}
				if !st.Detail.Valid() {
					t.Errorf("stage %q carries detail %q outside the closed vocabulary", st.Stage, st.Detail)
				}
			}
			if tc.wantClass != "" {
				found := false
				for _, l := range resp.Labels {
					if l.Class == tc.wantClass {
						found = true
					}
				}
				if !found {
					t.Errorf("expected class %q, got %+v", tc.wantClass, resp.Labels)
				}
			}
			if tc.wantConf != "" && resp.Confidence != tc.wantConf {
				t.Errorf("confidence is %q, want %q (labels %+v)", resp.Confidence, tc.wantConf, resp.Labels)
			}
		})
	}
}

// TestEmptyLabelSetIsALegitimateResult is the defect that was fixed in device/protocol: "an event
// with no labels is a legitimate output meaning the classifier ran and found nothing, which is
// materially different from confidence: degraded" (§9.2).
func TestEmptyLabelSetIsALegitimateResult(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	h := testrig.Host(t, store)
	v := h.Classify(context.Background(), req(protocol.ModeM1, "text/plain", "nothing here"))
	if len(v.Response.Labels) != 0 {
		t.Fatalf("expected no labels, got %+v", v.Response.Labels)
	}
	if v.Response.Confidence != protocol.ConfidenceHigh {
		t.Fatalf("confidence is %q, want high", v.Response.Confidence)
	}
	if err := v.Response.Validate(); err != nil {
		t.Fatalf("an empty label set with confidence high must validate: %v", err)
	}
}

// TestReleaseStatesGovernEnforcement is §9.6's table.
func TestReleaseStatesGovernEnforcement(t *testing.T) {
	t.Run("shadow records and never enforces", func(t *testing.T) {
		store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{Version: "v1"})
		v := testrig.Host(t, store).Classify(context.Background(), req(protocol.ModeM2, "text/plain", cardBody))
		if len(v.Response.Labels) == 0 {
			t.Fatal("a shadow release must still classify")
		}
		if !v.Shadowed || v.Action != protocol.ActionLogged {
			t.Errorf("shadow verdict: shadowed=%v action=%q", v.Shadowed, v.Action)
		}
		if !v.EnforcementSuppressed || v.SuppressionCause != "shadow" {
			t.Errorf("suppression: %v %q", v.EnforcementSuppressed, v.SuppressionCause)
		}
		if v.Response.Excerpt == nil {
			t.Error("an M2 shadow release must still produce its minimised excerpt")
		}
	})

	t.Run("enforcing acts on a high-confidence label", func(t *testing.T) {
		store, _, _ := testrig.Store(t, release.StateEnforcing, testrig.ReleaseOptions{Version: "v1"})
		v := testrig.Host(t, store).Classify(context.Background(), req(protocol.ModeM1, "text/plain", cardBody))
		if v.Action != protocol.ActionBlocked {
			t.Errorf("action is %q; the default policy blocks a 0.9 payment_card label", v.Action)
		}
		if v.RuleID != "PAYMENT_CARD_PAN" || v.ActionClass != "payment_card" {
			t.Errorf("the decision is not attributable to the rule that produced it: %+v", v)
		}
		if !v.DecidedLocally {
			t.Error("§7.4 requires decided_locally on a device-local decision")
		}
	})

	t.Run("rolled_back classifies at the previous version and stops enforcing", func(t *testing.T) {
		priv, pub := testrig.Key(t)
		trust := release.NewTrust(pub)
		store := release.NewStore()
		v1 := filepath.Join(t.TempDir(), "v1")
		testrig.WriteRelease(t, v1, priv, testrig.ReleaseOptions{Version: "v1", State: release.StateEnforcing})
		if _, err := store.Apply(v1, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
			t.Fatal(err)
		}
		v2 := filepath.Join(t.TempDir(), "v2")
		testrig.WriteRelease(t, v2, priv, testrig.ReleaseOptions{Version: "v2", State: release.StateEnforcing})
		if _, err := store.Apply(v2, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
			t.Fatal(err)
		}
		v3 := filepath.Join(t.TempDir(), "v3")
		testrig.WriteRelease(t, v3, priv, testrig.ReleaseOptions{Version: "v3", State: release.StateRolledBack, Previous: "v1"})
		if _, err := store.Apply(v3, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
			t.Fatal(err)
		}
		v := testrig.Host(t, store).Classify(context.Background(), req(protocol.ModeM1, "text/plain", cardBody))
		if v.Release != "v1" {
			t.Errorf("rolled_back should classify at the previous version, got %q", v.Release)
		}
		if v.Response.ClassifierVersion != "v1" {
			t.Errorf("classifier_version is %q, want the previous version", v.Response.ClassifierVersion)
		}
		if v.Action != protocol.ActionLogged || !v.EnforcementSuppressed {
			t.Errorf("rolled_back must stop enforcing: action=%q suppressed=%v", v.Action, v.EnforcementSuppressed)
		}
		if v.ReleaseState != release.StateRolledBack {
			t.Errorf("release state is %q", v.ReleaseState)
		}
	})

	t.Run("an enforcing release cannot act on a degraded classification", func(t *testing.T) {
		store, _, _ := testrig.Store(t, release.StateEnforcing, testrig.ReleaseOptions{})
		h := testrig.Host(t, store, func(o *classify.Options) {
			o.Limits = norm.Limits{MaxInputBytes: 8, MaxTextBytes: 8, MaxJSONTokens: 4, MaxDepth: 4}
		})
		v := h.Classify(context.Background(), req(protocol.ModeM1, "text/plain", cardBody))
		if v.Response.Confidence != protocol.ConfidenceDegraded {
			t.Fatalf("expected a degraded classification, got %q", v.Response.Confidence)
		}
		if v.Action != protocol.ActionLogged || v.SuppressionCause != "degraded" {
			t.Errorf("a failed classifier blocked something: action=%q cause=%q", v.Action, v.SuppressionCause)
		}
	})
}

// TestShadowReleaseIsEvaluatedAlongsideTheEnforcingOne is §9.6's "both states coexist".
func TestShadowReleaseIsEvaluatedAlongsideTheEnforcingOne(t *testing.T) {
	priv, pub := testrig.Key(t)
	trust := release.NewTrust(pub)
	store := release.NewStore()

	v1 := filepath.Join(t.TempDir(), "v1")
	testrig.WriteRelease(t, v1, priv, testrig.ReleaseOptions{Version: "v1", State: release.StateEnforcing})
	if _, err := store.Apply(v1, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
		t.Fatal(err)
	}
	shadowRules := `{"version":"r2","rules":[
		{"rule_id":"BANANA","class":"legal","score":1.0,"when":[{"signal":"regex","dialect":"linear","pattern":"banana"}]}
	]}`
	v2 := filepath.Join(t.TempDir(), "v2")
	testrig.WriteRelease(t, v2, priv, testrig.ReleaseOptions{Version: "v2", State: release.StateShadow, Rules: []byte(shadowRules)})
	if _, err := store.Apply(v2, trust, rules.DefaultCaps(), model.DefaultCaps()); err != nil {
		t.Fatal(err)
	}

	v := testrig.Host(t, store).Classify(context.Background(), req(protocol.ModeM1, "text/plain", "banana "+cardBody))
	if v.Release != "v1" {
		t.Fatalf("the enforcing release must stay authoritative, got %q", v.Release)
	}
	for _, l := range v.Response.Labels {
		if l.Class == "legal" {
			t.Fatalf("the shadow release's label leaked into the enforcing verdict: %+v", v.Response.Labels)
		}
	}
	if v.Shadow == nil {
		t.Fatal("the shadow release was not evaluated")
	}
	if v.Shadow.Release != "v2" || v.Shadow.Action != protocol.ActionLogged {
		t.Errorf("shadow verdict: %+v", v.Shadow)
	}
	found := false
	for _, l := range v.Shadow.Labels {
		if l.Class == "legal" {
			found = true
		}
	}
	if !found {
		t.Errorf("the shadow release did not produce its own label: %+v", v.Shadow.Labels)
	}
}

// TestSummariesValidateForEveryCorpusShape keeps the protocol contract honest across a spread of
// inputs, including ones the tests above do not use.
func TestEveryResponseSatisfiesTheProtocolContract(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateEnforcing, testrig.ReleaseOptions{})
	h := testrig.Host(t, store)
	reqs := []protocol.ClassifyRequest{
		req(protocol.ModeM1, "text/plain", cardBody),
		req(protocol.ModeM2, "text/plain", cardBody),
		req(protocol.ModeM3, "text/plain", cardBody),
		req(protocol.ModeM1, "text/plain", ""),
		req(protocol.ModeM0, "text/plain", cardBody),
		req(protocol.ModeM1, "application/json", `{"a":"b"}`),
		req(protocol.CollectionMode("nonsense"), "text/plain", cardBody),
		req(protocol.ModeM1, "text/plain", strings.Repeat("x", 3<<20)),
		{Content: nil, Mode: protocol.ModeM1, BudgetMS: 1},
	}
	for i, r := range reqs {
		v := h.Classify(context.Background(), r)
		if err := v.Response.Validate(); err != nil {
			t.Errorf("request %d: %v", i, err)
		}
		if v.Response.ClassifierVersion == "" {
			t.Errorf("request %d: no classifier_version, so the verdict is unattributable", i)
		}
	}
}

func TestMetricsReportNearestRankP95(t *testing.T) {
	m := classify.NewMetrics(100)
	for i := 1; i <= 100; i++ {
		m.Observe(classify.StageRules, time.Duration(i)*time.Millisecond)
	}
	st := m.Stats(classify.StageRules)
	if st.Count != 100 || st.P95 != 95*time.Millisecond || st.P50 != 50*time.Millisecond || st.Max != 100*time.Millisecond {
		t.Errorf("stats: %+v", st)
	}
	empty := m.Stats("never-observed")
	if empty.Count != 0 || empty.P95 != 0 {
		t.Errorf("an unobserved stage reported %+v", empty)
	}
}

func TestHandshakeRefusesMismatchesWithoutCrashing(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{Version: "v1"})
	h := testrig.Host(t, store)

	ok := h.Handshake(protocol.HandshakeRequest{CoreVersion: "core-1", ProtocolVersion: protocol.Version})
	if !ok.OK || ok.ClassifierVersion != "v1" {
		t.Errorf("a matching handshake was refused: %+v", ok)
	}
	bad := h.Handshake(protocol.HandshakeRequest{CoreVersion: "core-1", ProtocolVersion: protocol.Version + 1})
	if bad.OK || bad.Reason != protocol.DetailVersionMismatch {
		t.Errorf("a framing version mismatch must be a degraded handshake response: %+v", bad)
	}
	noCore := h.Handshake(protocol.HandshakeRequest{ProtocolVersion: protocol.Version})
	if noCore.OK || noCore.Reason != protocol.DetailVersionMismatch {
		t.Errorf("a handshake with no core version: %+v", noCore)
	}
	wrongRelease := h.Handshake(protocol.HandshakeRequest{CoreVersion: "core-1", ProtocolVersion: protocol.Version, ClassifierVersion: "v0"})
	if wrongRelease.OK || wrongRelease.Reason != protocol.DetailVersionMismatch {
		t.Errorf("§9.6's version handshake failure: %+v", wrongRelease)
	}
}

func TestDigestMismatchIsCountedAndTheHostDigestWins(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	h := testrig.Host(t, store)
	r := req(protocol.ModeM1, "text/plain", cardBody)
	r.ContentDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	v := h.Classify(context.Background(), r)
	if v.Digest == r.ContentDigest || v.Digest == "" {
		t.Errorf("the host must return the digest it computed over the normalised text, got %q", v.Digest)
	}
	if v.Response.Counters["digest_mismatch"] == 0 {
		t.Error("the caller/host digest disagreement was not counted")
	}
	if v.Response.Confidence == protocol.ConfidenceDegraded {
		t.Error("a digest disagreement is a dedup-contract defect, not a classification failure")
	}
}

// TestIdentityIsStructurallyUnreachable is §3.3: the classifier receives bytes and returns labels.
func TestIdentityIsStructurallyUnreachable(t *testing.T) {
	// 1. The type the host accepts has no identity field, and protocol's own compile-time guard
	//    fails the build if one is added.
	if err := testRigClassifyRequestHasNoIdentity(); err != nil {
		t.Error(err)
	}

	// 2. This component defines no request type of its own: a local copy would be the way identity
	//    creeps back in.
	src := packageSource(t)
	for _, forbidden := range []string{"type ClassifyRequest", "type Request struct", "type ClassifyInput"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("package classify declares %q; it must consume protocol.ClassifyRequest, not define its own", forbidden)
		}
	}

	// 3. The verdict the host emits carries nothing that could identify a tool, a user or a
	//    destination.
	store, _, _ := testrig.Store(t, release.StateEnforcing, testrig.ReleaseOptions{})
	v := testrig.Host(t, store).Classify(context.Background(), req(protocol.ModeM2, "text/plain", cardBody))
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, name := range protocol.IdentityFieldNames {
		if _, present := decoded[name]; present {
			t.Errorf("the verdict carries an identity field %q", name)
		}
		if strings.Contains(string(blob), `"`+name+`"`) {
			t.Errorf("the serialised verdict mentions identity field %q somewhere in its structure", name)
		}
	}
}

func testRigClassifyRequestHasNoIdentity() error {
	blob, err := json.Marshal(protocol.ClassifyRequest{})
	if err != nil {
		return err
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(blob, &decoded); err != nil {
		return err
	}
	for _, name := range protocol.IdentityFieldNames {
		if _, present := decoded[name]; present {
			return os.ErrInvalid
		}
	}
	return nil
}

func TestClassifyIsSafeForConcurrentUse(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateEnforcing, testrig.ReleaseOptions{})
	h := testrig.Host(t, store)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				v := h.Classify(context.Background(), req(protocol.ModeM2, "text/plain", cardBody))
				if err := v.Response.Validate(); err != nil {
					errs <- err.Error()
					return
				}
				if len(v.Response.Labels) == 0 {
					errs <- "a concurrent classification lost its labels"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestM2ExcerptIsMinimisedAndM3HasNone is the contract's excerpt rule: required at M2, forbidden
// at M3 (where the content is held locally instead).
func TestM2ExcerptIsMinimisedAndM3HasNone(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	h := testrig.Host(t, store, func(o *classify.Options) { o.MaxExcerptChars = 8 })

	m1 := h.Classify(context.Background(), req(protocol.ModeM1, "text/plain", cardBody))
	if m1.Response.Excerpt != nil {
		t.Error("M1 must carry no excerpt")
	}
	m2 := h.Classify(context.Background(), req(protocol.ModeM2, "text/plain", cardBody))
	if m2.Response.Excerpt == nil {
		t.Fatal("M2 must carry a minimised excerpt")
	}
	if len([]rune(m2.Response.Excerpt.Text)) > 8 {
		t.Errorf("the excerpt was not bounded: %q", m2.Response.Excerpt.Text)
	}
	if m2.Response.Excerpt.Kind != protocol.ExcerptMatchSpan {
		t.Errorf("excerpt kind is %q", m2.Response.Excerpt.Kind)
	}
	m3 := h.Classify(context.Background(), req(protocol.ModeM3, "text/plain", cardBody))
	if m3.Response.Excerpt != nil {
		t.Error("the contract forbids content_excerpt at M3")
	}
}

func packageSource(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}
