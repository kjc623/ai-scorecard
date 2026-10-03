package rules_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/internal/importcheck"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
)

func packageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the package directory")
	}
	return filepath.Dir(file)
}

const validRules = `{
  "version": "unit-1",
  "rules": [
    {"rule_id": "PAN", "class": "payment_card", "score": 0.9,
     "when": [
       {"signal": "regex", "dialect": "linear", "pattern": "[0-9]{13,19}", "max_matches": 8},
       {"signal": "validator", "validator": "luhn"},
       {"signal": "context", "predicate": "not_preceded_by", "value": "test"}
     ],
     "emits": {"excerpt_kind": "match_span"}}
  ]
}`

func TestValidRulesCompileAndEvaluate(t *testing.T) {
	set, err := rules.Compile([]byte(validRules), rules.DefaultCaps())
	if err != nil {
		t.Fatalf("a valid rules document was rejected: %v", err)
	}
	if set.RuleCount() != 1 || set.Version() != "unit-1" {
		t.Fatalf("compiled set is %d rules, version %q", set.RuleCount(), set.Version())
	}
	out := set.Evaluate("please charge 4111111111111111 today", time.Time{}, time.Now)
	if len(out.Candidates) != 1 {
		t.Fatalf("expected one candidate, got %d (%+v)", len(out.Candidates), out.Candidates)
	}
	c := out.Candidates[0]
	if c.RuleID != "PAN" || c.Class != "payment_card" || c.Score != 0.9 {
		t.Errorf("candidate is %+v", c)
	}
	if len(c.PendingValidators) != 1 || c.PendingValidators[0] != "luhn" {
		t.Errorf("the validator must be deferred to the validators stage, got %v", c.PendingValidators)
	}
	if got := c.MatchText("please charge 4111111111111111 today"); got != "4111111111111111" {
		t.Errorf("match text is %q", got)
	}
}

// TestHostileRuleFilesAreRejectedWhole is §9.5's "a malformed or hostile rule file is rejected,
// never partially applied": every one of these must produce an error and *no* set, so the caller
// cannot fall back to a partially loaded rule file.
func TestHostileRuleFilesAreRejectedWhole(t *testing.T) {
	long := strings.Repeat("a", 600)
	oneRule := `{"rule_id":"R","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}`
	repeated := make([]string, 513)
	for i := range repeated {
		repeated[i] = oneRule
	}
	manyRules := `{"version":"v","rules":[` + strings.Join(repeated, ",") + `]}`
	cases := []struct {
		name string
		body string
		want string // substring of the error, so the rejection is actionable
	}{
		{"not json", `{`, "declared schema"},
		{"empty document", ``, "empty"},
		{"no version", `{"rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}]}`, "no version"},
		{"no rules", `{"version":"v","rules":[]}`, "no rules"},
		{"unknown top-level field", `{"version":"v","rules":[],"exec":"rm -rf /"}`, "declared schema"},
		{"unknown rule field", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"script":"process.exit()","when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}]}`, "declared schema"},
		{"unknown signal field", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x","host_function":"net.Dial"}]}]}`, "declared schema"},
		{"unknown signal kind", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"code","pattern":"x"}]}]}`, "closed operator set"},
		{"unknown validator", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"},{"signal":"validator","validator":"exec"}]}]}`, "closed validator set"},
		{"unknown predicate", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"},{"signal":"context","predicate":"matches_regex","value":"."}]}]}`, "closed set"},
		{"non-linear dialect", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"backtracking","pattern":"x"}]}]}`, "dialect"},
		{"lookahead pattern", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"(?=secret)"}]}]}`, "does not compile"},
		{"backreference pattern", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"(a)\\1"}]}]}`, "does not compile"},
		{"pattern over cap", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"` + long + `"}]}]}`, "over the"},
		{"too many rules", manyRules, "over the"},
		{"duplicate rule_id", `{"version":"v","rules":[
			{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]},
			{"rule_id":"A","class":"d","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"y"}]}]}`, "twice"},
		{"score out of range", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":1.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}]}`, "outside [0,1]"},
		{"validator before regex", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"validator","validator":"luhn"},{"signal":"regex","dialect":"linear","pattern":"x"}]}]}`, "before its regex"},
		{"two regex signals", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"},{"signal":"regex","dialect":"linear","pattern":"y"}]}]}`, "second regex"},
		{"max_matches over cap", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x","max_matches":100000}]}]}`, "max_matches"},
		{"empty when", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[]}]}`, "empty"},
		{"cross-kind field", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x","validator":"luhn"}]}]}`, "belongs to another signal kind"},
		{"bad rule_id charset", `{"version":"v","rules":[{"rule_id":"lower-case","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}]}`, "rule_id must match"},
		{"bad class charset", `{"version":"v","rules":[{"rule_id":"A","class":"Not A Class","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}]}`, "class"},
		{"trailing content", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}]}{"more":1}`, "trailing content"},
		{"bad excerpt kind", `{"version":"v","rules":[{"rule_id":"A","class":"c","score":0.5,"emits":{"excerpt_kind":"full_payload"},"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}]}`, "excerpt_kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := rules.Compile([]byte(tc.body), rules.DefaultCaps())
			if err == nil {
				t.Fatalf("hostile rule file was accepted: %d rules", set.RuleCount())
			}
			if set != nil {
				t.Fatalf("a rejected rule file returned a partially built set: %d rules", set.RuleCount())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("rejection %q does not mention %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), "rules: release rejected") {
				t.Errorf("rejection is not wrapped in ErrRejected: %v", err)
			}
		})
	}
}

