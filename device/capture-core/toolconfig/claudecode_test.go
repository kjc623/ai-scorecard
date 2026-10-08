package toolconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testDesired(logPrompts bool) Desired {
	return Desired{OTel: true, HTTPListen: "127.0.0.1:47318", Token: testToken, LogPrompts: logPrompts}
}

// newTestWriter is a Claude Code writer over a managed file and a state directory in the test's
// temporary directory, with Claude Code installed.
func newTestWriter(t *testing.T) (*ClaudeCode, string) {
	t.Helper()
	root := t.TempDir()
	dir, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewClaudeCodeWriter(dir, filepath.Join(root, "ClaudeCode", "managed-settings.json"))
	w.installed = func() bool { return true }
	return w, w.Path()
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file the agent wrote may be read-only to the test's account; replacing it needs only the
	// folder's rights.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// envOf decodes the managed file's env object.
func envOf(t *testing.T, path string) map[string]string {
	t.Helper()
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(readFile(t, path), utf8BOM), &doc); err != nil {
		t.Fatalf("the managed file is not JSON: %v", err)
	}
	return doc.Env
}

// The documented environment variables, with the receiver's address and token and prompt logging
// as the mode asks.
func TestClaudeCodeEnvKeys(t *testing.T) {
	w, path := newTestWriter(t)
	if err := w.Apply(testDesired(true)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":  "http/protobuf",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://127.0.0.1:47318",
		"OTEL_EXPORTER_OTLP_HEADERS":   "Authorization=Bearer " + testToken,
		"OTEL_LOG_USER_PROMPTS":        "1",
		"OTEL_LOG_ASSISTANT_RESPONSES": "0",
	}
	got := envOf(t, path)
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, got[k], v)
		}
	}
	if ok, err := w.Holds(testDesired(true)); err != nil || !ok {
		t.Fatalf("Holds = %v, %v after Apply", ok, err)
	}
	if ok, _ := w.Holds(testDesired(false)); ok {
		t.Fatal("Holds says prompt logging is off while the file switches it on")
	}
}

const customerFile = `{
    "permissions": {"deny": ["Bash(curl:*)"], "ask": []},
    "env": {"HTTPS_PROXY": "http://proxy.corp.example:8080", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://otel.corp.example"},
    "companyAnnouncements": ["Use the <approved> & audited tools"],
    "model": "claude-sonnet"
}
`

// The agent's keys merge into the env object; every other key, and the customer's own env keys,
// keep their values and their order.
func TestClaudeCodeMergesIntoAnExistingFile(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	if err := w.Apply(testDesired(false)); err != nil {
		t.Fatal(err)
	}
	got, err := parseObject(readFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := parseObject([]byte(customerFile))
	var keys []string
	for _, m := range got.members {
		keys = append(keys, m.key)
	}
	if strings.Join(keys, ",") != "permissions,env,companyAnnouncements,model" {
		t.Fatalf("top-level keys = %v, want the customer's, in order", keys)
	}
	for _, k := range []string{"permissions", "companyAnnouncements", "model"} {
		a, _ := got.get(k)
		b, _ := before.get(k)
		var ca, cb bytes.Buffer
		_ = json.Compact(&ca, a)
		_ = json.Compact(&cb, b)
		if ca.String() != cb.String() {
			t.Errorf("%s = %s, want %s", k, ca.String(), cb.String())
		}
	}
	env := envOf(t, path)
	if env["HTTPS_PROXY"] != "http://proxy.corp.example:8080" {
		t.Errorf("the customer's HTTPS_PROXY = %q", env["HTTPS_PROXY"])
	}
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:47318" || env["OTEL_LOG_USER_PROMPTS"] != "0" {
		t.Errorf("env = %v, want the agent's endpoint and prompt logging off", env)
	}
}

// The backup is the file as it was before the agent's first write; later writes do not replace it.
func TestClaudeCodeBackupIsTakenOnce(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	if err := w.Apply(testDesired(false)); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, w.backup.path)
	if err := state.CheckFile(w.backup.path); err != nil {
		t.Errorf("the backup is not protected: %v", err)
	}
	if err := w.Apply(testDesired(true)); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(Desired{OTel: true, HTTPListen: "127.0.0.1:50000", Token: testToken}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, w.backup.path), first) {
		t.Fatal("a later write replaced the backup")
	}
	o, ok, err := w.backup.load()
	if err != nil || !ok || !o.Present || string(o.Content) != customerFile {
		t.Fatalf("backup = %+v, %v, %v; want the customer's file", o, ok, err)
	}
}

