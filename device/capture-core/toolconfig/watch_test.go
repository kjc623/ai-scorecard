package toolconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/protocol"
)

// driftBudget is how soon a change made outside the agent must be reverted.
const driftBudget = 5 * time.Second

// lineLog keeps every line logged.
type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// matching is the logged lines that contain s.
func (l *lineLog) matching(s string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return out
}

// reapplied marks the line a re-apply logs.
const reapplied = "changed outside the agent"

// startWatcher runs a watcher until the test ends.
func startWatcher(t *testing.T, log core.Logger) *Watcher {
	t.Helper()
	w := NewWatcher(log)
	if w.fs == nil {
		t.Fatal("file change notifications did not start")
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		w.Run(stop)
		close(done)
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return w
}

// startWatched starts a provider over w for tool, with the bundle switching Claude Code's OTel
// export on at m1, registered with watcher, and stops it when the test ends (before the watcher).
func startWatched(t *testing.T, tl tool, w Writer, watcher *Watcher, log core.Logger) *Provider {
	t.Helper()
	p := newProvider(tl, w, Config{
		Token:      func() string { return testToken },
		Executable: func() (string, error) { return testExe, nil },
		Log:        log,
		Watcher:    watcher,
	})
	b := otelBundle(protocol.ModeM1)
	b.Endpoint.Tools[CopilotTool] = b.Endpoint.Tools["claude_code"]
	if err := p.ApplyPolicy(b); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	return p
}

// eventually waits up to the drift budget for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(driftBudget)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, driftBudget)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func holdsNow(w Writer, d Desired) func() bool {
	return func() bool {
		ok, err := w.Holds(d)
		return err == nil && ok
	}
}

func checkRow(t *testing.T, p *Provider, state protocol.CollectorState, detail protocol.Detail) {
	t.Helper()
	if h := p.Health(); h.State != state || h.Detail != detail {
		t.Fatalf("row = %s/%s, want %s/%s", h.State, h.Detail, state, detail)
	}
}

// dropEndpoint deletes OTEL_EXPORTER_OTLP_ENDPOINT from the managed file's env and saves the file
// in place, as an administrator's editor would.
func dropEndpoint(t *testing.T, path string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readFile(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["env"].(map[string]any), "OTEL_EXPORTER_OTLP_ENDPOINT")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// An edit to the managed file that takes out one of the agent's keys is reverted within the budget,
// the row reports the tamper, and the re-apply logs one line naming the tool and the file, without
// its content.
func TestDriftExternalEditIsReverted(t *testing.T) {
	log := &lineLog{}
	watcher := startWatcher(t, log)
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	p := startWatched(t, supportedTool, w, watcher, log)
	checkRow(t, p, protocol.StateHealthy, protocol.DetailNone)

	dropEndpoint(t, path)
	eventually(t, "the endpoint is put back", holdsNow(w, testDesired(true)))
	if env := envOf(t, path); env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:47318" || env["HTTPS_PROXY"] != "http://proxy.corp.example:8080" {
		t.Fatalf("env after the revert = %v", env)
	}
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)

	lines := log.matching(reapplied)
	if len(lines) != 1 {
		t.Fatalf("re-apply lines = %q, want one", lines)
	}
	if !strings.Contains(lines[0], ClaudeCodeTool) || !strings.Contains(lines[0], path) || strings.Contains(lines[0], testToken) || strings.Contains(lines[0], "OTEL_") {
		t.Fatalf("re-apply line %q: want the tool and the path, and none of the file", lines[0])
	}
}

// A managed file deleted and created again without the agent's keys is reverted, and so is a
// deleted folder; the folder's watch then comes back, so the next edit is reverted too.
func TestDriftDeleteAndRecreateIsReverted(t *testing.T) {
	watcher := startWatcher(t, nil)
	w, path := newTestWriter(t)
	p := startWatched(t, supportedTool, w, watcher, nil)
	want := testDesired(true)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	writeFile(t, path, []byte(customerFile))
	eventually(t, "the re-created file carries the agent's keys", holdsNow(w, want))
	if env := envOf(t, path); env["HTTPS_PROXY"] != "http://proxy.corp.example:8080" {
		t.Fatalf("the re-created file's own keys were not kept: %v", env)
	}
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)

	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the deleted folder's file is written again", holdsNow(w, want))

	dropEndpoint(t, path)
	eventually(t, "an edit after the folder came back is put back", holdsNow(w, want))
}

// The agent's own writes, a bundle's re-apply among them, are seen by the watcher and compare
// clean: nothing is re-applied and the row stays healthy.
func TestDriftOwnWriteIsNotTamper(t *testing.T) {
	log := &lineLog{}
	watcher := startWatcher(t, log)
	w, path := newTestWriter(t)
	p := startWatched(t, supportedTool, w, watcher, log)
	before := readFile(t, path)

	if err := p.ApplyPolicy(otelBundle(protocol.ModeM0)); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, path)) == string(before) {
		t.Fatal("the mode change did not rewrite the file")
	}
	time.Sleep(4 * watcher.debounce)
	checkRow(t, p, protocol.StateHealthy, protocol.DetailNone)
	if lines := log.matching(reapplied); len(lines) != 0 {
		t.Fatalf("the agent's own write was re-applied: %q", lines)
	}
}

