// Package appcapture replays one prompt per app in the console's app catalogue, other than the
// browser web apps the extension's own replay covers, through the agent's code for the route that
// app's prompts take, and checks the outcome against the app's fixture in testdata/.
//
// A fixture holds what the app sends when a person submits a prompt (a request body on the TLS
// proxy or the loopback broker, an OTLP export, or a hook's stdin), the text the person typed, and
// three facts the replay must reproduce exactly:
//
//	captured    the route records the prompt with content: the request is decrypted or brokered,
//	            and the content is the extracted text or, where none is found, the body as sent
//	attributed  every record's tool fingerprint is catalogued under the fixture's app_key
//	prompt      every record's extracted text is exactly what the person typed
//
// The check is exact in both directions: a change that breaks an app fails here, and so does one
// that fixes it, until its fixture says so. `go test -v -run TestAppCapture` logs the results as a
// table.
package appcapture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/otlp/normalizers"
	"github.com/shadow-ai-capture/device/capture-core/parsers/targets"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/capture-core/toolconfig"
	"github.com/shadow-ai-capture/device/protocol"
)

// moduleRoot is what a fixture's file paths are relative to.
const moduleRoot = ".."

type fixture struct {
	file   string
	AppKey string `json:"app_key"`
	Route  string `json:"route"`
	Source struct {
		Kind    string `json:"kind"`
		From    string `json:"from"`
		Checked string `json:"checked"`
	} `json:"source"`
	Typed    string         `json:"typed"`
	TLS      *tlsInput      `json:"tls"`
	OTLP     *otlpInput     `json:"otlp"`
	Hook     *hookInput     `json:"hook"`
	Loopback *loopbackInput `json:"loopback"`
	Expect   struct {
		Captured   *bool  `json:"captured"`
		Attributed *bool  `json:"attributed"`
		Prompt     *bool  `json:"prompt"`
		Why        string `json:"why"`
	} `json:"expect"`
}

// tlsInput is a request through the TLS proxy, from Process (the sending executable's file name).
type tlsInput struct {
	Process   string `json:"process"`
	Host      string `json:"host"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	body
}

// otlpInput is an OTLP/JSON export request of Signal (logs or traces), sent by the executable at
// Sender. Each placeholder in the export is replaced by its value.
type otlpInput struct {
	Signal       string            `json:"signal"`
	Export       json.RawMessage   `json:"export"`
	ExportFile   string            `json:"export_file"`
	Placeholders map[string]string `json:"placeholders"`
	Sender       string            `json:"sender"`
}

// hookInput is what a tool writes to `capture-core --hook <tool> <event>` on stdin.
type hookInput struct {
	Tool      string          `json:"tool"`
	Event     string          `json:"event"`
	Stdin     json.RawMessage `json:"stdin"`
	StdinFile string          `json:"stdin_file"`
}

// loopbackInput is a request to a local model server on Port.
type loopbackInput struct {
	Port      int    `json:"port"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	body
}

// body is a request body: inline JSON, a file, or base64 bytes.
type body struct {
	Body       json.RawMessage `json:"body"`
	BodyFile   string          `json:"body_file"`
	BodyBase64 string          `json:"body_base64"`
}

func (b body) bytes() ([]byte, error) {
	switch {
	case len(b.Body) > 0 && b.BodyFile == "" && b.BodyBase64 == "":
		return b.Body, nil
	case len(b.Body) == 0 && b.BodyFile != "" && b.BodyBase64 == "":
		return os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(b.BodyFile)))
	case len(b.Body) == 0 && b.BodyFile == "" && b.BodyBase64 != "":
		return base64.StdEncoding.DecodeString(b.BodyBase64)
	}
	return nil, errors.New("give exactly one of body, body_file and body_base64")
}

func inlineOrFile(inline json.RawMessage, file string) ([]byte, error) {
	if (len(inline) > 0) == (file != "") {
		return nil, errors.New("give the input inline or as a file, not both or neither")
	}
	if file != "" {
		return os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(file)))
	}
	return inline, nil
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	var out []fixture
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx fixture
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		fx.file = filepath.Base(f)
		out = append(out, fx)
	}
	if len(out) == 0 {
		t.Fatal("no fixtures in testdata")
	}
	return out
}

// record is one prompt record a route made: its tool fingerprint, the content the pipeline reads
// and what the route's extraction makes of it.
type record struct {
	fingerprint string
	content     []byte
	text        string
	err         error
}

// replayed is what a route did with one fixture's input. gate says why it recorded nothing.
type replayed struct {
	records []record
	gate    string
}

