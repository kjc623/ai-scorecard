// Package normalizers is the list of OTLP normalizers the service registers with the receiver, in
// the order the receiver consults them. The receiver's privacy test runs every normalizer this
// list returns.
package normalizers

import (
	"context"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/otlp/claudecode"
	"github.com/shadow-ai-capture/device/capture-core/otlp/codex"
	"github.com/shadow-ai-capture/device/capture-core/otlp/genai"
	"github.com/shadow-ai-capture/device/capture-core/policy"
)

// Pipeline is the part of core.Pipeline the normalizers use.
type Pipeline interface {
	Identity() (core.Identity, bool)
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
	Record(ctx context.Context, f core.Fact) error
}

// Deps is what the normalizers are built from.
type Deps struct {
	Pipeline Pipeline
	// Bundles returns the bundle in force, for the enforcement rules.
	Bundles func() *policy.Bundle
	// Counters is the otel_receiver collector's counter set.
	Counters *core.CounterSet
	// AppByExe names the catalog app whose executable has this base name. nil matches nothing.
	AppByExe func(base string) (appKey string, ok bool)
	Log      core.Logger
	Clock    func() time.Time
}

// Registered returns the normalizers in the order the receiver consults them.
func Registered(d Deps) []otlp.Normalizer {
	return []otlp.Normalizer{
		claudecode.New(claudecode.Config{Pipeline: d.Pipeline, Log: d.Log, Bundles: d.Bundles}),
		codex.New(codex.Config{Pipeline: d.Pipeline, Log: d.Log, Bundles: d.Bundles}),
		// The generic normalizer accepts every service.name, so it comes last and sees only what
		// no tool's normalizer accepts.
		genai.New(genai.Config{Pipeline: d.Pipeline, Counters: d.Counters, AppByExe: d.AppByExe, Bundles: d.Bundles, Clock: d.Clock}),
	}
}
