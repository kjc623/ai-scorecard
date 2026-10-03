package classifierhost_test

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/release"
)

// This file is the §9.1 table's second row, end to end: a document is parsed **natively only**
// ("Document parsing — Delegated to the child process" vs "Unavailable ... the extension may not
// parse a document"), and the text path stays byte-identical across the two targets.
//
// It runs the shipped CLI, so the native document case exercises the whole chain — host →
// parser/isolation → a real child process spawned as `parse-child` → parser → normalise → rules →
// validators → label — rather than a fake.

// docxWith returns a minimal but real docx: a zip whose word/document.xml carries the text.
func docxWith(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<Types/>`,
		"word/document.xml":   `<w:document><w:body><w:p>` + text + `</w:p></w:body></w:document>`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type documentCorpus struct {
	Name     string         `json:"name"`
	BudgetMS int64          `json:"budget_ms"`
	Cases    []documentCase `json:"cases"`
}

type documentCase struct {
	ID         string `json:"id"`
	Mode       string `json:"mode"`
	MediaType  string `json:"media_type"`
	ContentB64 string `json:"content_base64,omitempty"`
	Content    string `json:"content,omitempty"`
}

func TestDocumentParsingIsNativeOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both targets and spawns parser children")
	}
	goTool := requireTool(t, "go")
	nodeTool := requireNode(t)

	tmp := t.TempDir()
	priv, pub := testrig.Key(t)
	releaseDir := filepath.Join(tmp, "release")
	testrig.WriteRelease(t, releaseDir, priv, testrig.ReleaseOptions{Version: "documents-1", State: release.StateShadow})

	docx := docxWith(t, "please charge card 4111 1111 1111 1111 for the invoice")
	corpusBody := documentCorpus{
		Name:     "documents-native-only-v1",
		BudgetMS: 5000,
		Cases: []documentCase{
			{
				ID: "docx-with-a-card", Mode: "m1",
				MediaType:  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
				ContentB64: base64.StdEncoding.EncodeToString(docx),
			},
			{ID: "text-with-a-card", Mode: "m1", MediaType: "text/plain", Content: "please charge card 4111 1111 1111 1111 for the invoice"},
		},
	}
	corpusPath := filepath.Join(tmp, "documents.json")
	body, err := json.Marshal(corpusBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corpusPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	native := filepath.Join(tmp, "classifier-host"+exeSuffix())
	buildTarget(t, goTool, "", native)
	wasm := filepath.Join(tmp, "classifier-host.wasm")
	buildTarget(t, goTool, "js/wasm", wasm)
	copyShim(t, goTool, tmp)

	relDir := filepath.ToSlash(releaseDir)
	corpus := filepath.ToSlash(corpusPath)
	pubHex := testrig.PubHex(pub)
	nativeOut := filepath.Join(tmp, "native.json")
	wasmOut := filepath.Join(tmp, "wasm.json")

	run(t, filepath.Dir(native), native, "classify",
		"--release", relDir, "--pubkey", pubHex, "--corpus", corpus, "--out", filepath.ToSlash(nativeOut))
	runWasm(t, nodeTool, filepath.Join(testrig.ModuleRoot(), "tools", "run-wasm.mjs"), wasm, "classify",
		"--release", relDir, "--pubkey", pubHex, "--corpus", corpus, "--out", filepath.ToSlash(wasmOut))

	nativeRecords := readRecords(t, nativeOut)
	wasmRecords := readRecords(t, wasmOut)
	if len(nativeRecords) != 2 || len(wasmRecords) != 2 {
		t.Fatalf("expected two records per target, got %d and %d", len(nativeRecords), len(wasmRecords))
	}
	nativeDoc, nativeText := nativeRecords[0], nativeRecords[1]
	wasmDoc, wasmText := wasmRecords[0], wasmRecords[1]

	// Native: the document was parsed by a child process and its text classified.
	if nativeDoc.Degraded {
		t.Errorf("the native target degraded a parseable document: %+v", nativeDoc)
	}
	if !hasClass(nativeDoc, "payment_card") {
		t.Errorf("the document's text was not classified natively: %+v", nativeDoc.Labels)
	}

	// wasm: §9.1 says the extension may not parse a document, so the honest answer is degraded —
	// never an empty label set that would read as "no sensitive content".
	if !wasmDoc.Degraded {
		t.Errorf("the wasm target claimed to classify a document: %+v", wasmDoc)
	}
	if len(wasmDoc.Labels) != 0 {
		t.Errorf("a degraded wasm document carried labels: %+v", wasmDoc.Labels)
	}
	if !contains(wasmDoc.DegradedStages, "parse") {
		t.Errorf("the wasm document's degradation is not attributed to the parse stage: %+v", wasmDoc)
	}

	// The text case stays byte-identical: §9.1's property is about the classifier, and this is the
	// evidence that the document route is the *only* intended divergence.
	nativeTextJSON, _ := json.Marshal(nativeText)
	wasmTextJSON, _ := json.Marshal(wasmText)
	if !bytes.Equal(nativeTextJSON, wasmTextJSON) {
		t.Errorf("the text path diverged between targets:\n native: %s\n wasm:   %s", nativeTextJSON, wasmTextJSON)
	}

	report := "corpus=documents-native-only-v1 cases=2\n" +
		"native docx: degraded=" + boolString(nativeDoc.Degraded) + " labels=" + labelString(nativeDoc) + "\n" +
		"wasm docx:   degraded=" + boolString(wasmDoc.Degraded) + " stages=" + strings.Join(wasmDoc.DegradedStages, ",") + "\n" +
		"text path identical=true\n"
	writeReport(t, "document-routing.txt", report)
	t.Log("\n" + report)
}

func readRecords(t *testing.T, path string) []canonicalRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []canonicalRecord
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s is not a record list: %v", path, err)
	}
	return out
}

func hasClass(r canonicalRecord, class string) bool {
	for _, l := range r.Labels {
		if l.Class == class {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func labelString(r canonicalRecord) string {
	var parts []string
	for _, l := range r.Labels {
		parts = append(parts, l.Class)
	}
	return strings.Join(parts, ",")
}
