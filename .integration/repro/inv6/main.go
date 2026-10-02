// .integration/repro/inv6 - independent INV-6 probe: drive capture-core with collection paths that
// fail, and show what the coverage row is built from.
//
// INV-6: "a dead collection path reports degraded/absent, never zero" - i.e. a path that is not
// working must never surface as healthy-with-zero-counters. This program uses only exported API
// of device/capture-core.
//
// Run: cd .integration/repro/inv6 && go run .
// Exit: 0 when every assertion holds, 1 otherwise (each failure is printed).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

var failures []string

func check(condition bool, format string, args ...any) {
	if !condition {
		failures = append(failures, fmt.Sprintf(format, args...))
		fmt.Printf("  FAIL  %s\n", fmt.Sprintf(format, args...))
		return
	}
	fmt.Printf("  ok    %s\n", fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------------------------
// A provider whose Start fails but whose Health() claims healthy: the registry must override the
// claim, because a provider cannot report success it did not observe (core/health.go package doc).
// ---------------------------------------------------------------------------------------------

type fakeProvider struct {
	route      protocol.Route
	startErr   error
	panicStart bool
	claim      protocol.CollectorState // what Health() says, regardless of the truth
	started    bool
}

func (p *fakeProvider) Name() protocol.Route { return p.route }

func (p *fakeProvider) Start(context.Context) error {
	if p.panicStart {
		panic("provider exploded during Start")
	}
	if p.startErr != nil {
		return p.startErr
	}
	p.started = true
	return nil
}

func (p *fakeProvider) Stop(context.Context) error { return nil }

func (p *fakeProvider) Health() core.Health {
	c := core.NewCounterSet(time.Unix(1_700_000_000, 0))
	switch p.claim {
	case protocol.StateHealthy:
		return core.Healthy(protocol.DetailNone, time.Unix(1_700_000_000, 0), time.Unix(1_700_000_100, 0), c)
	case protocol.StateDegraded:
		return core.Degraded(protocol.DetailUpstreamUnreachable, time.Unix(1_700_000_000, 0), time.Time{}, c)
	default:
		return core.Absent(protocol.DetailNone, time.Unix(1_700_000_000, 0), c)
	}
}

func (p *fakeProvider) ApplyPolicy(policy.Bundle) error { return nil }

func registrySection() {
	fmt.Println("INV-6a: provider registry, three paths - one healthy, one refusing to start, one panicking")
	clock := func() time.Time { return time.Unix(1_700_000_100, 0) }
	r := core.NewRegistry(clock, nil)

	healthy := &fakeProvider{route: protocol.RouteProxyTLS, claim: protocol.StateHealthy}
	refuses := &fakeProvider{route: protocol.RouteCLIShim, startErr: errors.New("listen: port already in use"), claim: protocol.StateHealthy}
	panics := &fakeProvider{route: protocol.RouteProcDetect, panicStart: true, claim: protocol.StateHealthy}
	for _, p := range []core.Provider{healthy, refuses, panics} {
		if err := r.Add(p); err != nil {
			check(false, "Add(%s): %v", p.Name(), err)
		}
	}

	results := r.StartAll(context.Background())
	for _, res := range results {
		fmt.Printf("  start %-16s err=%v panicked=%q\n", res.Route, res.Err, res.Panicked)
	}

	rows := map[protocol.Route]core.Health{}
	for _, route := range r.Routes() {
		h, ok := r.HealthFor(route)
		if !ok {
			check(false, "no health row for %s", route)
			continue
		}
		rows[route] = h
		fmt.Printf("  row   %-16s state=%-8s detail=%-20q last_success=%v counters=%v\n",
			route, h.State, h.Detail, h.LastSuccess.IsZero(), h.Counters)
	}

	check(rows[protocol.RouteProxyTLS].State == protocol.StateHealthy,
		"the provider that started is healthy (%s)", rows[protocol.RouteProxyTLS].State)
	check(rows[protocol.RouteCLIShim].State != protocol.StateHealthy,
		"a provider that refused to start is NEVER healthy, although its own Health() claims healthy (row says %q)",
		rows[protocol.RouteCLIShim].State)
	check(rows[protocol.RouteCLIShim].State == protocol.StateAbsent,
		"the refusing provider's row is absent, not degraded (%q)", rows[protocol.RouteCLIShim].State)
	check(rows[protocol.RouteProcDetect].State == protocol.StateAbsent,
		"a panicking Start degrades only its own row, and that row is absent (%q)", rows[protocol.RouteProcDetect].State)
	check(len(results) == 3 && results[1].Err != nil && results[2].Panicked != "",
		"every provider reports a start result (err for the refusal, panic text for the panic)")

	for route, h := range rows {
		check(len(h.Counters) == len(protocol.AllCounters),
			"%s carries all %d counters (a missing counter must not read as zero)", route, len(protocol.AllCounters))
		check(h.Validate() == nil, "%s row is inside the closed vocabularies", route)
	}
}

// ---------------------------------------------------------------------------------------------
// The pipeline: a classifier that cannot answer. The coverage row for that route is built from
// these counters and from LastSuccess, so both are checked.
// ---------------------------------------------------------------------------------------------

type memorySink struct{ entries []protocol.Entry }

func (s *memorySink) Append(e protocol.Entry) (protocol.Entry, error) {
	e.Seq = uint64(len(s.entries) + 1)
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *memorySink) Stats() protocol.SpoolStats { return protocol.SpoolStats{Depth: len(s.entries)} }

type deadClassifier struct{ calls int }

func (c *deadClassifier) Classify(context.Context, protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	c.calls++
	return protocol.ClassifyResponse{}, errors.New("classifier host is unreachable")
}

type fixedReader struct{ body []byte }

func (r fixedReader) Read(context.Context) ([]byte, error) { return r.body, nil }

func pipelineSection() {
	fmt.Println()
	fmt.Println("INV-6b: pipeline with a classifier that fails, at M1 (content is read and must be classified)")

	sink := &memorySink{}
	classifier := &deadClassifier{}
	clock := func() time.Time { return time.Unix(1_700_000_100, 0) }
	p, err := core.NewPipeline(sink, clock, func() string { return "11111111-2222-4333-8444-555555555555" })
	if err != nil {
		check(false, "NewPipeline: %v", err)
		return
	}
	p.Identity = core.Identity{TenantID: "tenant-1", DeviceID: "device-1", UserRef: "user-1"}
	bundle := &policy.Bundle{
		Version:       "b2",
		EffectiveAt:   time.Unix(1_700_000_000, 0),
		Actor:         "tenant-admin@example.invalid",
		TenantDefault: protocol.ModeM3,
		ToolModes:     map[string]protocol.CollectionMode{"tool": protocol.ModeM1},
		Classifier:    policy.ClassifierRelease{ReleaseID: "rel-1", State: policy.ReleaseEnforcing},
	}
	p.Bundles = func() *policy.Bundle { return bundle }
	p.Normalizer = dedup.IdentityNFC{}
	p.Classifier = classifier

	body := []byte(`{"messages":[{"role":"user","content":"Summarise the attached contract."}]}`)
	out, err := p.Process(context.Background(), core.Observation{
		Route:           protocol.RouteProxyTLS,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: "tool",
		OccurredAt:      time.Unix(1_700_000_000, 0),
		SizeBytes:       int64(len(body)),
		Content:         fixedReader{body: body},
		Decision:        &protocol.Decision{RuleID: "R-1", Action: protocol.ActionLogged, DecidedLocally: true},
		Extract: core.ExtractorFunc(func([]byte, string) (string, []dedup.Attachment, error) {
			return "Summarise the attached contract.", nil, nil
		}),
	})
	fmt.Printf("  outcome: emitted=%v degraded=%v reason=%q mode=%q err=%v\n", out.Emitted, out.Degraded, out.Reason, out.Mode, err)
	fmt.Printf("  classifier calls=%d, spooled entries=%d\n", classifier.calls, len(sink.entries))

	counters := p.Counters(protocol.RouteProxyTLS).Cumulative()
	fmt.Printf("  counters: %v\n", counters)
	lastSuccess := p.LastSuccess(protocol.RouteProxyTLS)
	fmt.Printf("  last_success zero=%v\n", lastSuccess.IsZero())

	check(classifier.calls == 1, "the classifier was actually driven once")
	check(counters[protocol.CounterObserved] >= 1, "the route counted the observation it saw (observed=%d)", counters[protocol.CounterObserved])
	trouble := counters[protocol.CounterErrors] + counters[protocol.CounterDropped]
	check(trouble >= 1, "the failure is counted (errors+dropped=%d), so the row cannot be all zeros", trouble)
	if out.Degraded && !lastSuccess.IsZero() {
		// Recorded, not asserted as a failure: pipeline.go:498 calls MarkSuccess whenever the
		// entry was spooled, degraded or not. It is only a risk if a provider derives "healthy"
		// from LastSuccess alone (pipeline.go:228-229 documents it as the basis for health).
		fmt.Println("  NOTE  a degraded observation still set LastSuccess; a row that derives healthy from")
		fmt.Println("        LastSuccess alone would be wrong (see REPORT.md, INV-6 observation).")
	}
	check(out.Degraded || !out.Emitted || err != nil,
		"the failure is never a silent success (degraded=%v emitted=%v err=%v)", out.Degraded, out.Emitted, err)

	if len(sink.entries) > 0 {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(sink.entries[len(sink.entries)-1].Payload, &env); err != nil {
			check(false, "the spooled envelope is not JSON: %v", err)
		} else {
			confidence := string(env["confidence"])
			_, hasLabels := env["labels"]
			fmt.Printf("  spooled envelope: confidence=%s labels_present=%v\n", confidence, hasLabels)
			check(confidence == `"degraded"` || confidence == `""`,
				"an envelope emitted without a working classifier says degraded, never a confident band (confidence=%s)", confidence)
			check(!hasLabels || confidence == `"degraded"`, "no labels are claimed from a classifier that did not answer")
		}
	} else {
		check(!out.Emitted, "nothing was spooled and the outcome says so rather than claiming an emission")
	}
}

func main() {
	registrySection()
	pipelineSection()
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("INV-6: %d assertion(s) FAILED\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("INV-6: every assertion held")
}
