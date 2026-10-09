package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/shadow-ai-capture/device/capture-core/classifierlink"
	"github.com/shadow-ai-capture/device/capture-core/component"
	"github.com/shadow-ai-capture/device/capture-core/contentstore"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/enforce"
	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/capture-core/merge"
	"github.com/shadow-ai-capture/device/capture-core/otlp"
	"github.com/shadow-ai-capture/device/capture-core/otlp/normalizers"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/loopback"
	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/capture-core/toolconfig"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// A canary prompt goes through each of the five ways prompt text enters the agent, at m0 and at m1,
// with an AWS key beside it so classification has something to find:
//
//   - hooks: a `capture-core --hook test` process, built from the agent's source, hands a
//     hook_evaluate to the hook relay over the native endpoint;
//   - otel: an OTLP/HTTP log record shaped like Claude Code's user_prompt reaches the receiver;
//   - proxy: an intercepted request goes through proxy.tls to a fake upstream;
//   - extension: an extension observation frame reaches the native endpoint;
//   - loopback: a request to a local model server's port goes through the broker to a fake runtime.
//
// The rig is the service's graph built from the agent's packages: the providers in a registry
// started by the supervisor under one bundle, the hook and OTel prompts through the merge buffer,
// the real pipeline with the real classifier-host, the real spool and content store, and the real
// drain to a fake edge. The service's own frame dispatch and health.json writer live in its main
// package, which no other module can import; the rig repeats them over the same packages.
//
// Each path's canary is then searched for, as text, hex, base64 at each alignment and JSON \u
// escapes, in every envelope the edge received, the spool through its own reader and as stored,
// every log line, health.json, the health report the edge received, and the content store. Nothing
// may be found at either mode. At m1 each prompt envelope carries a digest and the credential label.
//
// The tools would not send prompt text at m0 (the extension sends no content, the tool's prompt
// logging is off); here they send it anyway, so what keeps it out is the agent's own mode gate.

const (
	privacyAWSKey = "AKIAIOSFODNN7EXAMPLE"
	privacyHook   = "hooks"
	privacyOTel   = "otel"
	privacyProxy  = "proxy"
	privacyExt    = "extension"
	privacyLoop   = "loopback"
)

// privacyPaths are the paths in table order, with the route each one's envelope names.
var privacyPaths = []struct {
	name  string
	route protocol.Route
}{
	{privacyHook, protocol.RouteToolHook},
	{privacyOTel, protocol.RouteToolOTel},
	{privacyProxy, protocol.RouteProxyTLS},
	{privacyExt, protocol.RouteExtWebRequest},
	{privacyLoop, protocol.RouteProxyLoopback},
}

// privacyCanary is a fresh SACCANARY-<uuid>.
func privacyCanary(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return "SACCANARY-" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// privacyPrompt is an ordinary prompt carrying the canary and an AWS key. It holds no character JSON
// escapes, so it can be placed in a JSON document as it is.
func privacyPrompt(canary string) string {
	return "Draft the release notes for project " + canary + " and remind the team that the build uses the AWS key " + privacyAWSKey + " for uploads."
}

// canaryForms is every encoding of canary searched for: the text, hex, standard and URL base64 at
// each byte alignment (the stretch of the encoding that depends only on the canary), and JSON \u
// escapes in either case.
func canaryForms(canary string) []string {
	forms := []string{canary, hex.EncodeToString([]byte(canary))}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		for pad := 0; pad < 3; pad++ {
			s := enc.EncodeToString(append(make([]byte, pad), canary...))
			forms = append(forms, s[(pad*8+5)/6:len(s)-4])
		}
	}
	var lower, upper strings.Builder
	for _, r := range canary {
		fmt.Fprintf(&lower, `\u%04x`, r)
		fmt.Fprintf(&upper, `\u%04X`, r)
	}
	forms = append(forms, lower.String(), upper.String())
	slices.Sort(forms)
	return slices.Compact(forms)
}

func privacyHits(b []byte, canary string) int {
	n := 0
	for _, f := range canaryForms(canary) {
		n += bytes.Count(b, []byte(f))
	}
	return n
}

