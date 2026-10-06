package rules_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/internal/importcheck"
	"github.com/shadow-ai-capture/device/classifier-host/rules"
	"github.com/shadow-ai-capture/device/classifier-host/validators"
)

const cardRules = `{
  "version": "unit-1",
  "rules": [
    {"rule_id": "PAN", "class": "payment_card", "score": 0.9,
     "when": [
       {"signal": "regex", "dialect": "linear", "pattern": "[0-9]{13,19}", "max_matches": 8},
       {"signal": "validator", "validator": "luhn"},
       {"signal": "context", "predicate": "not_preceded_by", "value": "test"}
     ]}
  ]
}`

func compile(t *testing.T, doc string) *rules.Set {
	t.Helper()
	set, err := rules.Compile([]byte(doc))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return set
}

func TestCompileAndEvaluate(t *testing.T) {
	set := compile(t, cardRules)
	if set.Len() != 1 || set.Version() != "unit-1" {
		t.Fatalf("compiled %d rules, version %q", set.Len(), set.Version())
	}
	text := "please charge 4111111111111111 today"
	out := set.Evaluate(text, time.Time{}, time.Now)
	if len(out.Candidates) != 1 || out.Truncated {
		t.Fatalf("outcome %+v", out)
	}
	c := out.Candidates[0]
	if c.RuleID != "PAN" || c.Class != "payment_card" || c.Score != 0.9 || c.MatchText(text) != "4111111111111111" {
		t.Errorf("candidate %+v", c)
	}
	if len(c.Validators) != 1 || c.Validators[0] != "luhn" {
		t.Errorf("the candidate must carry its validators to the next stage, got %v", c.Validators)
	}
}