// facts are the three facts a fixture states.
type facts struct{ Captured, Attributed, Prompt bool }

func (r replayed) facts(c catalogue, fx fixture) facts {
	f := facts{Captured: len(r.records) > 0}
	for _, rec := range r.records {
		f.Captured = f.Captured && len(rec.content) > 0
	}
	f.Attributed, f.Prompt = f.Captured, f.Captured
	for _, rec := range r.records {
		f.Attributed = f.Attributed && c.app(rec.fingerprint) == fx.AppKey
		f.Prompt = f.Prompt && rec.err == nil && rec.text == fx.Typed
	}
	return f
}

func (r replayed) describe(c catalogue) string {
	if len(r.records) == 0 {
		return "nothing recorded: " + r.gate
	}
	var parts []string
	for _, rec := range r.records {
		named := c.app(rec.fingerprint)
		if named == "" {
			named = "not catalogued"
		}
		text := fmt.Sprintf("%q", clip(rec.text))
		if rec.err != nil {
			text = fmt.Sprintf("no text (%v), content %q", rec.err, clip(string(rec.content)))
		}
		parts = append(parts, fmt.Sprintf("%s (%s): %s", rec.fingerprint, named, text))
	}
	return strings.Join(parts, "; ")
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func replay(ctx context.Context, b *policy.Bundle, fx fixture) (replayed, error) {
	switch fx.Route {
	case "tls":
		return replayTLS(b, fx.TLS)
	case "otlp":
		return replayOTLP(ctx, b, fx.OTLP)
	case "hook":
		return replayHook(ctx, b, fx.Hook)
	case "loopback":
		return replayLoopback(b, fx.Loopback)
	case "none":
		return replayed{gate: "the app's prompts take no route the agent observes"}, nil
	}
	return replayed{}, fmt.Errorf("route %q", fx.Route)
}

// replayTLS takes the TLS proxy's decisions for a CONNECT to the host from the process: a host
// outside the interception set, and an app whose native collector is on, are tunnelled blind;
// anything else is decrypted, named as the proxy names it, and its body extracted by the
// destination's parser.
func replayTLS(b *policy.Bundle, in *tlsInput) (replayed, error) {
	body, err := in.bytes()
	if err != nil {
		return replayed{}, err
	}
	if !b.Intercepts(in.Host, 443) {
		return replayed{gate: in.Host + " is not in the interception set, so it is tunnelled blind"}, nil
	}
	if app, ok := appByExe(b, in.Process); ok && toolconfig.NativelyCovered(b, app) {
		return replayed{gate: fmt.Sprintf("%s is %s, whose native collector is on, so it is tunnelled blind", in.Process, app)}, nil
	}
	text, _, xerr := targets.Registry().For(in.Host, in.Path).Extract(body, in.MediaType)
	tool := tlsproxy.Tool(b, func(base string) (string, bool) { return appByExe(b, base) }, in.Process, in.Host, in.Path)
	return replayed{records: []record{{tool, body, text, xerr}}}, nil
}

// replayLoopback takes the loopback broker's view of a request to a local port: only a port the
// bundle's loopback section holds is brokered, under that entry's tool fingerprint.
func replayLoopback(b *policy.Bundle, in *loopbackInput) (replayed, error) {
	body, err := in.bytes()
	if err != nil {
		return replayed{}, err
	}
	i := slices.IndexFunc(b.Loopback.Ports, func(p policy.LoopbackPort) bool { return p.Port == in.Port })
	if i < 0 {
		return replayed{gate: fmt.Sprintf("the bundle's loopback section holds no port %d, so the request reaches the server directly", in.Port)}, nil
	}
	host := fmt.Sprintf("127.0.0.1:%d", in.Port)
	text, _, xerr := targets.Registry().For(host, in.Path).Extract(body, in.MediaType)
	return replayed{records: []record{{b.Loopback.Ports[i].ToolFingerprint, body, text, xerr}}}, nil
}

// replayOTLP hands each resource of the export to the first registered normalizer that accepts its
// service.name, as the receiver does.
func replayOTLP(ctx context.Context, b *policy.Bundle, in *otlpInput) (replayed, error) {
	raw, err := inlineOrFile(in.Export, in.ExportFile)
	if err != nil {
		return replayed{}, err
	}
	// A placeholder sits inside a JSON string, so its value goes in escaped and without quotes.
	for k, v := range in.Placeholders {
		value, _ := json.Marshal(v)
		raw = bytes.ReplaceAll(raw, []byte(k), value[1:len(value)-1])
	}
	pipe := &pipeline{}
	norms := normalizers.Registered(normalizers.Deps{
		Pipeline: pipe,
		Bundles:  func() *policy.Bundle { return b },
		Counters: core.NewCounterSet(time.Now()),
		AppByExe: func(base string) (string, bool) { return appByExe(b, base) },
		Clock:    time.Now,
	})
	normalizerFor := func(r *resourcepb.Resource) otlp.Normalizer {
		var service string
		for _, kv := range r.GetAttributes() {
			if kv.GetKey() == "service.name" {
				service = kv.GetValue().GetStringValue()
			}
		}
		for _, n := range norms {
			if n.Accepts(service) {
				return n
			}
		}
		return nil
	}
	from := otlp.Sender{PID: 4242, Image: in.Sender, Person: &core.Person{UserRef: replayUser}, Resolved: true}
	switch in.Signal {
	case "logs":
		var req collogspb.ExportLogsServiceRequest
		if err := protojson.Unmarshal(raw, &req); err != nil {
			return replayed{}, err
		}
		for _, rl := range req.GetResourceLogs() {
			if n := normalizerFor(rl.GetResource()); n != nil {
				n.Logs(ctx, from, rl)
			}
		}
	case "traces":
		var req coltracepb.ExportTraceServiceRequest
		if err := protojson.Unmarshal(raw, &req); err != nil {
			return replayed{}, err
		}
		for _, rs := range req.GetResourceSpans() {
			if n := normalizerFor(rs.GetResource()); n != nil {
				n.Spans(ctx, from, rs)
			}
		}
	default:
		return replayed{}, fmt.Errorf("signal %q is neither logs nor traces", in.Signal)
	}
	return pipe.replayed(ctx, "the export carries no prompt the normalizer records")
}

// replayHook runs the tool's hook adapter over its stdin, as `capture-core --hook` does, and hands
// the hook_evaluate it sends to the relay.
func replayHook(ctx context.Context, b *policy.Bundle, in *hookInput) (replayed, error) {
	stdin, err := inlineOrFile(in.Stdin, in.StdinFile)
	if err != nil {
		return replayed{}, err
	}
	adapter, ok := hooks.Lookup(in.Tool)
	if !ok {
		return replayed{gate: "no hook adapter for " + in.Tool}, nil
	}
	ev, err := adapter.Parse(in.Event, stdin)
	if err != nil {
		return replayed{gate: "the adapter refused the input: " + err.Error()}, nil
	}
	ev.CapPrompt()
	frame, err := json.Marshal(ev)
	if err != nil {
		return replayed{}, err
	}
	pipe := &pipeline{}
	relay := hooks.New(hooks.Config{Pipeline: pipe, Bundles: func() *policy.Bundle { return b }})
	if err := relay.Start(ctx); err != nil {
		return replayed{}, err
	}
	hook, service := net.Pipe()
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_, _ = localipc.ReadFrame(hook)
	}()
	relay.Serve(service, hostinfo.User{}, protocol.NativeMessage{Type: protocol.TypeHookEvaluate, Version: protocol.Version, ID: "replay-1", Body: frame})
	<-answered
	_ = hook.Close()
	_ = service.Close()
	_ = relay.Stop(ctx)
	return pipe.replayed(ctx, "the relay recorded nothing for "+in.Tool)
}

