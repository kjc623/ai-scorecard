package hooks_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/classifierlink"
	"github.com/shadow-ai-capture/device/capture-core/component"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	testTenant  = "44444444-4444-4444-8444-444444444444"
	testDevice  = "aaaaaaaa-0000-7000-8000-000000000036"
	testUserRef = "u_hook_user"
	awsKey      = "AKIAIOSFODNN7EXAMPLE"
)

// memSink is the spool as the pipeline sees it. A non-nil gate holds each append until it is
// closed; err fails every append.
type memSink struct {
	mu      sync.Mutex
	entries []protocol.Entry
	err     error
	gate    chan struct{}
}

func (s *memSink) Append(e protocol.Entry) (protocol.Entry, error) {
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return protocol.Entry{}, s.err
	}
	e.Seq = uint64(len(s.entries) + 1)
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *memSink) Stats() protocol.SpoolStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocol.SpoolStats{Depth: len(s.entries)}
}

func (s *memSink) all() []protocol.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.Entry(nil), s.entries...)
}

// envelope is the part of a spooled envelope the tests read.
type envelope struct {
	Source          string             `json:"source"`
	ToolFingerprint string             `json:"tool_fingerprint"`
	Mode            string             `json:"collection_mode"`
	UserRef         string             `json:"user_ref"`
	SizeBytes       int64              `json:"size_bytes"`
	Labels          []protocol.Label   `json:"labels"`
	Decision        *protocol.Decision `json:"policy_decision"`
}

func decodeEnvelope(t *testing.T, e protocol.Entry) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

// testBundle switches the hooks on for claude_code (and off for cursor) at mode, with rules.
func testBundle(mode protocol.CollectionMode, rules ...policy.Rule) *policy.Bundle {
	return &policy.Bundle{
		Version:       "1",
		TenantDefault: mode,
		Endpoint: policy.EndpointPolicy{
			Hooks: policy.EndpointHooks{Enabled: true},
			Tools: map[string]policy.EndpointTool{"claude_code": {Hooks: true}, "cursor": {Hooks: false}},
		},
		Rules: rules,
	}
}

var blockCredentials = policy.Rule{
	RuleID: "block_credentials", Action: policy.RuleBlock,
	Match:   policy.RuleMatch{Labels: []string{"credential"}},
	Message: "Remove the credential and try again.", Link: "https://intranet.example/ai",
}

// newPipeline is an enrolled pipeline over sink with b in force.
func newPipeline(t *testing.T, sink core.Sink, b *policy.Bundle) *core.Pipeline {
	t.Helper()
	p, err := core.NewPipeline(sink, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Bundles = func() *policy.Bundle { return b }
	p.SetIdentity(core.Identity{TenantID: testTenant, DeviceID: testDevice, UserRef: "u_console"})
	return p
}

// newRelay is a started relay over pipe, attributing every hook to testUserRef. The pipeline
// classifies with the same classifier, as the service's does.
func newRelay(t *testing.T, pipe *core.Pipeline, classifier core.Classifier) *hooks.Relay {
	t.Helper()
	pipe.Classifier = classifier
	r := hooks.New(hooks.Config{
		Pipeline:   pipe,
		Bundles:    pipe.Bundles,
		Classifier: classifier,
		Person:     func(hostinfo.User) core.Person { return core.Person{UserRef: testUserRef} },
	})
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return r
}

// evaluateFrame is a hook_evaluate frame for a prompt to tool.
func evaluateFrame(t *testing.T, tool, prompt string) protocol.NativeMessage {
	t.Helper()
	ev := protocol.HookEvaluate{Tool: tool, Event: "prompt", SessionID: "session-1", PromptText: prompt}
	ev.CapPrompt()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.NativeMessage{Type: protocol.TypeHookEvaluate, Version: protocol.Version, ID: "1", Body: body}
}

// ask serves first on one end of a pipe and returns the frame the relay answered with. served
// closes once the relay has recorded the prompt.
func ask(t *testing.T, r *hooks.Relay, first protocol.NativeMessage) (answer protocol.NativeMessage, served <-chan struct{}) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		r.Serve(server, hostinfo.User{Account: "hook-user"}, first)
	}()
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	payload, err := localipc.ReadFrame(client)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	if err := json.Unmarshal(payload, &answer); err != nil {
		t.Fatal(err)
	}
	return answer, done
}

