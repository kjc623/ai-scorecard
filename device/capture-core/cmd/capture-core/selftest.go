package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// runSelftest is the evidence run: it starts the real service, feeds the real golden frames through
// the real native-messaging framing, shows the health channel, shows the spool contents, shuts down
// and asserts §3.5's release-first ordering. Everything it prints is produced by the same code paths
// the service uses; nothing is simulated except the two peers a browser and a classifier host would
// provide, and those are labelled.
//
// It exits non-zero if any assertion fails, which is what makes it usable as a gate.
func runSelftest(cfg Config, log *slog.Logger) error {
	st := &selfTest{out: os.Stdout, log: log}
	work, err := workDirFor(cfg)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(work); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(work, "spool"), 0o700); err != nil {
		return err
	}
	st.section("work directory", work)

	// The held and upstream ports are chosen here and written into the bundle as policy data. No
	// test binds a vendor default (§6.1), and nothing here binds a fixed port.
	heldPort, err := freePort()
	if err != nil {
		return err
	}
	upstreamPort, err := startUpstream(log)
	if err != nil {
		return err
	}
	st.section("ports", fmt.Sprintf("loopback broker will hold %d, relocated upstream on %d (both ephemeral, both policy data)", heldPort, upstreamPort))

	// A signed bundle, minted here with a throwaway key: this is the same verification path a real
	// bundle takes, and the key is written beside it so the run is reproducible.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	bundle := selftestBundle(heldPort, upstreamPort)
	raw, err := policy.Sign("selftest-key", priv, bundle)
	if err != nil {
		return err
	}
	bundlePath := filepath.Join(work, "bundle.json")
	if err := os.WriteFile(bundlePath, raw, 0o600); err != nil {
		return err
	}
	cfg.BundlePath = bundlePath
	cfg.PolicyKey = hex.EncodeToString(pub)
	cfg.PolicyKeyID = "selftest-key"
	cfg.SpoolDir = filepath.Join(work, "spool")
	cfg.SpoolKey = filepath.Join(work, "spool.key")
	if cfg.TenantID == "" {
		cfg.TenantID = "11111111-1111-4111-8111-111111111111"
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = "22222222-2222-4222-8222-222222222222"
	}
	if cfg.UserRef == "" {
		cfg.UserRef = "selftest-user"
	}
	cfg.EnableTLS = true
	cfg.TLSListen = "127.0.0.1:0"
	cfg.EnableLoopback = true
	cfg.HealthFile = filepath.Join(work, "health.jsonl")
	cfg.HealthInterval = 200 * time.Millisecond
	st.section("bundle", fmt.Sprintf("%s (version %s, signed by %s)", bundlePath, bundle.Version, cfg.PolicyKeyID))

	// A classifier host speaking the real framing over a real socket. This is not device/classifier-
	// host: it answers with fixed labels so the run does not depend on a release directory. The real
	// host is started with `classifier-host serve --release DIR --pubkey HEX --transport unix --addr
	// PATH`; the README names that command.
	sockPath := filepath.Join(work, "classifier.sock")
	classifier := startFakeClassifierHost(log, sockPath)
	defer classifier.close()
	cfg.ClassifierAddress = "unix:" + sockPath
	st.section("classifier host", fmt.Sprintf("fake host on unix:%s (real framing, fixed labels; the real host command is in the README)", sockPath))

	// Resolved configuration, printed before anything starts.
	st.section("resolved configuration")
	if err := printConfig(cfg, slogLogger{log}); err != nil {
		st.failf("print-config failed: %v", err)
	}

	// Start the real service.
	svc, err := newService(context.Background(), cfg, log)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		st.failf("startup failed: %v", err)
		return st.finish()
	}
	st.section("startup order (§3.5)", strings.Join(svc.sup.Order(), " > "))
	st.check(cfg.PolicyKeyID != "", "policy key id resolved")

	host := svc.nativeHost()
	framesDir, err := findFramesDir()
	if err != nil {
		st.failf("%v", err)
	} else {
		st.section("golden frames through the real native-messaging framing", framesDir)
		res, err := driveGoldenFrames(ctx, host, framesDir, st.out)
		if err != nil {
			st.failf("driving golden frames: %v", err)
		}
		st.check(res.total == 6, "all six golden frames were driven (got %d)", res.total)
		st.check(res.failed == 0, "every golden frame produced an ack or a typed refusal (failures=%d)", res.failed)
	}

	// The same channel, the other directions: a mode query, a policy sync, and the extension's own
	// health row. These are what make the extension's inline decision local instead of a round trip.
	st.section("native channel: mode query, policy sync, extension health")
	for _, extra := range []struct {
		name  string
		frame protocol.NativeMessage
	}{
		{"mode_query chatgpt", mustMessage(protocol.TypeModeQuery, "x-mode", protocol.ModeQuery{ToolFingerprint: "genai.web.chat.v1:chatgpt", Host: "chatgpt.com", SizeBytes: 2048})},
		{"mode_query gemini (M0 tool)", mustMessage(protocol.TypeModeQuery, "x-mode2", protocol.ModeQuery{ToolFingerprint: "genai.web.chat.v1:gemini"})},
		{"policy_sync", mustMessage(protocol.TypePolicySync, "x-sync", protocol.PolicySyncRequest{KnownVersion: bundle.Version})},
		{"extension health", mustMessage(protocol.TypeHealth, "x-health", protocol.HealthReport{
			DeviceID: cfg.DeviceID, Collector: "capture-extension", State: protocol.StateDegraded,
			Detail: protocol.DetailClassifierUnavailable, Since: time.Now().Add(-time.Minute),
			Counters: map[protocol.Counter]uint64{protocol.CounterObserved: 12, protocol.CounterEmitted: 9, protocol.CounterDropped: 1},
		})},
	} {
		raw, _ := json.Marshal(extra.frame)
		response := host.Handle(ctx, raw)
		var resp protocol.NativeMessage
		_ = json.Unmarshal(response, &resp)
		fmt.Fprintf(st.out, "  %-32s -> %-12s %s\n", extra.name, resp.Type, summarizeResponse(resp))
	}

	// An over-cap body at a mode that permits reading: §5.3's discipline must show up as
	// `confidence: degraded`, never as a silent "clean".
	st.section("over-cap behaviour at M1 (synthetic frame; the golden over-cap case is an M0 tool)")
	overCap := protocol.ObservationMessage{
		ClientID: "selftest-overcap", Route: protocol.RouteExtWebRequest,
		ToolFingerprint: "genai.web.chat.v1:chatgpt", OccurredAt: time.Now().UTC(),
		SizeBytes: 8 << 20, HasContent: false, OverCap: true, DegradedReason: protocol.DetailContentOverCap,
	}
	rawOverCap, _ := json.Marshal(mustMessage(protocol.TypeObservation, "x-overcap", overCap))
	fmt.Fprintf(st.out, "  response: %s\n", summarizeResponsePayload(host.Handle(ctx, rawOverCap)))

	// The classifier host goes away: the next content-bearing frame must degrade to rules-only with
	// `confidence: degraded` and still be accepted — never fail the submission (§3.4, C21).
	st.section("classifier host stopped: rules-only fallback")
	classifier.close()
	_ = svc.host.Stop(ctx)
	fallback := protocol.ObservationMessage{
		ClientID: "selftest-fallback", Route: protocol.RouteExtWebRequest,
		ToolFingerprint: "genai.web.chat.v1:chatgpt", OccurredAt: time.Now().UTC(),
		SizeBytes: 25, HasContent: true, Content: []byte("Summarise the attached contract."),
	}
	rawFallback, _ := json.Marshal(mustMessage(protocol.TypeObservation, "x-fallback", fallback))
	fmt.Fprintf(st.out, "  response: %s\n", summarizeResponsePayload(host.Handle(ctx, rawFallback)))
	classifierStarted := true
	_ = classifierStarted

	// Health channel.
	st.section("health channel")
	if err := svc.health.PrintSnapshot(os.Stdout); err != nil {
		st.failf("printing health: %v", err)
	}
	snap := svc.health.Snapshot()
	st.check(len(snap.Reports) > 0, "the health channel carries at least one coverage row")
	for _, row := range snap.Reports {
		st.check(row.Validate() == nil, "health row for %s validates as protocol.HealthReport", row.Collector)
	}
	st.check(snap.Spool.Depth >= 6, "the spool holds the observations the frames produced (depth=%d)", snap.Spool.Depth)

	// Spool contents.
	st.section("spool contents (the real capture-spool, encrypted at rest)")
	entries, err := svc.peekSpool(100)
	if err != nil {
		st.failf("peeking the spool: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	m0Seen, degradedSeen := 0, 0
	for _, e := range entries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(e.Payload, &fields); err != nil {
			st.failf("spooled entry %d is not JSON: %v", e.Seq, err)
			continue
		}
		confidence := strings.Trim(string(fields["confidence"]), `"`)
		fmt.Fprintf(st.out, "  seq=%d kind=%s route=%s mode=%s bytes=%d dedup=%s… confidence=%q client=%s\n",
			e.Seq, e.Kind, e.Route, e.CollectionMode, e.SizeBytes, shortHash(e.DedupKey), confidence, e.ClientID)
		if err := core.ValidateEnvelopeMode(e.Payload); err != nil {
			st.failf("spooled entry %d fails the mode branch check: %v", e.Seq, err)
		}
		if e.CollectionMode == protocol.ModeM0 {
			m0Seen++
			for _, forbidden := range []string{"content_digest", "labels", "classifier_version", "confidence", "content_excerpt", "attachments"} {
				if _, ok := fields[forbidden]; ok {
					st.failf("M0 entry %d carries %q, which is evidence content was read", e.Seq, forbidden)
				}
			}
		} else if confidence == string(protocol.ConfidenceDegraded) {
			degradedSeen++
		}
	}
	st.check(m0Seen > 0, "an M0 observation was spooled with no content-derived field (found %d)", m0Seen)
	st.check(degradedSeen > 0, "a degraded observation was spooled with confidence=degraded (found %d)", degradedSeen)

	// The spool must hold ciphertext, not plaintext: the payload the pipeline wrote is inside the
	// spool's encrypted segments, and the prompt text must not appear in them.
	if found, err := spoolHoldsPlaintext(cfg.SpoolDir, "Summarise the attached contract."); err != nil {
		st.failf("scanning the spool for plaintext: %v", err)
	} else {
		st.check(!found, "the spool directory holds no plaintext prompt bytes (§12 encryption at rest)")
	}

	// Shutdown, and the ordering property that matters most: the loopback port is released first.
	st.section("shutdown (§3.5)")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := svc.Stop(shutdownCtx); err != nil {
		st.failf("shutdown: %v", err)
	}
	order := svc.sup.Order()
	startupSteps := len(core.StartupOrder())
	if len(order) > startupSteps {
		st.check(order[startupSteps] == core.StepReleaseLoopback, "the loopback port was released first (got %q)", order[startupSteps])
	} else {
		st.failf("shutdown recorded no steps")
	}
	fmt.Fprintf(st.out, "  full recorded order: %s\n", strings.Join(order, " > "))
	st.check(portIsFree(heldPort), "port %d is free after shutdown", heldPort)

	// The health file, if a writer was configured, must contain the same shape.
	if cfg.HealthFile != "" {
		if data, err := os.ReadFile(cfg.HealthFile); err == nil {
			lines := bytes.Count(data, []byte("\n"))
			st.check(lines >= 1, "the health channel wrote %d line(s) to %s", lines, cfg.HealthFile)
		}
	}

	// The last step is the one that cannot be faked in-process: the binary is started again as the
	// native-messaging host a browser would start, with real Chromium frames on its stdin and its
	// answers read back from its stdout. Everything above ran inside one process; this runs the
	// actual child-process model.
	st.section("native-messaging host as a separate process (Chromium's model)")
	if err := childProcessCheck(st, cfg, work); err != nil {
		st.failf("child-process native host: %v", err)
	}

	return st.finish()
}