const (
	replayTenant = "44444444-4444-4444-8444-444444444444"
	replayDevice = "aaaaaaaa-0000-7000-8000-000000000036"
	replayUser   = "u_appcapture"
)

// pipeline stands in for core.Pipeline at m3: it keeps every prompt observation a route hands it.
type pipeline struct {
	mu  sync.Mutex
	obs []core.Observation
}

func (p *pipeline) Identity() (core.Identity, bool) {
	return core.Identity{TenantID: replayTenant, DeviceID: replayDevice, UserRef: replayUser}, true
}

func (p *pipeline) ResolveMode(core.ScopeQuery) core.Resolution {
	return core.Resolution{Mode: protocol.ModeM3}
}

func (p *pipeline) Counters(protocol.Route) *core.CounterSet { return core.NewCounterSet(time.Now()) }

func (p *pipeline) Record(context.Context, core.Fact) error { return nil }

func (p *pipeline) Process(_ context.Context, obs core.Observation) (core.Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if obs.Kind == protocol.KindPrompt {
		p.obs = append(p.obs, obs)
	}
	return core.Outcome{Route: obs.Route, Mode: protocol.ModeM3, Emitted: true, Reason: core.ReasonEmitted}, nil
}

// replayed reads each observation's content and extracts its text, as the pipeline does at m3.
func (p *pipeline) replayed(ctx context.Context, gate string) (replayed, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := replayed{gate: gate}
	for _, obs := range p.obs {
		rec := record{fingerprint: obs.ToolFingerprint}
		if obs.Content != nil {
			content, err := obs.Content.Read(ctx)
			if err != nil {
				return replayed{}, err
			}
			rec.content = content
		}
		if obs.Extract == nil {
			rec.err = errors.New("the route gives no extractor")
		} else {
			rec.text, _, rec.err = obs.Extract.Extract(rec.content, obs.MediaType)
		}
		out.records = append(out.records, rec)
	}
	return out, nil
}