// decision is the hook_decision an answer carries.
func decision(t *testing.T, answer protocol.NativeMessage) protocol.HookDecision {
	t.Helper()
	if answer.Type != protocol.TypeHookDecision {
		t.Fatalf("the relay answered %s: %s", answer.Type, answer.Body)
	}
	var d protocol.HookDecision
	if err := json.Unmarshal(answer.Body, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func waitServed(t *testing.T, served <-chan struct{}) {
	t.Helper()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not finish recording")
	}
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) { l.t.Logf(format, args...) }

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// goBuild builds pkg in module dir into out.
func goBuild(t *testing.T, dir, pkg, out string, ldflags string) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is needed to build %s: %v", pkg, err)
	}
	args := []string{"build", "-trimpath", "-o", out}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	cmd := exec.Command(goTool, append(args, pkg)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
}

// classifierRelease is a classifier-host binary and a signed release of the shipped rules and
// model.
type classifierRelease struct {
	exe, dir, pubkey string
}

// buildClassifier builds classifier-host and signs a release with a fresh key, the way the
// packages are built.
func buildClassifier(t *testing.T, out string) classifierRelease {
	t.Helper()
	src := filepath.Join("..", "..", "classifier-host")
	rel := classifierRelease{exe: filepath.Join(out, exeName("classifier-host")), dir: filepath.Join(out, "classifier-release")}
	goBuild(t, src, "./cmd/classifier-host", rel.exe, "")
	signer := filepath.Join(t.TempDir(), exeName("classifier-release"))
	goBuild(t, src, "./cmd/classifier-release", signer, "")
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "release.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(signer,
		"--rules", filepath.Join(src, "rules", "default.json"),
		"--model", filepath.Join(src, "rules", "model.json"),
		"--key", keyFile, "--version", "hooks-test-1", "--out", rel.dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("sign the classifier release: %v\n%s", err, stderr.String())
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "pubkey="); ok {
			rel.pubkey = v
		}
	}
	if rel.pubkey == "" {
		t.Fatalf("classifier-release printed no pubkey: %s", b)
	}
	return rel
}

// startClassifier runs classifier-host under a component supervisor and connects to it over the
// classifier link, as the service does.
func startClassifier(t *testing.T, rel classifierRelease) (*classifierlink.Client, *component.Supervisor) {
	t.Helper()
	return superviseClassifier(t, rel.exe, "serve", "--release", rel.dir, "--pubkey", rel.pubkey, "--transport", "stdio")
}

// startSlowClassifier runs this test binary as a classifier host that answers each request after
// slowHostDelay, under a component supervisor.
func startSlowClassifier(t *testing.T) (*classifierlink.Client, *component.Supervisor) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return superviseClassifier(t, exe, slowHostArg)
}

// The test binary is also a fake classifier host: run with slowHostArg first, it serves the
// classifier protocol on its stdin and stdout, answering every request after slowHostDelay with
// the credential label, instead of running the tests.
const (
	slowHostArg   = "hooks-test-slow-classifier"
	slowHostDelay = 100 * time.Millisecond
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == slowHostArg {
		os.Exit(runSlowHost())
	}
	os.Exit(m.Run())
}

func runSlowHost() int {
	if _, err := protocol.ReadFrameChecked(os.Stdin); err != nil {
		return 1
	}
	hs, _ := json.Marshal(protocol.HandshakeResponse{OK: true, ClassifierVersion: "slow-1"})
	if err := protocol.WriteFrame(os.Stdout, hs); err != nil {
		return 1
	}
	answer, _ := json.Marshal(protocol.ClassifyResponse{
		Labels:            []protocol.Label{{Class: "credential", Score: 0.9, RuleID: "R_SLOW"}},
		ClassifierVersion: "slow-1",
		Confidence:        protocol.ConfidenceHigh,
	})
	for {
		if _, err := protocol.ReadFrameChecked(os.Stdin); err != nil {
			return 0
		}
		time.Sleep(slowHostDelay)
		if err := protocol.WriteFrame(os.Stdout, answer); err != nil {
			return 1
		}
	}
}

// superviseClassifier runs the classifier host exe under a component supervisor and connects to it
// over the classifier link.
func superviseClassifier(t *testing.T, exe string, args ...string) (*classifierlink.Client, *component.Supervisor) {
	t.Helper()
	var cl *classifierlink.Client
	host := component.New(component.Spec{
		Collector: protocol.CollectorClassifierHost,
		Path:      exe,
		Args:      args,
		Stdio:     true,
		Ready:     func(ctx context.Context) error { return cl.Connect(ctx) },
	}, testLogger{t})
	cl = classifierlink.NewWithDialer(host.Dial, "hooks-test", 2*time.Second)
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("start classifier-host: %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background()) })
	if h := host.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("classifier-host is %s/%s after its handshake, want healthy", h.State, h.Detail)
	}
	return cl, host
}
