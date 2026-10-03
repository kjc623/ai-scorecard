// Package classifierhost is the component-level documentation for the classifier host. The code
// lives in the subpackages: classify (pipeline, budget, release states), rules (the data DSL and
// its interpreter), validators, model, release (signed releases), norm (normalisation),
// docparse and parser/parser/isolation (the §10 child and its parent-enforced limits), and
// cmd/classifier-host (one source, native and js/wasm).
//
// The tests at this level are the ones that need the *built artefacts* rather than a package:
// equivalence_test.go builds both targets and diffs the labels of a fixed corpus, and
// measure_test.go produces the per-stage latency report ADR 0016 makes an acceptance criterion.
package classifierhost