// slogPrintf is the service's adapter from slog to the Printf logger the providers use.
type slogPrintf struct{ log *slog.Logger }

func (s slogPrintf) Printf(format string, args ...any) { s.log.Info(fmt.Sprintf(format, args...)) }

// privacyBundle switches every prompt path on at mode: the OTLP receiver and the hook relay for
// Claude Code, TLS inspection of the fake upstream, and the loopback broker on Ollama's port.
func privacyBundle(mode protocol.CollectionMode, otelHTTP, otelGRPC, tlsPort, heldPort, runtimePort int) *policy.Bundle {
	return &policy.Bundle{
		Version:         "PRIVACY-1",
		EffectiveAt:     time.Now().Add(-time.Hour),
		TenantDefault:   mode,
		ToolModes:       map[string]protocol.CollectionMode{},
		PopulationModes: map[string]protocol.CollectionMode{},
		DeviceModes:     map[string]protocol.CollectionMode{},
		ClassModes:      map[string]protocol.CollectionMode{},
		Endpoint: policy.EndpointPolicy{
			OTel: policy.EndpointOTel{
				Enabled:    true,
				HTTPListen: fmt.Sprintf("127.0.0.1:%d", otelHTTP),
				GRPCListen: fmt.Sprintf("127.0.0.1:%d", otelGRPC),
			},
			Hooks: policy.EndpointHooks{Enabled: true},
			Tools: map[string]policy.EndpointTool{"claude_code": {OTel: true, Hooks: true}},
		},
		Interception: policy.Interception{
			Enabled:     true,
			SeedHosts:   []string{"127.0.0.1"},
			Ports:       []int{tlsPort},
			ProxyCanary: fmt.Sprintf("127.0.0.1:%d", tlsPort),
		},
		Loopback: policy.LoopbackPolicy{Ports: []policy.LoopbackPort{{
			ToolFingerprint: toolconfig.OllamaFingerprint,
			Port:            heldPort,
			UpstreamPort:    runtimePort,
			PreflightPath:   "/api/version",
			Mode:            mode,
		}}},
	}
}

// bundleLoader is the supervisor's first step: the bundle in force is applied to the providers.
type bundleLoader struct {
	reg    *core.Registry
	bundle *policy.Bundle
	log    core.Logger
}

func (l bundleLoader) Load(context.Context) error {
	for _, r := range l.reg.ApplyPolicy(*l.bundle) {
		if r.Err != nil {
			l.log.Printf("provider %s could not apply the bundle: %v", r.Collector, r.Err)
		}
	}
	return nil
}

// privacySpool is the supervisor's view of a spool the rig opened, drained through the drain.
type privacySpool struct {
	sp *capturespool.Spool
	d  *drain.Drainer
}

func (s privacySpool) Open(context.Context) error { return nil }
func (s privacySpool) Stats() protocol.SpoolStats { return s.sp.Stats() }
func (s privacySpool) Close() error               { return nil }
func (s privacySpool) Drain(ctx context.Context, deadline time.Time) (core.DrainResult, error) {
	res, err := s.d.Drain(ctx, deadline)
	return core.DrainResult{Delivered: res.Delivered, Dropped: res.Rejected, Err: err}, err
}

// serveNative is the service's handling of one native endpoint connection: a hook_evaluate goes to
// the hook relay, and a browser relay's observation frames are recorded under the connection's
// person and answered with an ack or a refusal.
func serveNative(conn net.Conn, relay *hooks.Relay, pipe *core.Pipeline, person core.Person, log *slog.Logger) {
	payload, err := localipc.ReadFrame(conn)
	if err != nil {
		return
	}
	var first protocol.NativeMessage
	if json.Unmarshal(payload, &first) == nil && first.Type == protocol.TypeHookEvaluate {
		relay.Serve(conn, hostinfo.User{Account: "privacy"}, first)
		return
	}
	log.Info("native messaging: browser connected")
	for {
		if err := localipc.WriteFrame(conn, nativeAnswer(pipe, person, payload)); err != nil {
			return
		}
		if payload, err = localipc.ReadFrame(conn); err != nil {
			return
		}
	}
}

