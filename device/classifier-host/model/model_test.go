package model_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/model"
)

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestLoadVerifiesTheDigestBeforeAnythingElse(t *testing.T) {
	raw := model.DevArtefactJSON()

	if _, err := model.Load(raw, "", model.DefaultCaps()); err == nil {
		t.Fatal("a load with no digest to verify against was accepted")
	}
	if _, err := model.Load(raw, digest([]byte("other")), model.DefaultCaps()); err == nil {
		t.Fatal("an artefact that does not match its digest was accepted")
	} else if !strings.Contains(err.Error(), "does not match its signed digest") {
		t.Errorf("unexpected error: %v", err)
	}
	m, err := model.Load(raw, digest(raw), model.DefaultCaps())
	if err != nil {
		t.Fatalf("a correctly digested artefact was rejected: %v", err)
	}
	if m.Version() != "dev-artefact-1" {
		t.Errorf("version %q", m.Version())
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
	cases := []struct {
		name string
		raw  []byte
	}{
		{"not json", []byte("{")},
		{"unknown field", []byte(`{"version":"v","scale":1000,"classes":[],"network":"https://evil"}`)},
		{"no version", mutate(func(m map[string]any) { delete(m, "version") })},
		{"zero scale", mutate(func(m map[string]any) { m["scale"] = 0 })},
		{"no classes", mutate(func(m map[string]any) { m["classes"] = []any{} })},
		{"bad class name", mutate(func(m map[string]any) {
			m["classes"].([]any)[0].(map[string]any)["class"] = "Not A Class"
		})},
		{"threshold over scale", mutate(func(m map[string]any) {
			m["classes"].([]any)[0].(map[string]any)["threshold"] = 5000
		})},
		{"no features", mutate(func(m map[string]any) {
			m["classes"].([]any)[0].(map[string]any)["features"] = []any{}
		})},
		{"zero weight", mutate(func(m map[string]any) {
			m["classes"].([]any)[0].(map[string]any)["features"].([]any)[0].(map[string]any)["weight"] = 0
		})},
		{"trailing content", append(mutate(func(m map[string]any) {}), []byte(`{"x":1}`)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := model.Load(tc.raw, digest(tc.raw), model.DefaultCaps()); err == nil {
				t.Fatal("a hostile model artefact was accepted")
			}
		})
	}
}

// TestScoringIsFixedPointAndRepeatable is §9.1's determinism requirement at its root: scores are
// integer arithmetic divided once, so two targets cannot disagree in the last bit.
func TestScoringIsFixedPointAndRepeatable(t *testing.T) {
	raw := model.DevArtefactJSON()
	m, err := model.Load(raw, digest(raw), model.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	text := "the patient received a prescription; the physician noted the diagnosis and dosage"
	first := m.Score(text, nil, nil)
	if len(first) == 0 {
		t.Fatal("the development artefact scored a clinical sentence as nothing")
	}
	for _, p := range first {
		if p.Score != float64(p.Fixed)/1000.0 {
			t.Errorf("score %v is not the integer sum divided by the scale: fixed=%d", p.Score, p.Fixed)
		}
		if p.Score < 0 || p.Score > 1 {
			t.Errorf("score %v is outside [0,1]", p.Score)
		}
	}
	for i := 0; i < 50; i++ {
		again := m.Score(text, nil, nil)
		if len(again) != len(first) {
			t.Fatalf("repeat %d produced %d predictions, first run produced %d", i, len(again), len(first))
		}
		for j := range again {
			if again[j] != first[j] {
				t.Fatalf("repeat %d differs at prediction %d: %+v vs %+v", i, j, again[j], first[j])
			}
		}
	}
}

func TestResolvedClassesAreSkipped(t *testing.T) {
	raw := model.DevArtefactJSON()
	m, err := model.Load(raw, digest(raw), model.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	text := "the patient received a prescription"
	all := m.Score(text, nil, nil)
	if len(all) == 0 {
		t.Fatal("no predictions to skip")
	}
	skip := map[string]bool{all[0].Class: true}
	for _, p := range m.Score(text, skip, nil) {
		if p.Class == all[0].Class {
			t.Errorf("§9.2 says the model runs on what the rules did not resolve, but %q was scored anyway", p.Class)
		}
	}
}

func TestDeadlineStopsScoring(t *testing.T) {
	raw := model.DevArtefactJSON()
	m, err := model.Load(raw, digest(raw), model.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	preds := m.Score("patient diagnosis prescription", nil, func() bool { return true })
	if len(preds) != 0 {
		t.Errorf("an expired deadline still produced %d predictions", len(preds))
	}
}

func TestScoresAreSortedDeterministically(t *testing.T) {
	raw := model.DevArtefactJSON()
	m, err := model.Load(raw, digest(raw), model.DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	preds := m.Score("the patient had a diagnosis; the agreement was signed; package main func main", nil, nil)
	for i := 1; i < len(preds); i++ {
		if preds[i-1].Fixed < preds[i].Fixed {
			t.Fatalf("predictions are not ordered by score: %+v", preds)
		}
		if preds[i-1].Fixed == preds[i].Fixed && preds[i-1].Class > preds[i].Class {
			t.Fatalf("ties are not broken by class name: %+v", preds)
		}
	}
}
