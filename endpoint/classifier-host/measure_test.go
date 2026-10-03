package classifierhost_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/release"
)

// §9.4 is a budget, not a comment, and ADR 0016 makes the measurement an acceptance criterion:
// "the 300 ms budget must be measured against the real module with the real rules corpus, and that
// measurement is part of the component's acceptance rather than a footnote".
//
// This test runs the same `measure` subcommand on both targets — natively and inside the wasm
// module under Node — and asserts:
//
//  1. every stage's p95 is within §9.4's share for that target;
//  2. the four stage p95s sum within the interactive target;
//  3. the wasm module size and its instantiate cost are published (reports/wasm-size.txt).
//
// It asserts the *budget*, not a machine-specific number: the published report is the measurement,
// and this test fails if the implementation stops meeting the ladder it claims to meet.

type latencyPercentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	Max   float64 `json:"max_ms"`
}

type latencyReport struct {
	Target        string                        `json:"target"`
	Corpus        string                        `json:"corpus"`
	Cases         int                           `json:"cases"`
	Iterations    int                           `json:"iterations"`
	BudgetMS      map[string]float64            `json:"budget_ms"`
	Stages        map[string]latencyPercentiles `json:"stages"`
	Total         latencyPercentiles            `json:"total"`
	SumStageP95MS float64                       `json:"sum_stage_p95_ms"`
	WithinBudget  bool                          `json:"within_budget"`
	OverBudget    []string                      `json:"over_budget"`
}

const (
	stageNormalise  = "normalise"
	stageRules      = "rules"
	stageValidators = "validators"
	stageModel      = "model"
)

func TestLatencyBudgetIsMeasuredOnBothTargets(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both targets and runs thousands of classifications")
	}
	goTool := requireTool(t, "go")
	nodeTool := requireNode(t)

	tmp := t.TempDir()
	priv, pub := testrig.Key(t)
	releaseDir := filepath.Join(tmp, "release")
	testrig.WriteRelease(t, releaseDir, priv, testrig.ReleaseOptions{
		Version: "measure-1",
		State:   release.StateShadow,
	})
	relDir := filepath.ToSlash(releaseDir)
	corpus := filepath.ToSlash(testrig.Testdata("corpus.json"))
	pubHex := testrig.PubHex(pub)
	iterations := "200"

	nativeBin := filepath.Join(tmp, "classifier-host"+exeSuffix())
	buildTarget(t, goTool, "", nativeBin)
	wasmBin := filepath.Join(tmp, "classifier-host.wasm")
	buildTarget(t, goTool, "js/wasm", wasmBin)
	copyShim(t, goTool, tmp)
	driver := filepath.Join(testrig.ModuleRoot(), "tools", "run-wasm.mjs")

	nativeReport := filepath.Join(tmp, "latency-native.json")
	wasmReport := filepath.Join(tmp, "latency-wasm.json")
	run(t, filepath.Dir(nativeBin), nativeBin, "measure",
		"--release", relDir, "--pubkey", pubHex, "--corpus", corpus,
		"--iterations", iterations, "--out", filepath.ToSlash(nativeReport))
	metrics := runWasm(t, nodeTool, driver, wasmBin, "measure",
		"--release", relDir, "--pubkey", pubHex, "--corpus", corpus,
		"--iterations", iterations, "--out", filepath.ToSlash(wasmReport))

	nativeLat := readLatency(t, nativeReport)
	wasmLat := readLatency(t, wasmReport)

	// Publish both reports, then assert the ladder.
	copyReport(t, "latency-native.json", nativeReport)
	copyReport(t, "latency-wasm.json", wasmReport)
	summary := latencySummary(nativeLat, wasmLat, metrics)
	writeReport(t, "latency-summary.md", summary)
	t.Log("\n" + summary)

	assertBudget(t, "native", nativeLat)
	assertBudget(t, "js/wasm", wasmLat)

	// ADR 0016's stated concern is the module's own load cost against the 300 ms interactive
	// budget. Publish the number; fail only if instantiation alone would consume the budget.
	if metrics.CompileAndInstantiateMS > 300 {
		t.Errorf("js/wasm instantiate took %.1f ms, over the 300 ms interactive budget on its own",
			metrics.CompileAndInstantiateMS)
	}
}