// nativeAnswer handles one extension frame as the service does.
func nativeAnswer(pipe *core.Pipeline, person core.Person, payload []byte) []byte {
	refuse := func(reason protocol.RefusalReason, format string, args ...any) []byte {
		body, _ := json.Marshal(protocol.Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)})
		out, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypeRefusal, Version: protocol.Version, Body: body})
		return out
	}
	var msg protocol.NativeMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return refuse(protocol.RefusalMalformed, "frame is not JSON: %v", err)
	}
	if msg.Type != protocol.TypeObservation {
		return refuse(protocol.RefusalUnknownType, "unknown message type %q", msg.Type)
	}
	var o protocol.ObservationMessage
	if err := json.Unmarshal(msg.Body, &o); err != nil {
		return refuse(protocol.RefusalMalformed, "observation body is not an ObservationMessage: %v", err)
	}
	if err := o.Validate(); err != nil {
		var r *protocol.RefusalError
		if errors.As(err, &r) {
			return refuse(r.Reason, "%s", r.Message)
		}
		return refuse(protocol.RefusalMalformed, "%v", err)
	}
	obs := core.Observation{
		Route:             o.Route,
		Kind:              protocol.KindPrompt,
		ToolFingerprint:   o.ToolFingerprint,
		OccurredAt:        o.OccurredAt,
		MonotonicOffsetMS: o.MonotonicOffsetMS,
		SizeBytes:         o.SizeBytes,
		Enforce:           enforce.Hook(pipe.Bundles, o.Route, o.ToolFingerprint, false),
		Extract: core.ExtractorFunc(func(p []byte, _ string) (string, []dedup.Attachment, error) {
			if len(p) == 0 {
				return "", nil, errors.New("native host: no payload to canonicalise")
			}
			return dedup.Decode(p), nil, nil
		}),
		ClientID: o.ClientID,
		OverCap:  o.OverCap,
		Person:   &person,
	}
	if o.HasContent && len(o.Content) > 0 {
		obs.Content = frameContent(o.Content)
	}
	out, err := pipe.Process(context.Background(), obs)
	if err != nil {
		return refuse(protocol.RefusalQueueFull, "observation refused: %v", err)
	}
	body, _ := json.Marshal(protocol.Ack{ID: msg.ID, Detail: fmt.Sprintf("event=%s mode=%s confidence=%s degraded=%v", out.EventID, out.Mode, out.Confidence, out.Degraded)})
	ack, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypeAck, Version: protocol.Version, ID: msg.ID, Body: body})
	return ack
}

// frameContent is the content reader for bytes that arrived in a frame.
type frameContent []byte

func (c frameContent) Read(context.Context) ([]byte, error) { return c, nil }

// privacyHealthFile is the service's health.json document, from the same sources.
type privacyHealthFile struct {
	DeviceID       string                  `json:"device_id,omitempty"`
	Enrolled       bool                    `json:"enrolled"`
	AgentVersion   string                  `json:"agent_version"`
	GeneratedAt    time.Time               `json:"generated_at"`
	CollectionMode string                  `json:"collection_mode,omitempty"`
	PolicyVersion  string                  `json:"policy_version,omitempty"`
	Classifier     protocol.HealthReport   `json:"classifier"`
	Reports        []protocol.HealthReport `json:"reports"`
	Spool          protocol.SpoolStats     `json:"spool"`
	ContentHeld    int                     `json:"content_held_objects"`
	Drain          struct {
		State       protocol.CollectorState `json:"state"`
		Detail      protocol.Detail         `json:"detail,omitempty"`
		LastSuccess time.Time               `json:"last_success_at"`
		NotAfter    time.Time               `json:"certificate_not_after"`
	} `json:"drain"`
	NativeClients  int        `json:"native_clients"`
	Heartbeats     int        `json:"heartbeats_published"`
	LastHeartbeat  *time.Time `json:"last_heartbeat_at,omitempty"`
	HeartbeatError string     `json:"last_heartbeat_error,omitempty"`
}