// With nothing else changed, Remove puts the file back byte for byte, the customer's value for an
// agent key included, and drops the backup.
func TestClaudeCodeRemoveRestoresByteForByte(t *testing.T) {
	for name, content := range map[string][]byte{
		"customer file":   []byte(customerFile),
		"byte order mark": append(append([]byte(nil), utf8BOM...), []byte("{\r\n  \"model\": \"claude-opus\"\r\n}\r\n")...),
		"empty env":       []byte(`{"env":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			w, path := newTestWriter(t)
			writeFile(t, path, content)
			if err := w.Apply(testDesired(true)); err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(readFile(t, path), content) {
				t.Fatal("Apply did not change the file")
			}
			if err := w.Remove(); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, path); !bytes.Equal(got, content) {
				t.Fatalf("after Remove the file is\n%q\nwant\n%q", got, content)
			}
			if _, ok, _ := w.backup.load(); ok {
				t.Fatal("the backup is still kept after the file was restored")
			}
		})
	}
}

// A file the agent created is deleted by Remove when nothing else is in it, and kept, without the
// agent's keys, when something else was added.
func TestClaudeCodeRemoveOfAFileThatWasAbsent(t *testing.T) {
	w, path := newTestWriter(t)
	if err := w.Apply(testDesired(false)); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the managed file is still there (%v)", err)
	}

	if err := w.Apply(testDesired(false)); err != nil {
		t.Fatal(err)
	}
	doc, _ := parseObject(readFile(t, path))
	doc.set("model", json.RawMessage(`"claude-opus"`))
	out, _ := doc.indented()
	writeFile(t, path, out)
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(readFile(t, path))); got != "{\n  \"model\": \"claude-opus\"\n}" {
		t.Fatalf("after Remove the file is %q, want only the added key", got)
	}
}

// Remove takes out only the agent's keys and restores the customer's value for one it replaced;
// what someone else changed in the meantime stays.
func TestClaudeCodeRemoveKeepsOtherChanges(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	if err := w.Apply(testDesired(true)); err != nil {
		t.Fatal(err)
	}
	doc, _ := parseObject(readFile(t, path))
	doc.set("model", json.RawMessage(`"claude-opus"`))
	s := settings{doc: doc}
	env, _ := s.env()
	env.set("NO_PROXY", json.RawMessage(`"localhost"`))
	e, _ := env.compact()
	doc.set("env", e)
	out, _ := doc.indented()
	writeFile(t, path, out)

	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	got := envOf(t, path)
	want := map[string]string{
		"HTTPS_PROXY":                 "http://proxy.corp.example:8080",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otel.corp.example",
		"NO_PROXY":                    "localhost",
	}
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, got[k], v)
		}
	}
	after, _ := parseObject(readFile(t, path))
	if m, _ := after.get("model"); string(m) != `"claude-opus"` {
		t.Errorf("model = %s, want the change made after the agent's write", m)
	}
}

// A file Claude Code itself could not read is left untouched, and no backup is taken.
func TestClaudeCodeLeavesAFileThatIsNotAnObject(t *testing.T) {
	for name, content := range map[string]string{
		"array":            `[1, 2]`,
		"broken":           `{"env": {`,
		"env not object":   `{"env": ["A=1"]}`,
		"hooks not object": `{"hooks": [{"hooks": []}]}`,
		"event not array":  `{"hooks": {"PreToolUse": {"matcher": "Bash"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			w, path := newTestWriter(t)
			writeFile(t, path, []byte(content))
			if err := w.Apply(testDesired(false)); err == nil {
				t.Fatal("Apply succeeded over a file that is not a settings object")
			}
			if got := string(readFile(t, path)); got != content {
				t.Fatalf("the file changed to %q", got)
			}
			if _, ok, _ := w.backup.load(); ok {
				t.Fatal("a backup was taken of a file the agent did not write")
			}
		})
	}
}

// Remove without a backup changes nothing: the agent never wrote the file.
func TestClaudeCodeRemoveWithoutBackupChangesNothing(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != customerFile {
		t.Fatalf("the file changed to %q", got)
	}
}