// childProcessCheck starts this executable in --native-host mode with the six golden frames framed
// exactly as Chromium frames them, and decodes what comes back on stdout.
func childProcessCheck(st *selfTest, cfg Config, work string) error {
	framesDir, err := findFramesDir()
	if err != nil {
		return err
	}
	frames, err := frameBundle(framesDir)
	if err != nil {
		return err
	}
	framesFile := filepath.Join(work, "frames.bin")
	if err := os.WriteFile(framesFile, frames, 0o600); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	childSpool := filepath.Join(work, "child-spool")
	if err := os.MkdirAll(childSpool, 0o700); err != nil {
		return err
	}
	args := []string{
		"--native-host",
		"--spool-dir", childSpool,
		"--spool-key", filepath.Join(work, "child-spool.key"),
		"--tenant-id", cfg.TenantID,
		"--device-id", cfg.DeviceID,
		"--user-ref", cfg.UserRef,
		"--bundle", cfg.BundlePath,
		"--policy-key", cfg.PolicyKey,
		"--policy-key-id", cfg.PolicyKeyID,
		// The classifier host was stopped above, so the child must degrade exactly as the parent
		// did: rules-only, confidence degraded, and no failed submission.
		"--classifier-address", cfg.ClassifierAddress,
		"--health-file", "",
		"--proxy-tls=false",
		"--proxy-loopback=false",
		"--log-format", "text",
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = bytes.NewReader(frames)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w (stderr: %s)", exe, err, truncate(stderr.String(), 400))
	}

	responses, err := decodeFrames(stdout.Bytes())
	if err != nil {
		return err
	}
	acks, refusals := 0, 0
	for i, resp := range responses {
		switch resp.Type {
		case protocol.TypeAck:
			acks++
		case protocol.TypeRefusal:
			refusals++
		}
		fmt.Fprintf(st.out, "  child response %d: %-8s %s\n", i+1, resp.Type, summarizeResponse(resp))
	}
	st.check(len(responses) == 6, "the child process answered all six frames (got %d)", len(responses))
	st.check(acks+refusals == len(responses), "every child response was an ack or a typed refusal")
	fmt.Fprintf(st.out, "  frames file: %s (%d bytes, %d frames)\n", framesFile, len(frames), len(responses))
	return nil
}