func TestContextPredicateFiltersMatches(t *testing.T) {
	set, err := rules.Compile([]byte(validRules), rules.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		text string
		want int
	}{
		{"charge 4111111111111111", 1},
		{"test 4111111111111111", 0},
		{"this is a test of the 4111111111111111 number", 0},
		{"4111111111111111 in a documentation example", 1}, // "test" appears *after*, and the predicate is about before
	}
	for _, tc := range cases {
		out := set.Evaluate(tc.text, time.Time{}, time.Now)
		if len(out.Candidates) != tc.want {
			t.Errorf("%q: %d candidates, want %d", tc.text, len(out.Candidates), tc.want)
		}
	}
}

func TestMaxMatchesIsEnforced(t *testing.T) {
	body := `{"version":"v","rules":[{"rule_id":"DIGITS","class":"number","score":0.5,
		"when":[{"signal":"regex","dialect":"linear","pattern":"[0-9]{3}","max_matches":2}]}]}`
	set, err := rules.Compile([]byte(body), rules.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	out := set.Evaluate("111 222 333 444", time.Time{}, time.Now)
	if len(out.Candidates) != 2 {
		t.Fatalf("%d candidates, want the declared max of 2", len(out.Candidates))
	}
}

// TestExpiredDeadlineTruncatesInsteadOfRunningOn is §9.4's exhaustion rule at the rules stage: the
// engine stops, says so, and hands back what it already had.
func TestExpiredDeadlineTruncatesInsteadOfRunningOn(t *testing.T) {
	set, err := rules.Compile([]byte(validRules), rules.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	out := set.Evaluate("charge 4111111111111111", time.Now().Add(-time.Millisecond), time.Now)
	if !out.Truncated {
		t.Fatal("an expired deadline did not truncate the evaluation")
	}
	if out.StoppedInFamily != "payment_card" {
		t.Errorf("truncation did not name the family it stopped in: %q", out.StoppedInFamily)
	}
	if len(out.FamiliesRun) != 0 {
		t.Errorf("a family was reported as run even though nothing was evaluated: %v", out.FamiliesRun)
	}
}

func TestRuleIDsAreSortedAndStable(t *testing.T) {
	set, err := rules.Compile([]byte(validRules), rules.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	ids := set.RuleIDs()
	if len(ids) != 1 || ids[0] != "PAN" {
		t.Errorf("rule ids: %v", ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Errorf("rule ids are not sorted: %v", ids)
		}
	}
}

// TestRulesPackageCannotReachAnything is the structural half of §9.5: the interpreter that
// evaluates signed data must not be able to open a file, a socket, a process or the spool. The
// allowlist is the package's whole dependency set, so a future edit that adds os/net/exec fails
// here rather than shipping "promotable without a software release" as "arbitrary code delivered to
// the fleet".
func TestRulesPackageCannotReachAnything(t *testing.T) {
	importcheck.AssertClosed(t, packageDir(t),
		"bytes", "encoding/json", "errors", "fmt", "io", "regexp", "sort", "strings", "time",
		"github.com/shadow-ai-capture/device/classifier-host/validators")
	for _, forbidden := range importcheck.Forbidden {
		for _, imp := range importcheck.Imports(t, packageDir(t)) {
			if imp == forbidden {
				t.Errorf("package rules imports %q: a signed rule file must not be able to reach it", imp)
			}
		}
	}
}