// buildHookExe builds capture-core with its hook dialling endpoint, the way the hook benchmark does.
func buildHookExe(t *testing.T, endpoint string) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is needed to build capture-core: %v", err)
	}
	exe := filepath.Join(t.TempDir(), "capture-core")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goTool, "build", "-trimpath", "-o", exe, "-ldflags", "-X main.hookBenchEndpoint="+endpoint, "./cmd/capture-core")
	build.Dir = filepath.Join("..", "capture-core")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build capture-core: %v\n%s", err, out)
	}
	return exe
}

type privacyRow struct {
	mode                                                    protocol.CollectionMode
	path                                                    string
	uploads, spool, logs, healthFile, healthReport, content int
}

func TestPrivacy_CanaryThroughEveryPathIsFoundNowhere(t *testing.T) {
	hostLog := &privacyLog{}
	cl, host := startClassifierHost(t, hostLog)
	endpoint := privacyEndpoint(t)
	hookExe := buildHookExe(t, endpoint)
	schema := deviceSubmissionSchema(t)

	var rows []privacyRow
	for _, mode := range []protocol.CollectionMode{protocol.ModeM0, protocol.ModeM1} {
		t.Run(string(mode), func(t *testing.T) {
			rig := privacyRig{t: t, mode: mode, cl: cl, host: host, hostLog: hostLog, endpoint: endpoint, hookExe: hookExe}
			rows = append(rows, rig.run(schema)...)
		})
	}

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "mode\tpath\tuploads\tspool\tlogs\thealth.json\thealth report\tcontent store")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\n", r.mode, r.path, r.uploads, r.spool, r.logs, r.healthFile, r.healthReport, r.content)
	}
	_ = w.Flush()
	t.Logf("canary hits per mode, path and location:\n%s", b.String())
}

type privacyRig struct {
	t        *testing.T
	mode     protocol.CollectionMode
	cl       *classifierlink.Client
	host     *component.Supervisor
	hostLog  *privacyLog
	endpoint string
	hookExe  string
}