func TestHostileDocumentsAreRejectedWhole(t *testing.T) {
	one := `{"rule_id":"R","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}`
	many := `{"version":"v","rules":[` + strings.Repeat(one+",", rules.MaxRules) + one + `]}`
	rule := func(r string) string { return `{"version":"v","rules":[` + r + `]}` }
	cases := []struct {
		name, body, want string
	}{
		{"not json", `{`, "schema"},
		{"empty", ``, "empty"},
		{"no version", `{"rules":[` + one + `]}`, "version"},
		{"no rules", `{"version":"v","rules":[]}`, "no rules"},
		{"unknown top-level field", `{"version":"v","rules":[],"exec":"rm -rf /"}`, "schema"},
		{"unknown rule field", rule(`{"rule_id":"A","class":"c","score":0.5,"script":"x","when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}`), "schema"},
		{"unknown signal field", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x","host":"y"}]}`), "schema"},
		{"unknown signal kind", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"},{"signal":"code","value":"x"}]}`), "closed set"},
		{"unknown validator", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"},{"signal":"validator","validator":"exec"}]}`), "closed validator set"},
		{"unknown predicate", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"},{"signal":"context","predicate":"matches","value":"."}]}`), "closed set"},
		{"non-linear dialect", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"pcre","pattern":"x"}]}`), "dialect"},
		{"lookahead", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"(?=secret)"}]}`), "does not compile"},
		{"backreference", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"(a)\\1"}]}`), "does not compile"},
		{"pattern over the limit", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"` + strings.Repeat("a", rules.MaxPatternBytes+1) + `"}]}`), "limit"},
		{"too many rules", many, "limit"},
		{"duplicate rule_id", rule(one + "," + one), "twice"},
		{"score out of range", rule(`{"rule_id":"A","class":"c","score":1.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}`), "outside [0,1]"},
		{"validator first", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"validator","validator":"luhn"},{"signal":"regex","dialect":"linear","pattern":"x"}]}`), "first signal"},
		{"two regex signals", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"},{"signal":"regex","dialect":"linear","pattern":"y"}]}`), "first signal"},
		{"max_matches over the limit", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x","max_matches":100000}]}`), "max_matches"},
		{"empty when", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[]}`), "empty"},
		{"field of another kind", rule(`{"rule_id":"A","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x","validator":"luhn"}]}`), "another kind"},
		{"rule_id charset", rule(`{"rule_id":"lower-case","class":"c","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}`), "rule_id"},
		{"class charset", rule(`{"rule_id":"A","class":"Not A Class","score":0.5,"when":[{"signal":"regex","dialect":"linear","pattern":"x"}]}`), "class"},
		{"trailing content", rule(one) + `{"more":1}`, "trailing content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := rules.Compile([]byte(tc.body))
			if err == nil || set != nil {
				t.Fatalf("a hostile document was accepted: %v", set)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "rules: document rejected") {
				t.Errorf("rejection %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestContextPredicatesFilterMatches(t *testing.T) {
	set := compile(t, cardRules)
	for text, want := range map[string]int{
		"charge 4111111111111111":                             1,
		"test 4111111111111111":                               0,
		"this is a TEST of the 4111111111111111 number":       0,
		"4111111111111111 in a test, after the number":        1,
		"a test card, long ago and far away 4111111111111111": 1,
	} {
		if got := len(set.Evaluate(text, time.Time{}, time.Now).Candidates); got != want {
			t.Errorf("%q: %d candidates, want %d", text, got, want)
		}
	}
}

func TestMaxMatchesIsEnforced(t *testing.T) {
	set := compile(t, `{"version":"v","rules":[{"rule_id":"D","class":"n","score":0.5,
		"when":[{"signal":"regex","dialect":"linear","pattern":"[0-9]{3}","max_matches":2}]}]}`)
	if got := len(set.Evaluate("111 222 333 444", time.Time{}, time.Now).Candidates); got != 2 {
		t.Fatalf("%d candidates, want the declared maximum of 2", got)
	}
}

func TestExpiredDeadlineTruncates(t *testing.T) {
	out := compile(t, cardRules).Evaluate("charge 4111111111111111", time.Now().Add(-time.Millisecond), time.Now)
	if !out.Truncated || out.StoppedAt != "PAN" || len(out.Candidates) != 0 {
		t.Fatalf("outcome %+v", out)
	}
}

// TestShippedRules compiles the rules the product ships and checks what each one must and must
// not match. Validators are applied here as the pipeline applies them.
func TestShippedRules(t *testing.T) {
	raw, err := os.ReadFile("default.json")
	if err != nil {
		t.Fatal(err)
	}
	set := compile(t, string(raw))
	cases := []struct {
		text string
		want []string // rule ids, in rule order
	}{
		{"charge 4111 1111 1111 1111 please", []string{"PAYMENT_CARD_PAN"}},
		{"amex 3782-822463-10005", []string{"PAYMENT_CARD_PAN"}},
		{"card 4111 1111 1111 1111 1234 expires", []string{"PAYMENT_CARD_PAN"}},
		{"test card 4111 1111 1111 1111", nil},
		{"invoice 4111 1111 1111 1112", nil},
		{"ssn 123-45-6788", []string{"US_SSN_STRUCTURE"}},
		{"ssn 078-05-1120", nil},
		{"nino QQ 12 34 56 A and AB123456C", []string{"UK_NINO"}},
		{"-----BEGIN OPENSSH PRIVATE KEY-----", []string{"PRIVATE_KEY_BLOCK"}},
		{"-----BEGIN PGP PRIVATE KEY BLOCK-----", []string{"PRIVATE_KEY_BLOCK"}},
		{"key AKIAIOSFODNN7EXAMPLE", []string{"AWS_ACCESS_KEY_ID"}},
		{"her Date of Birth is on file", []string{"CUSTOMER_PII_LABEL"}},
		{"the diagnosis was confirmed", []string{"HEALTH_CLINICAL_VOCABULARY"}},
		{"please be patient", nil},
		{"Whereas the parties agree", []string{"LEGAL_CLAUSE_VOCABULARY"}},
		{"the liability of the parties", nil},
		{"package main\n\nfunc main() {", []string{"SOURCE_DECLARATION"}},
		{"export default class Widget extends Base {", []string{"SOURCE_DECLARATION"}},
		{"#include <stdio.h>", []string{"SOURCE_DECLARATION"}},
		{"import os", []string{"SOURCE_DECLARATION"}},
		{"import duties on steel rose this year", nil},
		{"the class was cancelled", nil},
		{"The weather in Lisbon is mild in October.", nil},
	}
	for _, tc := range cases {
		var got []string
		for _, c := range set.Evaluate(tc.text, time.Time{}, time.Now).Candidates {
			if confirmed(c, tc.text) && (len(got) == 0 || got[len(got)-1] != c.RuleID) {
				got = append(got, c.RuleID)
			}
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%q matched %v, want %v", tc.text, got, tc.want)
		}
	}
}

func confirmed(c rules.Candidate, text string) bool {
	for _, name := range c.Validators {
		if check, _ := validators.Lookup(name); !check(c.MatchText(text)) {
			return false
		}
	}
	return true
}

// TestRulesCannotReachTheSystem pins the package's whole dependency set.
func TestRulesCannotReachTheSystem(t *testing.T) {
	importcheck.AssertClosed(t, ".",
		"bytes", "encoding/json", "errors", "fmt", "io", "regexp", "strings", "time", "unicode/utf8",
		"github.com/shadow-ai-capture/device/classifier-host/validators")
}