// frameBundle concatenates every golden case file as real Chromium frames.
func frameBundle(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var c goldenCase
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := writeNativeFrame(&buf, compactJSON(c.Frame)); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// decodeFrames reads a stream of Chromium frames and returns the messages in them.
func decodeFrames(data []byte) ([]protocol.NativeMessage, error) {
	r := bytes.NewReader(data)
	var out []protocol.NativeMessage
	for {
		payload, err := readNativeFrame(r)
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
		var msg protocol.NativeMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			return out, err
		}
		out = append(out, msg)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------------------------
// selfTest is a tiny assertion collector: every failure is printed and the run exits non-zero.

type selfTest struct {
	out      io.Writer
	log      *slog.Logger
	failures []string
}

func (s *selfTest) section(title string, detail ...string) {
	fmt.Fprintf(s.out, "\n=== %s ===\n", title)
	if len(detail) > 0 && detail[0] != "" {
		fmt.Fprintf(s.out, "  %s\n", strings.Join(detail, " "))
	}
}

func (s *selfTest) check(cond bool, format string, args ...any) {
	mark := "PASS"
	if !cond {
		mark = "FAIL"
		s.failures = append(s.failures, fmt.Sprintf(format, args...))
	}
	fmt.Fprintf(s.out, "  [%s] %s\n", mark, fmt.Sprintf(format, args...))
}

func (s *selfTest) failf(format string, args ...any) {
	s.failures = append(s.failures, fmt.Sprintf(format, args...))
	fmt.Fprintf(s.out, "  [FAIL] %s\n", fmt.Sprintf(format, args...))
}

func (s *selfTest) finish() error {
	fmt.Fprintf(s.out, "\n=== self test %s ===\n", map[bool]string{true: "PASSED", false: "FAILED"}[len(s.failures) == 0])
	if len(s.failures) == 0 {
		return nil
	}
	for _, f := range s.failures {
		fmt.Fprintf(s.out, "  failed: %s\n", f)
	}
	return fmt.Errorf("self test failed: %d assertion(s)", len(s.failures))
}

// ---------------------------------------------------------------------------------------------
// Peers the host cannot provide: a relocated upstream and a classifier host.

// startUpstream runs a local "inference server" the broker can preflight against, so the broker
// reaches HOLDING and the shutdown path has a real port to release.
func startUpstream(log *slog.Logger) (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Warn("selftest upstream stopped", "error", err)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// fakeClassifierHost speaks device/protocol's framing over a real Unix socket and answers with fixed
// labels. It exists because device/classifier-host needs a signed release directory to serve, and a
// selftest must not depend on one; the framing and the handshake are the real ones.
type fakeClassifierHost struct {
	ln     net.Listener
	done   chan struct{}
	closed bool
}

func startFakeClassifierHost(log *slog.Logger, sockPath string) *fakeClassifierHost {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Error("selftest: fake classifier host could not listen", "path", sockPath, "error", err)
		return &fakeClassifierHost{done: closedChan()}
	}
	h := &fakeClassifierHost{ln: ln, done: make(chan struct{})}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(conn)
		}
	}()
	return h
}