// Copilot's registry values changed or deleted from outside are set back through the registry
// seam's change notification, and the agent's own writes notify too without flagging tamper.
func TestDriftRegistryValueIsReverted(t *testing.T) {
	log := &lineLog{}
	watcher := startWatcher(t, log)
	in := everywhere
	w, reg, _ := newCopilotTestWriter(t, &in)
	tl := copilotTool
	tl.supported = true
	p := startWatched(t, tl, w, watcher, log)
	want := copilotDesired(true, true)
	if ok, err := w.Holds(want); err != nil || !ok {
		t.Fatalf("Start did not write the values: %v, %v", ok, err)
	}
	eventually(t, "both keys are watched", func() bool {
		return reg.watching(vsCodePolicyKey) == 1 && reg.watching(machineEnvKey) == 1
	})
	time.Sleep(4 * watcher.debounce)
	checkRow(t, p, protocol.StateHealthy, protocol.DetailNone)
	if lines := log.matching(reapplied); len(lines) != 0 {
		t.Fatalf("the agent's own writes were re-applied: %q", lines)
	}

	if err := reg.set(vsCodePolicyKey, "CopilotOtelEndpoint", stringValue("https://otel.corp.example")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the policy value is put back", holdsNow(w, want))
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)

	if err := reg.remove(machineEnvKey, "OTEL_EXPORTER_OTLP_HEADERS"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the deleted variable is put back", holdsNow(w, want))
	checkValue(t, reg, machineEnvKey, "OTEL_EXPORTER_OTLP_HEADERS", stringValue(wantEnvHeaders))

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Stop ends the registry watches", func() bool {
		return reg.watching(vsCodePolicyKey) == 0 && reg.watching(machineEnvKey) == 0
	})
}

// The tamper is reported until a health report has carried it and a later comparison is clean; a
// clean comparison before the report does not clear it. A re-apply that fails counts an error,
// keeps the row tampered, and is tried again at the next comparison.
func TestDriftTamperIsReportedThenClears(t *testing.T) {
	w, path := newTestWriter(t)
	log := &lineLog{}
	p := startWatched(t, supportedTool, w, nil, log)
	want := testDesired(true)

	p.checkDrift()
	checkRow(t, p, protocol.StateHealthy, protocol.DetailNone)

	dropEndpoint(t, path)
	p.checkDrift()
	if ok, _ := w.Holds(want); !ok {
		t.Fatal("the comparison did not apply the agent's keys again")
	}
	p.checkDrift()
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)
	p.checkDrift()
	checkRow(t, p, protocol.StateHealthy, protocol.DetailNone)
	p.checkDrift()
	checkRow(t, p, protocol.StateHealthy, protocol.DetailNone)

	errors := func() uint64 { return p.Counters().Cumulative()[protocol.CounterErrors] }
	writeFile(t, path, []byte("not json"))
	p.checkDrift()
	if errors() != 1 {
		t.Fatalf("errors = %d after a failed re-apply, want 1", errors())
	}
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)
	writeFile(t, path, []byte(customerFile))
	p.checkDrift()
	if ok, _ := w.Holds(want); !ok {
		t.Fatal("the next comparison did not try the re-apply again")
	}
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)
	p.checkDrift()
	checkRow(t, p, protocol.StateHealthy, protocol.DetailNone)
	if n := len(log.matching(reapplied)); n != 3 {
		t.Fatalf("%d re-apply lines, want 3", n)
	}
}

// A failed write that no outside change caused waits for the next bundle: the comparisons leave it
// degraded and do not retry it.
func TestDriftLeavesAFailedApplyToTheBundle(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte("not json"))
	p := startWatched(t, supportedTool, w, nil, nil)
	checkRow(t, p, protocol.StateDegraded, protocol.DetailConfigWriteFailed)
	writeFile(t, path, []byte(customerFile))
	p.checkDrift()
	if ok, _ := w.Holds(testDesired(true)); ok {
		t.Fatal("a comparison retried a failed apply")
	}
	checkRow(t, p, protocol.StateDegraded, protocol.DetailConfigWriteFailed)
}

// Without file notifications, the backstop's comparison still reverts a change.
func TestDriftBackstopCatchesAMissedChange(t *testing.T) {
	watcher := NewWatcher(nil)
	if watcher.fs != nil {
		_ = watcher.fs.Close()
		watcher.fs = nil
	}
	watcher.backstop = 300 * time.Millisecond
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		watcher.Run(stop)
		close(done)
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	w, path := newTestWriter(t)
	p := startWatched(t, supportedTool, w, watcher, nil)
	dropEndpoint(t, path)
	eventually(t, "the backstop puts the endpoint back", holdsNow(w, testDesired(true)))
	checkRow(t, p, protocol.StateTampered, protocol.DetailConfigTampered)
}

// Codex's provider watches both of its files.
func TestDriftWatchesBothCodexFiles(t *testing.T) {
	files := NewCodexFiles(&Codex{path: filepath.Join("a", "requirements.toml")}, &CodexConfig{path: filepath.Join("a", "config.toml")})
	var w watchedFiles = files
	if got := w.watchedFiles(); len(got) != 2 || got[0] != files.requirements.Path() || got[1] != files.config.Path() {
		t.Fatalf("watched files = %q", got)
	}
}