func (rig privacyRig) run(schema *jsonschema.Schema) []privacyRow {
	t := rig.t
	ctx := context.Background()
	dir := t.TempDir()
	logs := &privacyLog{}
	slogger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logf := slogPrintf{slogger}

	canaries := map[string]string{}
	for _, p := range privacyPaths {
		canaries[p.name] = privacyCanary(t)
	}

	// The fake upstream the proxy intercepts, and the fake local model runtime behind the broker.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-privacy","object":"chat.completion","choices":[]}`))
	}))
	t.Cleanup(upstream.Close)
	upstreamRoots := x509.NewCertPool()
	upstreamRoots.AddCert(upstream.Certificate())
	tlsPort := upstream.Listener.Addr().(*net.TCPAddr).Port
	runtimeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"llama3.2","message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	t.Cleanup(runtimeSrv.Close)
	runtimePort := runtimeSrv.Listener.Addr().(*net.TCPAddr).Port
	heldPort := freeLoopbackPort(t)

	bundle := privacyBundle(rig.mode, freeLoopbackPort(t), freeLoopbackPort(t), tlsPort, heldPort, runtimePort)
	if err := bundle.Validate(); err != nil {
		t.Fatalf("the rig's bundle is not one a tenant could send: %v", err)
	}
	bundles := func() *policy.Bundle { return bundle }

	// The spool, the content store and the pipeline.
	spoolKey, contentKey := make([]byte, capturespool.KeySize), make([]byte, 32)
	_, _ = rand.Read(spoolKey)
	_, _ = rand.Read(contentKey)
	spoolDir := filepath.Join(dir, state.SpoolDir)
	sp, err := capturespool.Open(capturespool.Config{Dir: spoolDir, Key: spoolKey, SyncEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	contentDir := filepath.Join(dir, state.ContentDir)
	store, err := contentstore.Open(contentDir, contentKey, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(sp, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	pipe.SetIdentity(core.Identity{TenantID: testTenant, DeviceID: testDevice, UserRef: testUser})
	pipe.Bundles = bundles
	pipe.Classifier = rig.cl
	pipe.ClassifyBudget = 30 * time.Second
	pipe.Content = store
	pipe.Log = slogger

	// The drain to the fake edge, enrolled.
	deviceCA, deviceCAKey := mustCA(t, "privacy device CA")
	peer := &ingestPeer{clientRoot: x509.NewCertPool()}
	peer.clientRoot.AddCert(deviceCA)
	edge, caFile := startTLSIngest(t, peer)
	creds, err := credential.Open(filepath.Join(dir, state.CredentialFile), spoolKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Save(issuedCredential(t, deviceCA, deviceCAKey)); err != nil {
		t.Fatal(err)
	}
	drainer, err := drain.New(drain.Config{
		Endpoint: edge.URL, CAFile: caFile, TenantID: testTenant, AgentVersion: "integration",
		BackoffBase: time.Millisecond, BackoffCap: 50 * time.Millisecond, DrainInterval: time.Second, Content: store,
	}, func() (protocol.Store, error) { return sp, nil }, creds, logf, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	// The providers, as the service builds them.
	reg := core.NewRegistry(time.Now, logf)
	proxy := tlsproxy.New(tlsproxy.Config{
		Listen: "127.0.0.1:0", Bundles: bundles, Pipeline: pipe, Log: logf, Clock: time.Now,
		CanaryHost: "127.0.0.1", CanaryPort: tlsPort, UpstreamRoots: upstreamRoots,
	})
	broker := loopback.New(loopback.Config{Bundles: bundles, Pipeline: pipe, Log: logf, Clock: time.Now}.WithPolicy(bundle.Loopback))
	prompts := merge.New(merge.Config{Pipeline: pipe, Log: logf})
	otelCounters := core.NewCounterSet(time.Now())
	receiver, err := otlp.New(otlp.Config{
		TokenPath: filepath.Join(dir, otlp.TokenFile),
		Normalizers: normalizers.Registered(normalizers.Deps{
			Pipeline: prompts, Bundles: bundles, Counters: otelCounters, Log: logf, Clock: time.Now,
		}),
		Counters: otelCounters,
		Log:      logf,
		Clock:    time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	person := core.Person{UserRef: testUser}
	relay := hooks.New(hooks.Config{
		Pipeline: pipe, Prompts: prompts, Bundles: bundles, Classifier: rig.cl,
		Person: func(hostinfo.User) core.Person { return person },
		Log:    logf, Clock: time.Now,
	})
	for _, prov := range []core.Provider{proxy, broker, receiver, relay} {
		if err := reg.Add(prov); err != nil {
			t.Fatal(err)
		}
	}
	sup, err := core.NewSupervisor(reg, logf, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sup.Policy = bundleLoader{reg: reg, bundle: bundle, log: logf}
	sup.Spool = privacySpool{sp: sp, d: drainer}
	sup.Loopback = broker
	sup.DrainDeadline = 10 * time.Second
	if err := sup.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := false
	shutdown := func() {
		if !stopped {
			stopped = true
			prompts.Close()
			_ = sup.Shutdown(ctx)
		}
	}
	t.Cleanup(shutdown)
	native := localipc.NewServer(rig.endpoint, func(conn net.Conn, _ hostinfo.User) {
		serveNative(conn, relay, pipe, person, slogger)
	}, slogger)
	if err := native.Start(); err != nil {
		t.Fatalf("native endpoint %s: %v", rig.endpoint, err)
	}
	t.Cleanup(native.Stop)

	// Each path's canary prompt.
	hookStderr := rig.sendHook(privacyPrompt(canaries[privacyHook]))
	rig.sendOTel(receiver, privacyPrompt(canaries[privacyOTel]))
	rig.sendProxy(proxy, tlsPort, privacyPrompt(canaries[privacyProxy]))
	rig.sendExtension(privacyPrompt(canaries[privacyExt]))
	rig.sendLoopback(heldPort, privacyPrompt(canaries[privacyLoop]))

	// The buffer releases the prompts it holds for a partner, as at the service's stop; then every
	// path's envelope is in the spool.
	prompts.Close()
	entries := waitForPromptEntries(t, sp)

	rows := map[string]*privacyRow{}
	for _, p := range privacyPaths {
		rows[p.name] = &privacyRow{mode: rig.mode, path: p.name}
	}
	count := func(b []byte, into func(*privacyRow) *int) {
		for name, c := range canaries {
			*into(rows[name]) += privacyHits(b, c)
		}
	}
	spoolHits := func(r *privacyRow) *int { return &r.spool }
	contentHits := func(r *privacyRow) *int { return &r.content }

	// The spool through its own reader, then its files and the content store's as stored, and each
	// content object opened through the store.
	for _, e := range entries {
		count(e.Payload, spoolHits)
		e.Payload = nil
		meta, _ := json.Marshal(e)
		count(meta, spoolHits)
	}
	for _, d := range []string{spoolDir, contentDir} {
		err := filepath.WalkDir(d, func(path string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if d == spoolDir {
				count(raw, spoolHits)
				return nil
			}
			count(raw, contentHits)
			if id, ok := strings.CutSuffix(de.Name(), ".sealed"); ok {
				plain, err := store.Get(id)
				if err != nil {
					return err
				}
				count(plain, contentHits)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// The drain to the edge, one health report, and health.json.
	res, err := drainer.Drain(ctx, time.Now().Add(10*time.Second))
	if err != nil || res.Delivered != len(entries) {
		t.Fatalf("drain delivered %d of %d (%v)", res.Delivered, len(entries), err)
	}
	reports, errs := reg.Reports(testDevice, bundle.Version)
	for _, err := range errs {
		t.Errorf("a health row does not validate: %v", err)
	}
	classifierRow := rig.host.Health().Report(testDevice, rig.cl.ClassifierVersion())
	classifierRow.Collector = string(rig.host.Name())
	collectors := []protocol.HealthReport{}
	for _, r := range append(slices.Clone(reports), classifierRow) {
		r.DeviceID = ""
		collectors = append(collectors, r)
	}
	st := sp.Stats()
	_, heartbeatErr := drainer.ReportHealth(ctx, protocol.HealthRequest{
		SchemaVersion: protocol.HealthSchemaVersion, ReportedAt: time.Now().UTC(), AgentVersion: "integration",
		CollectionMode: string(rig.mode), PolicyBundleVersion: bundle.Version,
		Spool:      protocol.SpoolHealth{DepthEvents: int64(st.Depth), SpoolBytes: st.UsedBytes, DroppedTotal: st.DroppedTotal, RejectedTotal: st.RejectedTotal},
		Collectors: collectors,
	})
	if heartbeatErr != nil {
		t.Fatalf("health report: %v", heartbeatErr)
	}
	snap := privacyHealthFile{
		DeviceID: testDevice, Enrolled: true, AgentVersion: "integration", GeneratedAt: time.Now().UTC(),
		CollectionMode: string(rig.mode), PolicyVersion: bundle.Version,
		Classifier: classifierRow, Reports: reports, Spool: sp.Stats(), ContentHeld: store.HeldObjects(),
		NativeClients: native.Clients(), Heartbeats: 1,
	}
	ds := drainer.Status()
	snap.Drain.State, snap.Drain.Detail, snap.Drain.LastSuccess, snap.Drain.NotAfter = ds.State, ds.Detail, ds.LastSuccess, ds.NotAfter
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	healthPath := filepath.Join(dir, state.HealthFile)
	if err := state.WriteFile(healthPath, raw); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(healthPath)
	if err != nil {
		t.Fatal(err)
	}
	count(written, func(r *privacyRow) *int { return &r.healthFile })

	peer.mu.Lock()
	uploads, health := slices.Clone(peer.bodies), slices.Clone(peer.health)
	peer.mu.Unlock()
	if len(health) != 1 {
		t.Fatalf("the edge received %d health reports, want 1", len(health))
	}
	count(health[0], func(r *privacyRow) *int { return &r.healthReport })

	// The envelopes as uploaded: one prompt per path, each accepted as ingest accepts it.
	byRoute := map[protocol.Route][]map[string]any{}
	for _, b := range uploads {
		count(b, func(r *privacyRow) *int { return &r.uploads })
		var batch protocol.EventBatch
		if err := json.Unmarshal(b, &batch); err != nil {
			t.Fatal(err)
		}
		for _, ev := range batch.Events {
			if err := acceptLikeIngest(t, schema, ev); err != nil {
				t.Errorf("ingest would refuse the envelope: %v\n%s", err, ev)
			}
			var env map[string]any
			if err := json.Unmarshal(ev, &env); err != nil {
				t.Fatal(err)
			}
			if env["kind"] == string(protocol.KindPrompt) {
				route := protocol.Route(fmt.Sprint(env["source"]))
				byRoute[route] = append(byRoute[route], env)
			}
		}
	}

	// Every log line, after the shutdown sequence has logged too, and the hook process's stderr.
	shutdown()
	native.Stop()
	lines := slices.Concat(logs.bytes(), rig.hostLog.bytes(), hookStderr)
	for _, want := range []string{"envelope spooled", "drain:", "local endpoint listening", "native messaging: browser connected"} {
		if !bytes.Contains(lines, []byte(want)) {
			t.Fatalf("the captured log has no %q line, so it is not the components' log", want)
		}
	}
	count(lines, func(r *privacyRow) *int { return &r.logs })

	var out []privacyRow
	for _, p := range privacyPaths {
		r := rows[p.name]
		out = append(out, *r)
		if r.uploads+r.spool+r.logs+r.healthFile+r.healthReport+r.content != 0 {
			t.Errorf("DEFECT: the %s canary was found at %s: uploads %d, spool %d, logs %d, health.json %d, health report %d, content store %d",
				p.name, rig.mode, r.uploads, r.spool, r.logs, r.healthFile, r.healthReport, r.content)
		}
		envs := byRoute[p.route]
		if len(envs) != 1 {
			t.Errorf("the edge received %d %s prompt envelopes from the %s path, want 1", len(envs), p.route, p.name)
			continue
		}
		env := envs[0]
		digest, _ := env["content_digest"].(string)
		var classes []string
		labels, hasLabels := env["labels"].([]any)
		for _, l := range labels {
			if m, ok := l.(map[string]any); ok {
				classes = append(classes, fmt.Sprint(m["class"]))
			}
		}
		if rig.mode == protocol.ModeM0 {
			if digest != "" || hasLabels {
				t.Errorf("the m0 %s envelope carries content-derived fields: %v", p.name, env)
			}
			continue
		}
		if !strings.HasPrefix(digest, "sha256:") || !slices.Contains(classes, "credential") {
			t.Errorf("the m1 %s envelope must carry a digest and the credential label; it has digest %q and labels %v", p.name, digest, classes)
		}
	}
	return out
}

// sendHook runs the hook as a tool does, with the test adapter's input, and returns its stderr. The
// hook must print the service's decision, not the allow it prints when it fails open.
func (rig privacyRig) sendHook(prompt string) []byte {
	t := rig.t
	input, err := json.Marshal(map[string]string{"tool": "claude_code", "session_id": "privacy-hook-" + string(rig.mode), "prompt_text": prompt})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(rig.hookExe, "--hook", hooks.TestTool, "UserPromptSubmit")
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the hook exited with %v: %s", err, stderr.Bytes())
	}
	var d protocol.HookDecision
	if err := json.Unmarshal(stdout.Bytes(), &d); err != nil || d.RuleID == "" {
		t.Fatalf("the hook printed %q, not the service's decision", stdout.String())
	}
	return stderr.Bytes()
}

// sendOTel exports Claude Code's user_prompt log record, the documented fixture with the prompt in
// place of its canary mark.
func (rig privacyRig) sendOTel(receiver *otlp.Receiver, prompt string) {
	t := rig.t
	raw, err := os.ReadFile(filepath.Join("..", "capture-core", "otlp", "testdata", "privacy", "claude-code", "logs-user-prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(raw), otelCanaryMark, prompt)
	req, err := http.NewRequest(http.MethodPost, "http://"+receiver.HTTPAddr()+"/v1/logs", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+receiver.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the OTLP export answered %d", resp.StatusCode)
	}
}

// tunnelConn is a connection whose first bytes were already read into r.
type tunnelConn struct {
	net.Conn
	r *bufio.Reader
}

func (c tunnelConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// sendProxy posts an OpenAI chat request through the proxy to the fake upstream, as a client the
// CLI shim points at the proxy does.
func (rig privacyRig) sendProxy(proxy *tlsproxy.Provider, port int, prompt string) {
	t := rig.t
	raw, err := net.DialTimeout("tcp", proxy.ListenAddr(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	target := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT through the proxy: %v %v", resp, err)
	}
	conn := tls.Client(tunnelConn{Conn: raw, r: br}, &tls.Config{ServerName: "127.0.0.1", RootCAs: proxy.CA().Pool()})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("the proxy's TLS handshake: %v", err)
	}
	body := `{"model":"gpt-4.1","messages":[{"role":"user","content":"` + prompt + `"}]}`
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: api.openai.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	answer, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("the proxied request: %v", err)
	}
	_, _ = io.Copy(io.Discard, answer.Body)
	answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the proxied request answered %d", answer.StatusCode)
	}
}

// sendExtension sends an observation frame with the prompt as its content, as the browser's relay
// carries it, and expects the agent's ack.
func (rig privacyRig) sendExtension(prompt string) {
	t := rig.t
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialPrivacyEndpoint(ctx, rig.endpoint)
	if err != nil {
		t.Fatalf("dial the native endpoint: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	sum := sha256.Sum256([]byte(prompt))
	body, err := json.Marshal(protocol.ObservationMessage{
		ClientID: "privacy-ext-" + string(rig.mode), Route: protocol.RouteExtWebRequest,
		ToolFingerprint: "genai.web.chat.v1:chatgpt", OccurredAt: time.Now().UTC(),
		SizeBytes: int64(len(prompt)), HasContent: true, ContentDigest: "sha256:" + hex.EncodeToString(sum[:]),
		Content: []byte(prompt),
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := json.Marshal(protocol.NativeMessage{Type: protocol.TypeObservation, Version: protocol.Version, ID: "privacy-1", Body: body})
	if err := localipc.WriteFrame(conn, frame); err != nil {
		t.Fatal(err)
	}
	payload, err := localipc.ReadFrame(conn)
	if err != nil {
		t.Fatalf("no answer from the native endpoint: %v", err)
	}
	var answer protocol.NativeMessage
	if err := json.Unmarshal(payload, &answer); err != nil || answer.Type != protocol.TypeAck {
		t.Fatalf("the native endpoint answered %s", payload)
	}
}

// sendLoopback posts an Ollama chat request to the port the broker holds.
func (rig privacyRig) sendLoopback(port int, prompt string) {
	t := rig.t
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker does not hold %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	body := `{"model":"llama3.2","stream":false,"messages":[{"role":"user","content":"` + prompt + `"}]}`
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post("http://"+addr+"/api/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("the request through the broker: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the request through the broker answered %d", resp.StatusCode)
	}
}

// waitForPromptEntries waits until the spool holds a prompt from every path, and returns its
// entries.
func waitForPromptEntries(t *testing.T, sp *capturespool.Spool) []protocol.Entry {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		entries, err := sp.Peek(1000)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[protocol.Route]bool{}
		for _, e := range entries {
			var env struct {
				Kind   protocol.Kind  `json:"kind"`
				Source protocol.Route `json:"source"`
			}
			if json.Unmarshal(e.Payload, &env) == nil && env.Kind == protocol.KindPrompt {
				seen[env.Source] = true
			}
		}
		missing := []string{}
		for _, p := range privacyPaths {
			if !seen[p.route] {
				missing = append(missing, p.name)
			}
		}
		if len(missing) == 0 {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("the spool holds no prompt from %v", missing)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
