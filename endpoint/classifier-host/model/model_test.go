package model_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/model"
)

func never() bool { return false }

// shipped loads the artefact the product ships.
func shipped(t *testing.T) *model.Model {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "rules", "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := model.Parse(raw)
	if err != nil {
		t.Fatalf("the shipped model artefact is rejected: %v", err)
	}
	return m
}

func TestShippedArtefactParses(t *testing.T) {
	if v := shipped(t).Version(); v == "" {
		t.Fatal("the shipped artefact has no version")
	}
}

func TestHostileArtefactsAreRejected(t *testing.T) {
	base := map[string]any{
		"version": "v",
		"scale":   1000,
		"classes": []any{map[string]any{
			"class": "health", "threshold": 300,
			"features": []any{map[string]any{"token": "patient", "weight": 200}},
		}},
	}
	mutate := func(f func(m map[string]any)) []byte {
		b, _ := json.Marshal(base)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	class := func(m map[string]any) map[string]any { return m["classes"].([]any)[0].(map[string]any) }
	cases := []struct {
		name string
		raw  []byte
	}{
		{"not json", []byte("{")},
		{"unknown field", []byte(`{"version":"v","scale":1000,"classes":[],"network":"https://example.invalid"}`)},
		{"no version", mutate(func(m map[string]any) { delete(m, "version") })},
		{"zero scale", mutate(func(m map[string]any) { m["scale"] = 0 })},
		{"no classes", mutate(func(m map[string]any) { m["classes"] = []any{} })},
		{"bad class name", mutate(func(m map[string]any) { class(m)["class"] = "Not A Class" })},
		{"threshold over scale", mutate(func(m map[string]any) { class(m)["threshold"] = 5000 })},
		{"no features", mutate(func(m map[string]any) { class(m)["features"] = []any{} })},
		{"zero weight", mutate(func(m map[string]any) {
			class(m)["features"].([]any)[0].(map[string]any)["weight"] = 0
		})},
		{"trailing content", append(mutate(func(map[string]any) {}), []byte(`{"x":1}`)...)},
	}
	if _, err := model.Parse(mutate(func(map[string]any) {})); err != nil {
		t.Fatalf("the unmutated base artefact is rejected: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := model.Parse(tc.raw); err == nil {
				t.Fatal("a hostile artefact was accepted")
			}
		})
	}
}

func TestScoringIsFixedPointAndRepeatable(t *testing.T) {
	m := shipped(t)
	text := "the patient received a prescription; the physician noted the diagnosis and dosage"
	first := m.Score(text, nil, never)
	if len(first) == 0 {
		t.Fatal("a clinical sentence scored nothing")
	}
	for _, p := range first {
		if p.Score != float64(p.Fixed)/1000 || p.Score < 0 || p.Score > 1 {
			t.Errorf("score %v is not the integer sum %d over the scale", p.Score, p.Fixed)
		}
	}
	for i := 0; i < 20; i++ {
		again := m.Score(text, nil, never)
		if len(again) != len(first) {
			t.Fatalf("repeat %d produced %d predictions, want %d", i, len(again), len(first))
		}
		for j := range again {
			if again[j] != first[j] {
				t.Fatalf("repeat %d differs at %d: %+v vs %+v", i, j, again[j], first[j])
			}
		}
	}
}

func TestPhrasesAndCountCaps(t *testing.T) {
	raw := []byte(`{"version":"v","scale":1000,"classes":[{"class":"c","threshold":1,"features":[
		{"token":"governing law","weight":100},{"token":"shall","weight":10,"max_count":2}]}]}`)
	m, err := model.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Score("Governing law: this shall, shall, shall apply. governing\nlaw", nil, never)
	if len(got) != 1 || got[0].Fixed != 2*100+2*10 {
		t.Fatalf("predictions %+v, want one class with 220", got)
	}
}

func TestResolvedClassesAreSkipped(t *testing.T) {
	m := shipped(t)
	text := "the patient received a prescription after the diagnosis"
	all := m.Score(text, nil, never)
	if len(all) == 0 {
		t.Fatal("no predictions to skip")
	}
	for _, p := range m.Score(text, map[string]bool{all[0].Class: true}, never) {
		if p.Class == all[0].Class {
			t.Errorf("class %q was scored although it was resolved", p.Class)
		}
	}
}

func TestExpiryStopsScoring(t *testing.T) {
	if preds := shipped(t).Score("patient diagnosis prescription", nil, func() bool { return true }); len(preds) != 0 {
		t.Errorf("an expired deadline still produced %d predictions", len(preds))
	}
}

func TestScoresAreOrderedDeterministically(t *testing.T) {
	preds := shipped(t).Score("the patient had a diagnosis; the agreement was signed; package main func main", nil, never)
	for i := 1; i < len(preds); i++ {
		if preds[i-1].Fixed < preds[i].Fixed || preds[i-1].Fixed == preds[i].Fixed && preds[i-1].Class > preds[i].Class {
			t.Fatalf("predictions are not ordered by score then class: %+v", preds)
		}
	}
}
