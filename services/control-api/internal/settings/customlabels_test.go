package settings_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
)

const customLabelsPath = "/admin/v1/settings/custom-labels"

// customLabels reads the custom labels the Settings read carries, canonicalised.
func (r *rig) customLabels(t *testing.T) string {
	t.Helper()
	rec := r.do(t, "admin", "GET", "/admin/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		CustomLabels json.RawMessage `json:"custom_labels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return canonical(t, string(got.CustomLabels))
}

// TestCustomLabelsReplaceTheList: a tenant without labels reads []; a PUT writes the list with its
// terms tidied (trimmed, repeats dropped, extensions without their dot and lower-cased) and the read
// returns it by display name; a rule may then name a label; a label a rule names cannot be left
// out, and once the rule no longer names it, it can. Each accepted write is one audit row with the
// previous and the new list.
func TestCustomLabelsReplaceTheList(t *testing.T) {
	r := newRig(t)
	if got := r.customLabels(t); got != `[]` {
		t.Fatalf("a tenant without custom labels reads %s", got)
	}

	put := `{"custom_labels":[
	  {"label":"custom_project_falcon","display_name":" Project Falcon ","words":["falcon"," Falcon ","project falcon",""],
	   "customer_names":["Acme Corp"],"file_extensions":[],"file_names":["falcon-*.xlsx"]},
	  {"label":"custom_cad","display_name":"CAD drawings","words":[],"customer_names":[],"file_extensions":[".DWG","step"],"file_names":[]}]}`
	if rec := r.do(t, "admin", "PUT", customLabelsPath, put); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	first := canonical(t, `[
	  {"label":"custom_cad","display_name":"CAD drawings","words":[],"customer_names":[],"file_extensions":["dwg","step"],"file_names":[]},
	  {"label":"custom_project_falcon","display_name":"Project Falcon","words":["falcon","project falcon"],
	   "customer_names":["Acme Corp"],"file_extensions":[],"file_names":["falcon-*.xlsx"]}]`)
	if got := r.customLabels(t); got != first {
		t.Fatalf("after the PUT:\n got %s\nwant %s", got, first)
	}

	if rec := r.do(t, "admin", "PUT", rulesPath, `{"rules":[{"rule_id":"block_falcon","action":"block","message":"Not for AI tools.","match":{"labels":["custom_project_falcon","credential"]}}]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("a rule naming a custom label: %d %s", rec.Code, rec.Body)
	}
	rec := r.do(t, "admin", "PUT", customLabelsPath, `{"custom_labels":[]}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != apierr.CodeCustomLabelInUse {
		t.Fatalf("leaving out a label a rule names: %d %s, want 409 %s", rec.Code, rec.Body, apierr.CodeCustomLabelInUse)
	}
	if got := r.customLabels(t); got != first {
		t.Fatalf("a refused write changed the labels: %s", got)
	}
	if rec := r.do(t, "admin", "PUT", rulesPath, `{"rules":[]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("clear rules: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(t, "admin", "PUT", customLabelsPath, `{"custom_labels":[]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if got := r.customLabels(t); got != `[]` {
		t.Fatalf("after clearing: %s", got)
	}

	var labelAudits []string
	for _, a := range r.store.Audits() {
		if a.Action != "tenant.custom_labels.set" {
			continue
		}
		if a.ActorID != admin.Actor || a.ObjectID != tenantA || a.Detail["subject"] != admin.Subject {
			t.Fatalf("audit %+v", a)
		}
		previous, _ := json.Marshal(a.Detail["previous"])
		next, _ := json.Marshal(a.Detail["new"])
		labelAudits = append(labelAudits, canonical(t, string(previous))+" -> "+canonical(t, string(next)))
	}
	if len(labelAudits) != 2 || !strings.HasPrefix(labelAudits[0], "[] -> ") || !strings.HasSuffix(labelAudits[1], " -> []") ||
		!strings.Contains(labelAudits[0], `"display_name":"Project Falcon"`) {
		t.Fatalf("custom label audits = %v, want the first write and the clear", labelAudits)
	}
}

// TestRulesRefuseAnUnknownCustomLabel: a rule may name only a custom label the tenant holds.
func TestRulesRefuseAnUnknownCustomLabel(t *testing.T) {
	r := newRig(t)
	rec := r.do(t, "admin", "PUT", rulesPath, `{"rules":[{"rule_id":"r","action":"block","message":"x","match":{"labels":["custom_nothing"]}}]}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != apierr.CodeInvalidRequest {
		t.Fatalf("a rule naming an unknown custom label: %d %s", rec.Code, rec.Body)
	}
}

// TestCustomLabelsRefuseEveryBadValue: each refused value is a 400 that names the label and the
// field, and leaves the list in force unchanged.
func TestCustomLabelsRefuseEveryBadValue(t *testing.T) {
	r := newRig(t)
	label := func(fields string) string {
		return `{"custom_labels":[{"label":"custom_ok","display_name":"OK","words":["ok"],"customer_names":[],"file_extensions":[],"file_names":[]},` + fields + `]}`
	}
	ok := func(over string) string {
		base := map[string]string{"label": `"custom_two"`, "display_name": `"Two"`, "words": `["two"]`, "customer_names": `[]`, "file_extensions": `[]`, "file_names": `[]`}
		for _, kv := range strings.Split(over, ";") {
			if k, v, found := strings.Cut(kv, "="); found {
				base[k] = v
			}
		}
		var parts []string
		for _, k := range []string{"label", "display_name", "words", "customer_names", "file_extensions", "file_names"} {
			parts = append(parts, fmt.Sprintf("%q:%s", k, base[k]))
		}
		return label("{" + strings.Join(parts, ",") + "}")
	}
	var many []string
	for i := 0; i <= 50; i++ {
		many = append(many, fmt.Sprintf(`{"label":"custom_l%d","display_name":"L%d","words":["w"],"customer_names":[],"file_extensions":[],"file_names":[]}`, i, i))
	}
	var terms []string
	for i := 0; i <= 200; i++ {
		terms = append(terms, fmt.Sprintf(`"t%d"`, i))
	}
	cases := []struct {
		name, body, code, field string
	}{
		{"no list", `{}`, apierr.CodeInvalidRequest, ""},
		{"51 labels", `{"custom_labels":[` + strings.Join(many, ",") + `]}`, apierr.CodeInvalidRequest, ""},
		{"a label without the prefix", ok(`label="two"`), apierr.CodeInvalidRequest, "label"},
		{"an upper-case label", ok(`label="custom_Two"`), apierr.CodeInvalidRequest, "label"},
		{"a label of 64 characters", ok(`label="custom_` + strings.Repeat("x", 57) + `"`), apierr.CodeInvalidRequest, "label"},
		{"a repeated label", ok(`label="custom_ok"`), apierr.CodeInvalidRequest, "label"},
		{"a blank display name", ok(`display_name="  "`), apierr.CodeInvalidRequest, "display_name"},
		{"a display name of 81 characters", ok(`display_name="` + strings.Repeat("x", 81) + `"`), apierr.CodeInvalidRequest, "display_name"},
		{"a repeated display name", ok(`display_name="ok"`), apierr.CodeInvalidRequest, "display_name"},
		{"no terms", ok(`words=[" "]`), apierr.CodeInvalidRequest, "words"},
		{"a word of 101 characters", ok(`words=["` + strings.Repeat("x", 101) + `"]`), apierr.CodeInvalidRequest, "words"},
		{"a word of punctuation", ok(`words=["--"]`), apierr.CodeInvalidRequest, "words"},
		{"a customer name with a control character", ok(`customer_names=["Acme\u0007"]`), apierr.CodeInvalidRequest, "customer_names"},
		{"an extension with a space", ok(`file_extensions=["d wg"]`), apierr.CodeInvalidRequest, "file_extensions"},
		{"an extension of 17 characters", ok(`file_extensions=["` + strings.Repeat("x", 17) + `"]`), apierr.CodeInvalidRequest, "file_extensions"},
		{"a file name with a path", ok(`file_names=["docs/plan.pdf"]`), apierr.CodeInvalidRequest, "file_names"},
		{"a file name with a bracket", ok(`file_names=["plan[1].pdf"]`), apierr.CodeInvalidRequest, "file_names"},
		{"a file name of wildcards", ok(`file_names=["*"]`), apierr.CodeInvalidRequest, "file_names"},
		{"201 terms", ok(`words=[` + strings.Join(terms, ",") + `]`), apierr.CodeInvalidRequest, "words"},
		{"an unknown field", label(`{"label":"custom_two","display_name":"Two","words":["two"],"regex":"x"}`), apierr.CodeSchemaViolation, ""},
	}
	for _, c := range cases {
		rec := r.do(t, "admin", "PUT", customLabelsPath, c.body)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != c.code {
			t.Errorf("%s: %d %s, want 400 %s", c.name, rec.Code, rec.Body, c.code)
			continue
		}
		if c.field != "" {
			var e struct {
				Error struct {
					Detail struct {
						Index int    `json:"index"`
						Field string `json:"field"`
					} `json:"detail"`
				} `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			if e.Error.Detail.Index != 1 || e.Error.Detail.Field != c.field {
				t.Errorf("%s: names label %d field %q, want label 1 field %q: %s", c.name, e.Error.Detail.Index, e.Error.Detail.Field, c.field, rec.Body)
			}
		}
	}
	if got := r.customLabels(t); got != `[]` {
		t.Fatalf("a refused write changed the labels: %s", got)
	}

	// The boundaries are accepted: the most labels, the most terms, the longest values.
	if rec := r.do(t, "admin", "PUT", customLabelsPath, `{"custom_labels":[`+strings.Join(many[:50], ",")+`]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("50 labels: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(t, "admin", "PUT", customLabelsPath, ok(`label="custom_`+strings.Repeat("x", 56)+`";display_name="`+strings.Repeat("é", 80)+`";words=[`+strings.Join(terms[:200], ",")+`]`)); rec.Code != http.StatusNoContent {
		t.Fatalf("boundaries: %d %s", rec.Code, rec.Body)
	}
}
