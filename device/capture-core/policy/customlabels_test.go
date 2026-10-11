package policy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

// customLabelsSection is a custom labels list in control-api's spelling: every list of every label.
const customLabelsSection = `[
  {"label": "custom_cad", "words": [], "customer_names": [], "file_extensions": ["dwg", "tar.gz"], "file_names": []},
  {"label": "custom_project_falcon", "words": ["falcon", "project falcon"], "customer_names": ["Acme Corp"],
   "file_extensions": [], "file_names": ["Falcon-*.xlsx", "roadmap?.pptx"]}
]`

func TestCustomLabelsDecode(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	b, err := v.Open(signWithMembers(t, priv, "42", map[string]string{"custom_labels": customLabelsSection}), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []CustomLabel{
		{Label: "custom_cad", Words: []string{}, CustomerNames: []string{}, FileExtensions: []string{"dwg", "tar.gz"}, FileNames: []string{}},
		{Label: "custom_project_falcon", Words: []string{"falcon", "project falcon"}, CustomerNames: []string{"Acme Corp"},
			FileExtensions: []string{}, FileNames: []string{"Falcon-*.xlsx", "roadmap?.pptx"}},
	}
	if !reflect.DeepEqual(b.CustomLabels, want) {
		t.Fatalf("custom_labels = %+v\nwant %+v", b.CustomLabels, want)
	}

	// The classifier takes the words and customer names of the labels that have any.
	if got, want := b.ClassifyTerms(), []protocol.CustomLabelTerms{{Label: "custom_project_falcon", Terms: []string{"falcon", "project falcon", "Acme Corp"}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("ClassifyTerms = %+v, want %+v", got, want)
	}

	for names, want := range map[string]string{
		"Site plan.DWG":                    "custom_cad",
		"backup.TAR.GZ":                    "custom_cad",
		"falcon-q3.xlsx|notes.txt":         "custom_project_falcon",
		"roadmap2.pptx|plan.dwg":           "custom_cad,custom_project_falcon",
		"roadmap22.pptx":                   "",
		"falcon-q3.xlsx.zip":               "",
		"dwg|archive.gz|no-extension-here": "",
	} {
		if got := strings.Join(b.FileLabels(strings.Split(names, "|")), ","); got != want {
			t.Errorf("FileLabels(%s) = %q, want %q", names, got, want)
		}
	}

	// A bundle without custom labels gives the classifier no terms and labels no file.
	none := testBundle("43")
	if none.ClassifyTerms() != nil || none.FileLabels([]string{"plan.dwg"}) != nil {
		t.Errorf("a bundle without custom labels labelled something")
	}
	var nilBundle *Bundle
	if nilBundle.ClassifyTerms() != nil || nilBundle.FileLabels([]string{"plan.dwg"}) != nil {
		t.Errorf("no bundle labelled something")
	}
}

func TestCustomLabelsValidation(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	refused := map[string][2]string{
		"label without the prefix":   {`"label": "custom_cad"`, `"label": "cad"`},
		"upper-case label":           {`"label": "custom_cad"`, `"label": "custom_CAD"`},
		"duplicate label":            {`"label": "custom_cad"`, `"label": "custom_project_falcon"`},
		"empty word":                 {`"words": ["falcon"`, `"words": [" "`},
		"empty file extension":       {`["dwg", "tar.gz"]`, `["dwg", ""]`},
		"unreadable file name":       {`"Falcon-*.xlsx"`, `"Falcon-[.xlsx"`},
		"misspelt list":              {`"customer_names": ["Acme Corp"]`, `"customers": ["Acme Corp"]`},
		"label of more than 56 more": {`"label": "custom_cad"`, `"label": "custom_` + strings.Repeat("c", 57) + `"`},
	}
	for name, r := range refused {
		section := strings.Replace(customLabelsSection, r[0], r[1], 1)
		if section == customLabelsSection {
			t.Fatalf("%s: the replacement did not apply", name)
		}
		_, err := v.Open(signWithMembers(t, priv, "50", map[string]string{"custom_labels": section}), nil)
		if CauseOf(err) != CauseSchemaInvalid {
			t.Errorf("%s: cause = %q, want %q (err=%v)", name, CauseOf(err), CauseSchemaInvalid, err)
		}
		if err != nil && strings.Contains(err.Error(), "Acme") {
			t.Errorf("%s: the error quotes a term: %v", name, err)
		}
	}
	if _, err := v.Open(signWithMembers(t, priv, "51", map[string]string{"custom_labels": `[]`}), nil); err != nil {
		t.Errorf("an empty list: refused: %v", err)
	}
}