var routes = []string{"tls", "otlp", "hook", "loopback", "none"}

// checkFields reports what a fixture is missing.
func checkFields(c catalogue, fx fixture) []string {
	var missing []string
	if !slices.ContainsFunc(c.apps, func(a policy.CatalogApp) bool { return a.AppKey == fx.AppKey }) {
		missing = append(missing, fmt.Sprintf("app_key %q is not in ref.app", fx.AppKey))
	}
	if c.webApps[fx.AppKey] {
		missing = append(missing, fmt.Sprintf("%s is a web app, which the extension's replay covers", fx.AppKey))
	}
	if !slices.Contains(routes, fx.Route) {
		missing = append(missing, fmt.Sprintf("route %q is not one of %v", fx.Route, routes))
	}
	if fx.Source.Kind != "documented" && fx.Source.Kind != "captured" {
		missing = append(missing, "source.kind (documented or captured)")
	}
	if fx.Source.From == "" {
		missing = append(missing, "source.from")
	}
	if _, err := time.Parse(time.DateOnly, fx.Source.Checked); err != nil {
		missing = append(missing, "source.checked (YYYY-MM-DD)")
	}
	if fx.Typed == "" {
		missing = append(missing, "typed")
	}
	inputs := map[string]bool{"tls": fx.TLS != nil, "otlp": fx.OTLP != nil, "hook": fx.Hook != nil, "loopback": fx.Loopback != nil}
	for route, given := range inputs {
		switch {
		case given && route != fx.Route:
			missing = append(missing, fmt.Sprintf("no %s input, since the route is %s", route, fx.Route))
		case !given && route == fx.Route:
			missing = append(missing, "the "+route+" input")
		}
	}
	e := fx.Expect
	if e.Captured == nil || e.Attributed == nil || e.Prompt == nil {
		missing = append(missing, "expect.captured, expect.attributed and expect.prompt")
	} else if (!*e.Captured || !*e.Attributed || !*e.Prompt) && e.Why == "" {
		missing = append(missing, "expect.why, since the app is not fully captured")
	}
	return missing
}

func TestEveryNonWebAppHasAFixture(t *testing.T) {
	c := loadCatalogue(t)
	covered := map[string]bool{}
	for _, fx := range loadFixtures(t) {
		covered[fx.AppKey] = true
	}
	for _, a := range c.apps {
		if !c.webApps[a.AppKey] && !covered[a.AppKey] {
			t.Errorf("%s has no fixture in appcapture/testdata", a.AppKey)
		}
	}
}

func TestAppCapture(t *testing.T) {
	c := loadCatalogue(t)
	b := c.bundle(t)
	ctx := context.Background()
	var table strings.Builder
	table.WriteString("\n| app | fixture | route | captured | attributed | prompt | why |\n|---|---|---|---|---|---|---|\n")
	for _, fx := range loadFixtures(t) {
		t.Run(strings.TrimSuffix(fx.file, ".json"), func(t *testing.T) {
			if missing := checkFields(c, fx); len(missing) > 0 {
				t.Fatalf("%s: %s", fx.file, strings.Join(missing, "; "))
			}
			got, err := replay(ctx, b, fx)
			if err != nil {
				t.Fatalf("%s: %v", fx.file, err)
			}
			f := got.facts(c, fx)
			t.Log(got.describe(c))
			want := facts{*fx.Expect.Captured, *fx.Expect.Attributed, *fx.Expect.Prompt}
			if f != want {
				t.Errorf("%s: got %+v, the fixture says %+v; %s", fx.file, f, want, got.describe(c))
			}
			fmt.Fprintf(&table, "| %s | %s | %s | %s | %s | %s | %s |\n", fx.AppKey, strings.TrimSuffix(fx.file, ".json"),
				fx.Route, yes(f.Captured), yes(f.Attributed), yes(f.Prompt), strings.ReplaceAll(fx.Expect.Why, "|", `\|`))
		})
	}
	t.Log(table.String())
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
