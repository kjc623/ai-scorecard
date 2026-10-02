package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"sort"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
)

// Percentiles is one measured distribution, in milliseconds.
type Percentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	Max   float64 `json:"max_ms"`
}

// MeasureReport is §9.4 measured rather than asserted: the per-stage p95 of a fixed corpus on one
// target, compared with the budget column that target is supposed to meet.
type MeasureReport struct {
	Target     string `json:"target"`
	Version    string `json:"version"`
	Corpus     string `json:"corpus"`
	Cases      int    `json:"cases"`
	Iterations int    `json:"iterations"`

	BudgetMS map[string]float64     `json:"budget_ms"`
	Stages   map[string]Percentiles `json:"stages"`
	Total    Percentiles            `json:"total"`

	SumStageP95MS float64  `json:"sum_stage_p95_ms"`
	WithinBudget  bool     `json:"within_budget"`
	OverBudget    []string `json:"over_budget,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// nearestRank is the percentile definition the host's own Metrics uses, repeated here so the
// harness and the health channel cannot disagree about what p95 means.
func nearestRank(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func percentiles(samples []float64) Percentiles {
	if len(samples) == 0 {
		return Percentiles{}
	}
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	return Percentiles{Count: len(s), P50: nearestRank(s, 50), P95: nearestRank(s, 95), Max: s[len(s)-1]}
}

// runMeasure is `classifier-host measure`. The same subcommand runs natively and under the wasm
// runtime, so ADR 0016's "the 300 ms interactive budget must be measured against the real module"
// is answered by two reports from one source.
func runMeasure(args []string) int {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	releaseDir := fs.String("release", "", "signed release directory")
	pubkey := fs.String("pubkey", "", "hex ed25519 release-signing public key")
	corpusPath := fs.String("corpus", "", "corpus JSON file")
	iterations := fs.Int("iterations", 200, "corpus iterations")
	out := fs.String("out", "", "output path for the JSON report ('-' for stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *corpusPath == "" {
		return fatalf("measure: --corpus is required")
	}
	r, err := loadRig(*releaseDir, *pubkey)
	if err != nil {
		return fatalf("measure: %v", err)
	}
	c, err := LoadCorpus(*corpusPath)
	if err != nil {
		return fatalf("measure: %v", err)
	}

	budget := targetBudget()
	samples := map[string][]float64{}
	var totals []float64
	ctx := context.Background()
	for i := 0; i < *iterations; i++ {
		for _, cs := range c.Cases {
			req, err := cs.Request(c.BudgetMS)
			if err != nil {
				return fatalf("measure: %v", err)
			}
			t0 := time.Now()
			v := r.host.Classify(ctx, req)
			totals = append(totals, ms(time.Since(t0)))
			for _, st := range v.Response.Stages {
				samples[st.Stage] = append(samples[st.Stage], ms(st.Duration))
			}
		}
	}

	rep := MeasureReport{
		Target:     runtime.GOOS + "/" + runtime.GOARCH,
		Version:    Version,
		Corpus:     *corpusPath,
		Cases:      len(c.Cases),
		Iterations: *iterations,
		BudgetMS: map[string]float64{
			classify.StageNormalise:  ms(budget.Normalise),
			classify.StageRules:      ms(budget.Rules),
			classify.StageValidators: ms(budget.Validators),
			classify.StageModel:      ms(budget.Model),
			"total":                  ms(budget.Total),
		},
		Stages: map[string]Percentiles{},
		Total:  percentiles(totals),
	}
	sumP95 := 0.0
	for stage, list := range samples {
		p := percentiles(list)
		rep.Stages[stage] = p
		switch stage {
		case classify.StageNormalise, classify.StageRules, classify.StageValidators, classify.StageModel:
			sumP95 += p.P95
			if limit := rep.BudgetMS[stage]; limit > 0 && p.P95 > limit {
				rep.OverBudget = append(rep.OverBudget, fmt.Sprintf("%s p95 %.3f ms over its %.3f ms share", stage, p.P95, limit))
			}
		}
	}
	rep.SumStageP95MS = sumP95
	if sumP95 > rep.BudgetMS["total"] {
		rep.OverBudget = append(rep.OverBudget, fmt.Sprintf("the four stage p95s sum to %.3f ms, over the %.3f ms interactive target", sumP95, rep.BudgetMS["total"]))
	}
	rep.WithinBudget = len(rep.OverBudget) == 0
	rep.Notes = append(rep.Notes,
		"p95 is nearest-rank over every stage observation of the whole run, not per corpus case",
		"stage shares are docs/01-collectors.md §9.4's columns for this target",
	)
	if runtime.GOARCH != "wasm" {
		rep.Notes = append(rep.Notes, "this measurement includes neither module load nor wasm instantiation; see reports/wasm-size.txt for the load cost")
	}
	sort.Strings(rep.OverBudget)

	if err := writeJSON(*out, rep); err != nil {
		return fatalf("measure: writing %s: %v", *out, err)
	}
	if *out == "" {
		return fatalf("measure: --out is required (the report is the deliverable)")
	}
	return 0
}