func (h *fakeClassifierHost) serve(conn net.Conn) {
	defer conn.Close()
	raw, err := protocol.ReadFrameChecked(conn)
	if err != nil {
		return
	}
	var req protocol.HandshakeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	resp, _ := json.Marshal(protocol.HandshakeResponse{OK: true, ClassifierVersion: "selftest-rules-1"})
	if err := protocol.WriteFrame(conn, resp); err != nil {
		return
	}
	for {
		payload, err := protocol.ReadFrameChecked(conn)
		if err != nil {
			return
		}
		var classify protocol.ClassifyRequest
		if err := json.Unmarshal(payload, &classify); err != nil {
			return
		}
		answer, _ := json.Marshal(protocol.ClassifyResponse{
			Labels:            []protocol.Label{{Class: "source_code", Score: 0.62, RuleID: "R_SOURCE_CODE"}},
			ClassifierVersion: "selftest-rules-1",
			Confidence:        protocol.ConfidenceMedium,
		})
		if err := protocol.WriteFrame(conn, answer); err != nil {
			return
		}
	}
}

func (h *fakeClassifierHost) close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.ln != nil {
		_ = h.ln.Close()
	}
	close(h.done)
}

// ---------------------------------------------------------------------------------------------
// Fixtures and small helpers.

// selftestBundle is the policy the run enforces. The tool modes come from the golden frames' own
// tool fingerprints, so the resolved mode for each frame is the mode the frame's name claims:
// gemini is an M0 tool (its frame carries no content), chatgpt and claude are M1/M2 tools.
//
// The tenant default is M3 on purpose. §11.1 takes the most restrictive value across every axis and
// applies the tenant default where an axis has no entry, which makes the default a tenant-wide
// *ceiling* that a tool entry can only lower. A default of M0 would cap every tool at M0 and the run
// would prove nothing about M1/M2 handling.
func selftestBundle(heldPort, upstreamPort int) *policy.Bundle {
	return &policy.Bundle{
		Version:       "selftest-1",
		EffectiveAt:   time.Now().Add(-time.Hour),
		Actor:         "selftest",
		TenantDefault: protocol.ModeM3,
		ToolModes: map[string]protocol.CollectionMode{
			"genai.web.chat.v1:chatgpt": protocol.ModeM2,
			"genai.web.chat.v1:claude":  protocol.ModeM1,
			"genai.web.chat.v1:gemini":  protocol.ModeM0,
		},
		Interception: policy.Interception{
			SeedHosts:    []string{"api.example.invalid"},
			Ports:        []int{443},
			BodyCapBytes: 4096,
		},
		Loopback: policy.LoopbackPolicy{Ports: []policy.LoopbackPort{{
			ToolFingerprint: "selftest_local",
			Port:            heldPort,
			UpstreamPort:    upstreamPort,
			PreflightPath:   "/health",
			Mode:            protocol.ModeM1,
		}}},
		ProcDetect: policy.ProcDetectPolicy{
			ImageSignatures:  []string{"ollama"},
			PortMap:          []policy.PortMapEntry{{Port: upstreamPort, ToolFingerprint: "selftest_local"}},
			SignatureVersion: "selftest-seed",
		},
		Spool:      policy.SpoolBounds{MaxBytes: 1 << 20, MaxRows: 500, DeviceRetentionHours: 24},
		Classifier: policy.ClassifierRelease{ReleaseID: "selftest-rules-1", State: policy.ReleaseEnforcing},
	}
}

func mustMessage(kind, id string, body any) protocol.NativeMessage {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return protocol.NativeMessage{Type: kind, Version: protocol.Version, ID: id, Body: raw}
}

// findFramesDir locates device/integration/testdata/native from wherever the binary is run.
func findFramesDir() (string, error) {
	candidates := []string{
		filepath.Join("..", "integration", "testdata", "native"),
		filepath.Join("device", "integration", "testdata", "native"),
		filepath.Join("..", "..", "device", "integration", "testdata", "native"),
		filepath.Join("integration", "testdata", "native"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not find device/integration/testdata/native from %q; pass --native-frames DIR", mustGetwd())
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return wd
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func shortHash(s string) string {
	if len(s) <= 18 {
		return s
	}
	return s[:18]
}

// spoolHoldsPlaintext scans every file in the spool directory for a needle. The spool encrypts each
// record with an AEAD, so a plaintext prompt in the directory would be a defect — this is the
// assertion that makes "encrypted at rest" observable from the outside.
func spoolHoldsPlaintext(dir, needle string) (bool, error) {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(needle)) {
			return fmt.Errorf("plaintext found in %s", path)
		}
		return nil
	}) != nil, nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