func assertBudget(t *testing.T, name string, r latencyReport) {
	t.Helper()
	for _, stage := range []string{stageNormalise, stageRules, stageValidators, stageModel} {
		st, ok := r.Stages[stage]
		if !ok {
			t.Errorf("%s: the %s stage was never measured", name, stage)
			continue
		}
		limit := r.BudgetMS[stage]
		if limit <= 0 {
			t.Errorf("%s: no budget is declared for stage %s", name, stage)
			continue
		}
		if st.P95 > limit {
			t.Errorf("%s: %s p95 %.3f ms exceeds its §9.4 share of %.1f ms", name, stage, st.P95, limit)
		}
	}
	if r.SumStageP95MS > r.BudgetMS["total"] {
		t.Errorf("%s: stage p95s sum to %.3f ms, over the %.1f ms interactive target", name, r.SumStageP95MS, r.BudgetMS["total"])
	}
	if !r.WithinBudget {
		t.Errorf("%s: the measurement itself reports over-budget stages: %v", name, r.OverBudget)
	}
	if r.Total.Count == 0 {
		t.Errorf("%s: no classification was measured", name)
	}
}

func readLatency(t *testing.T, path string) latencyReport {
	t.Helper()
	var r latencyReport
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the latency report: %v", err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("the latency report is not JSON: %v", err)
	}
	if len(r.Stages) == 0 {
		t.Fatalf("the latency report has no stages: %s", string(b))
	}
	return r
}

func copyReport(t *testing.T, name, src string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	writeReport(t, name, string(b))
}

func latencySummary(native, wasm latencyReport, m wasmMetrics) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# §9.4 latency, measured (%s)\n\n", runtime.GOOS+"/"+runtime.GOARCH)
	fmt.Fprintf(&b, "Corpus `%s`, %d cases, %d iterations per case. p95 is nearest-rank over every stage observation.\n\n",
		filepath.Base(native.Corpus), native.Cases, native.Iterations)
	b.WriteString("| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, stage := range []string{stageNormalise, stageRules, stageValidators, stageModel} {
		fmt.Fprintf(&b, "| %s | %.0f ms | %.4f ms | %.0f ms | %.4f ms |\n",
			stage, native.BudgetMS[stage], native.Stages[stage].P95, wasm.BudgetMS[stage], wasm.Stages[stage].P95)
	}
	fmt.Fprintf(&b, "| **sum of stage p95** | %.0f ms target | %.4f ms | %.0f ms target | %.4f ms |\n",
		native.BudgetMS["total"], native.SumStageP95MS, wasm.BudgetMS["total"], wasm.SumStageP95MS)
	fmt.Fprintf(&b, "| **whole classification** | — | p50 %.4f / p95 %.4f / max %.4f ms | — | p50 %.4f / p95 %.4f / max %.4f ms |\n",
		native.Total.P50, native.Total.P95, native.Total.Max, wasm.Total.P50, wasm.Total.P95, wasm.Total.Max)
	fmt.Fprintf(&b, "\njs/wasm module: %d bytes; compile+instantiate %.3f ms; module run (whole corpus) %.3f ms.\n",
		m.ModuleBytes, m.CompileAndInstantiateMS, m.RunMS)
	within := append([]string{}, native.OverBudget...)
	within = append(within, wasm.OverBudget...)
	sort.Strings(within)
	if len(within) == 0 {
		b.WriteString("\nEvery stage p95 is within its §9.4 share and every sum is within the interactive target.\n")
	} else {
		fmt.Fprintf(&b, "\nOVER BUDGET: %v\n", within)
	}
	return b.String()
}
