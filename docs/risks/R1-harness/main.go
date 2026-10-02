package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/loopback"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	childEnvMarker = "R1_HARNESS_MARKER"
	preflightPath  = "/api/tags" // a read-only per-tool path (A8); the harness never calls a generation endpoint
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "child-broker":
			os.Exit(runChildBroker(os.Args[2:]))
		case "child-hold":
			os.Exit(runChildHold(os.Args[2:]))
		}
	}

	which := "all"
	if len(os.Args) > 1 {
		which = os.Args[1]
	}

	say("R1 loopback inference capture boundary - harness run %s", time.Now().Format(time.RFC3339))
	say("go %s, host loopback only, ephemeral ports only (no vendor default port is bound)", runtime.Version())

	if which == "all" || which == "a" {
		claimA()
	}
	if which == "all" || which == "b" {
		claimB()
	}
	if which == "all" || which == "c" {
		claimC()
	}
	if which == "all" || which == "d" {
		claimD()
	}
	if which == "all" || which == "e" {
		claimE()
	}

	if !summary() {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------------------
// child modes: the parts of the measurement that need a real second process
// ---------------------------------------------------------------------------------------

// runChildBroker runs the real broker in this process and holds the claimed port until it is
// killed. The parent kills it with the platform's uncatchable kill, which is the only way to
// measure "process death is a release" at the socket, rather than inside the state machine.
func runChildBroker(args []string) int {
	marker := os.Getenv(childEnvMarker)
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: child-broker <claimedPort> <upstreamPort>")
		return 2
	}
	claimed, _ := strconv.Atoi(args[0])
	upstreamPort, _ := strconv.Atoi(args[1])

	pipe := &recordingPipeline{}
	br := brokerConfig{
		claimedPort:   claimed,
		upstreamPort:  upstreamPort,
		preflightPath: preflightPath,
		probeInterval: 200 * time.Millisecond,
		fullInterval:  10 * time.Second,
		preflightTO:   2 * time.Second,
		maxFailures:   5,
		coolDown:      10 * time.Second,
		backoffBase:   100 * time.Millisecond,
		backoffMax:    time.Second,
	}.build(pipe)

	if err := br.Start(context.Background()); err != nil {
		signalMarker(marker, "START_ERROR "+err.Error())
		return 3
	}
	if _, ok := waitFor(5*time.Second, func() bool { return br.Health().State == protocol.StateHealthy }); !ok {
		signalMarker(marker, "NOT_HEALTHY "+healthLine(br.Health()))
		return 4
	}
	signalMarker(marker, fmt.Sprintf("HOLDING pid=%d port=%d upstream=%d", os.Getpid(), claimed, upstreamPort))
	select {} // the parent kills this process
}

// runChildHold holds a port like the user's own inference server would if relocation failed:
// it accepts connections and answers with a banner, and it must never be closed, killed or
// displaced by the broker (§6.2 rule 5).
func runChildHold(args []string) int {
	marker := os.Getenv(childEnvMarker)
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: child-hold <port>")
		return 2
	}
	port, _ := strconv.Atoi(args[0])
	ln, err := net.Listen("tcp", loopbackAddr(port))
	if err != nil {
		signalMarker(marker, "BIND_ERROR "+err.Error())
		return 3
	}
	signalMarker(marker, fmt.Sprintf("HOLDING pid=%d port=%d", os.Getpid(), port))
	for {
		conn, err := ln.Accept()
		if err != nil {
			return 0
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprint(c, "R1-HARNESS-HOLDER: this port belongs to another process\n")
			buf := make([]byte, 512)
			_, _ = c.Read(buf)
		}(conn)
	}
}

// ---------------------------------------------------------------------------------------
// (a) capture and forward
// ---------------------------------------------------------------------------------------

func claimA() {
	say("")
	say("### (a) the client's request is captured by the broker and forwarded")

	up, err := startUpstream(false)
	if err != nil {
		verify("a", false, "could not start the stand-in upstream: %v", err)
		return
	}
	defer up.stop()
	claimed, err := freePort()
	if err != nil {
		verify("a", false, "could not allocate a loopback port: %v", err)
		return
	}

	pipe := &recordingPipeline{}
	br := brokerConfig{
		claimedPort:   claimed,
		upstreamPort:  up.port,
		preflightPath: preflightPath,
		probeInterval: 200 * time.Millisecond,
		fullInterval:  10 * time.Second,
		preflightTO:   2 * time.Second,
		maxFailures:   5,
		coolDown:      10 * time.Second,
		backoffBase:   100 * time.Millisecond,
		backoffMax:    time.Second,
	}.build(pipe)
	ctx := context.Background()
	if err := br.Start(ctx); err != nil {
		verify("a", false, "Start: %v", err)
		return
	}
	defer br.Stop(context.Background())

	held, ok := waitFor(5*time.Second, func() bool { return br.Health().State == protocol.StateHealthy })
	say("  upstream (stand-in local inference server) : %s", up.addr())
	say("  broker claimed port                       : %s", loopbackAddr(claimed))
	say("  broker reached HOLDING after              : %s", held.Round(time.Millisecond))
	if !ok {
		verify("a", false, "the broker never reached HOLDING: %s", healthLine(br.Health()))
		return
	}

	body := `{"model":"stand-in","messages":[{"role":"system","content":"ignore"},{"role":"user","content":"R1 harness probe: summarise the attached contract."}]}`
	request := buildProbe(loopbackAddr(claimed), body)
	resp, elapsed, err := sendRaw(loopbackAddr(claimed), request, 5*time.Second)
	if err != nil {
		verify("a", false, "the request through the broker failed: %v", err)
		return
	}
	say("  client sent %d bytes, response %d bytes in %s: %s", len(request), len(resp), elapsed.Round(time.Millisecond), firstLine(string(resp)))

	recs := up.snapshot()
	if len(recs) != 1 {
		verify("a", false, "the upstream received %d requests, want exactly 1", len(recs))
		return
	}
	rec := recs[0]
	reqLine, sentHeaders := splitHead(request)
	gotLine, gotHeaders := splitHead(rec.raw)
	say("  request line as sent     : %s", reqLine)
	say("  request line as received : %s", gotLine)
	say("  headers as sent          : %s", strings.Join(sentHeaders, " | "))
	say("  headers as received      : %s", strings.Join(gotHeaders, " | "))

	sentSum := sha256.Sum256([]byte(body))
	gotSum := sha256.Sum256(rec.body)
	say("  body sha256 as sent      : %s (%d bytes)", hex.EncodeToString(sentSum[:]), len(body))
	say("  body sha256 as received  : %s (%d bytes)", hex.EncodeToString(gotSum[:]), len(rec.body))

	bodyIdentical := sentSum == gotSum
	methodPathSame := strings.HasPrefix(gotLine, "POST /v1/chat/completions")
	hostRewritten := !containsHeader(gotHeaders, "Host: "+loopbackAddr(claimed)) && containsHeader(gotHeaders, "Host: "+up.addr())

	obs, bodies := pipe.snapshot()
	captured := len(obs) == 1 && len(bodies) == 1 && sha256.Sum256(bodies[0]) == sentSum
	if len(obs) == 1 {
		say("  pipeline observed        : route=%s kind=%s size_bytes=%d content_retained=%d bytes",
			obs[0].Route, obs[0].Kind, obs[0].SizeBytes, len(bodies[0]))
	}
	extracted := ""
	if len(obs) == 1 {
		if text, _, err := (loopback.JSONExtractor{}).Extract(bodies[0], obs[0].MediaType); err == nil {
			extracted = text
		}
	}
	say("  C1 extraction at the route: %q", extracted)

	verify("a", bodyIdentical && methodPathSame && captured,
		"body byte-identical to the upstream (sha256 %s), method+path preserved, pipeline captured %d observation(s) with the same bytes; headers re-serialized (Host rewritten to the upstream: %v)",
		hex.EncodeToString(gotSum[:8]), len(obs), hostRewritten)
}

// ---------------------------------------------------------------------------------------
// (b) a real process kill releases the port
// ---------------------------------------------------------------------------------------

func claimB() {
	say("")
	say("### (b) a real process kill releases the port; the client is refused, not hung")

	up, err := startUpstream(false)
	if err != nil {
		verify("b", false, "could not start the stand-in upstream: %v", err)
		return
	}
	defer up.stop()
	claimed, _ := freePort()

	child, _, err := startChild("b", "child-broker", strconv.Itoa(claimed), strconv.Itoa(up.port), preflightPath)
	if err != nil {
		verify("b", false, "could not start the broker child: %v", err)
		return
	}
	say("  broker running in child pid %d, claiming %s, upstream %s", child.Process.Pid, loopbackAddr(claimed), up.addr())

	// Prove it is really holding and forwarding before the kill.
	body := `{"prompt":"before the kill"}`
	if _, _, err := sendRaw(loopbackAddr(claimed), buildProbe(loopbackAddr(claimed), body), 5*time.Second); err != nil {
		if kerr := killChild(child); kerr != nil {
			say("  (child reap: %v)", kerr)
		}
		verify("b", false, "the broker child was not serving before the kill: %v", err)
		return
	}
	say("  before the kill: the client's request reached the upstream (upstream has %d request(s))", up.count())

	killStart := time.Now()
	if err := killChild(child); err != nil {
		say("  (kill returned %v; on Windows a killed process settles as exit code 1)", err)
	}
	say("  killed with the platform's uncatchable kill (TerminateProcess on Windows, SIGKILL on POSIX); no deferred code ran")

	var refusedAfter time.Duration
	var dialErr error
	var refusedOK bool
	deadline := 3 * time.Second
	for time.Since(killStart) < deadline {
		elapsed, err := dialRefused(loopbackAddr(claimed), 500*time.Millisecond)
		if err != nil {
			refusedAfter = time.Since(killStart)
			dialErr = err
			refusedOK = isRefused(err)
			_ = elapsed
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	say("  after the kill, first client error after %s: %v", refusedAfter.Round(time.Millisecond), dialErr)

	// The port must be free for whoever owns it next - the upstream, or the user's own server.
	ln, bindErr := net.Listen("tcp", loopbackAddr(claimed))
	rebindable := bindErr == nil
	if rebindable {
		_ = ln.Close()
	}
	say("  the OS released the socket: another process can bind %s: %v", loopbackAddr(claimed), rebindable)

	hung := refusedAfter >= deadline || refusedAfter == 0
	verify("b", refusedOK && rebindable && !hung,
		"connection refused (not a timeout) %s after the kill, error %q; port immediately rebindable by another process: %v",
		refusedAfter.Round(time.Millisecond), dialErr, rebindable)
}

// ---------------------------------------------------------------------------------------
// (c) a second real process holds the port
// ---------------------------------------------------------------------------------------

func claimC() {
	say("")
	say("### (c) a port held by another real process: tampered, never fought for")

	up, err := startUpstream(false)
	if err != nil {
		verify("c", false, "could not start the stand-in upstream: %v", err)
		return
	}
	defer up.stop()
	claimed, _ := freePort()

	// The holder is a real second process, like the user's own server started on its default
	// port because relocation failed.
	holder, _, err := startChild("c", "child-hold", strconv.Itoa(claimed))
	if err != nil {
		verify("c", false, "could not start the port holder: %v", err)
		return
	}
	defer func() { _ = killChild(holder) }()
	bannerBefore, _, _ := sendRaw(loopbackAddr(claimed), []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), 3*time.Second)
	say("  holder is a separate process, pid %d, holding %s", holder.Process.Pid, loopbackAddr(claimed))
	say("  holder's banner: %q", strings.TrimSpace(string(bannerBefore)))

	pipe := &recordingPipeline{}
	br := brokerConfig{
		claimedPort:   claimed,
		upstreamPort:  up.port, // a LIVE upstream: preflight passes, so a refusal to bind can only be about the holder
		preflightPath: preflightPath,
		probeInterval: 200 * time.Millisecond,
		fullInterval:  10 * time.Second,
		preflightTO:   2 * time.Second,
		maxFailures:   5,
		coolDown:      1 * time.Second, // short so recovery is observable in the transcript
		backoffBase:   100 * time.Millisecond,
		backoffMax:    time.Second,
	}.build(pipe)
	ctx := context.Background()
	if err := br.Start(ctx); err != nil {
		verify("c", false, "Start: %v", err)
		return
	}
	defer br.Stop(context.Background())

	took, ok := waitFor(6*time.Second, func() bool { return br.Health().State == protocol.StateTampered })
	h := br.Health()
	say("  broker health after %s: %s", took.Round(time.Millisecond), healthLine(h))
	conf, held, reachable := br.Coverage()
	say("  coverage row: configured=%d held=%d upstream_reachable=%d", conf, held, reachable)
	if !ok {
		verify("c", false, "the broker never reported tampered: %s", healthLine(h))
		return
	}
	tamperedDetail := h.Detail == protocol.DetailPortHeldByOther

	bannerAfter, _, _ := sendRaw(loopbackAddr(claimed), []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), 3*time.Second)
	holderAlive := strings.Contains(string(bannerAfter), "R1-HARNESS-HOLDER")
	say("  the holder still owns the port after the broker's attempt: %v", holderAlive)
	say("  the broker never bound it: a client on %s still reaches the holder, not the broker", loopbackAddr(claimed))

	verify("c", tamperedDetail && holderAlive && held == 0,
		"reported tampered/%s with coverage held=%d while a live upstream was reachable, and the holder process kept the port",
		dash(string(h.Detail)), held)

	// §6.4 says the broker re-checks after a cool-down rather than in a tight loop. Once the
	// holder is gone, does it recover - and does its health row follow?
	say("  -- after the holder dies --")
	killStart := time.Now()
	if err := killChild(holder); err != nil {
		say("  (holder kill: %v)", err)
	}
	recBefore := up.count()
	rebound := false
	var reboundAfter time.Duration
	for time.Since(killStart) < 8*time.Second {
		if _, _, err := sendRaw(loopbackAddr(claimed), buildProbe(loopbackAddr(claimed), `{"prompt":"after the holder died"}`), 2*time.Second); err == nil {
			if up.count() > recBefore {
				rebound = true
				reboundAfter = time.Since(killStart)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	hAfter := br.Health()
	say("  the broker re-bound the freed port: %v (after %s)", rebound, reboundAfter.Round(time.Millisecond))
	say("  health after recovery: %s", healthLine(hAfter))
	conf, held, reachable = br.Coverage()
	say("  coverage after recovery: configured=%d held=%d upstream_reachable=%d", conf, held, reachable)
	staleTamper := rebound && hAfter.State == protocol.StateTampered
	if staleTamper {
		say("  FINDING: the port is held again (held=%d) but health still reports tampered/%s - the condition no longer holds",
			held, dash(string(hAfter.Detail)))
	}
	verify("c2", rebound && !staleTamper,
		"after the holder died the broker re-bound the port and reported %s/%s (stale tampered row: %v)",
		hAfter.State, dash(string(hAfter.Detail)), staleTamper)
}

// ---------------------------------------------------------------------------------------
// (d) the upstream moves: release, stay released, report upstream_unreachable
// ---------------------------------------------------------------------------------------

func claimD() {
	say("")
	say("### (d) after the upstream moves, the broker stays released and says upstream_unreachable")

	up1, err := startUpstream(false)
	if err != nil {
		verify("d", false, "could not start the stand-in upstream: %v", err)
		return
	}
	defer up1.stop()
	claimed, _ := freePort()

	pipe := &recordingPipeline{}
	mk := func(upstreamPort int) *loopback.Broker {
		return brokerConfig{
			claimedPort:   claimed,
			upstreamPort:  upstreamPort,
			preflightPath: preflightPath,
			probeInterval: 150 * time.Millisecond,
			fullInterval:  10 * time.Second,
			preflightTO:   1 * time.Second,
			maxFailures:   5,
			coolDown:      2 * time.Second,
			backoffBase:   100 * time.Millisecond,
			backoffMax:    time.Second,
		}.build(pipe)
	}
	br := mk(up1.port)
	ctx := context.Background()
	if err := br.Start(ctx); err != nil {
		verify("d", false, "Start: %v", err)
		return
	}
	defer br.Stop(context.Background())
	if _, ok := waitFor(5*time.Second, func() bool { return br.Health().State == protocol.StateHealthy }); !ok {
		verify("d", false, "the broker never reached HOLDING: %s", healthLine(br.Health()))
		return
	}
	say("  holding %s, forwarding to %s", loopbackAddr(claimed), up1.addr())

	up2, err := up1.relocate()
	if err != nil {
		verify("d", false, "could not relocate the upstream: %v", err)
		return
	}
	defer up2.stop()
	say("  the upstream moved: %s stopped, a new server started on %s (nothing told the broker)", up1.addr(), up2.addr())

	// The watchdog is the release trigger: two consecutive missed probes (§6.3).
	releasedAfter, released := waitFor(6*time.Second, func() bool {
		_, err := dialRefused(loopbackAddr(claimed), 300*time.Millisecond)
		return err != nil
	})
	if !released {
		verify("d", false, "the port was still held %s after the upstream moved", releasedAfter.Round(time.Millisecond))
		return
	}
	elapsed, dialErr := dialRefused(loopbackAddr(claimed), 1*time.Second)
	say("  the broker released the port after %s; a client now gets %q in %s",
		releasedAfter.Round(time.Millisecond), dialErr, elapsed.Round(time.Millisecond))

	h := br.Health()
	conf, held, reachable := br.Coverage()
	say("  health  : %s", healthLine(h))
	say("  coverage: configured=%d held=%d upstream_reachable=%d", conf, held, reachable)
	time.Sleep(300 * time.Millisecond)
	h2 := br.Health()
	say("  health 300ms later (still released, not flapping): %s", healthLine(h2))

	okState := h.State == protocol.StateDegraded && h.Detail == protocol.DetailUpstreamUnreachable
	verify("d", okState && held == 0 && isRefused(dialErr),
		"stayed released (connection refused), reported %s/%s, coverage held=%d reachable=%d",
		h.State, dash(string(h.Detail)), held, reachable)

	// The supported way to follow a relocation is policy, not guesswork: the port entry is
	// bundle data (§6.1), so a new upstream port is applied and the broker re-preflights.
	applyPolicyTo(br, claimed, up2.port)
	reboundAfter, rebound := waitFor(6*time.Second, func() bool { return br.Health().State == protocol.StateHealthy })
	say("  after the bundle names the new upstream port (%d): HOLDING again after %s", up2.port, reboundAfter.Round(time.Millisecond))
	before := up2.count()
	if _, _, err := sendRaw(loopbackAddr(claimed), buildProbe(loopbackAddr(claimed), `{"prompt":"after relocation"}`), 3*time.Second); err == nil {
		if up2.count() > before {
			say("  a request now flows through the broker to the relocated server")
		}
	}
	h3 := br.Health()
	say("  health after the policy change: %s", healthLine(h3))
	verify("d2", rebound && h3.State == protocol.StateHealthy,
		"the broker followed the relocation through policy (HOLDING, %s/%s)", h3.State, dash(string(h3.Detail)))
}

func applyPolicyTo(br *loopback.Broker, claimed, upstreamPort int) {
	var bundle policy.Bundle
	bundle.Loopback.Ports = []policy.LoopbackPort{{
		ToolFingerprint: "r1.stand-in-inference",
		Port:            claimed,
		UpstreamPort:    upstreamPort,
		PreflightPath:   preflightPath,
		Mode:            protocol.ModeM1,
	}}
	if err := br.ApplyPolicy(bundle); err != nil {
		say("  ApplyPolicy: %v", err)
	}
}

// ---------------------------------------------------------------------------------------
// (e) repeated failure reaches cool-down rather than a bind/release loop
// ---------------------------------------------------------------------------------------

func claimE() {
	say("")
	say("### (e) a repeated-failure sequence reaches cool-down, not a bind/release loop")

	// A black-hole upstream: it accepts and never answers, so every preflight attempt times
	// out and every accepted connection is one observable attempt. The broker cannot reach
	// BINDING without a passing preflight (§6.2 rule 3), so no port is ever bound here.
	black, err := startUpstream(true)
	if err != nil {
		verify("e", false, "could not start the black-hole upstream: %v", err)
		return
	}
	defer black.stop()
	claimed, _ := freePort()

	const (
		preflightTO = 300 * time.Millisecond
		coolDown    = 3 * time.Second
		runFor      = 7 * time.Second
	)
	pipe := &recordingPipeline{}
	br := brokerConfig{
		claimedPort:   claimed,
		upstreamPort:  black.port,
		preflightPath: preflightPath,
		probeInterval: 10 * time.Second, // keep the watchdog out of the way
		fullInterval:  10 * time.Second,
		preflightTO:   preflightTO,
		maxFailures:   3,
		coolDown:      coolDown,
		backoffBase:   100 * time.Millisecond,
		backoffMax:    400 * time.Millisecond,
	}.build(pipe)

	ctx := context.Background()
	if err := br.Start(ctx); err != nil {
		verify("e", false, "Start: %v", err)
		return
	}
	defer br.Stop(context.Background())
	say("  upstream is a black hole (accepts, never answers): every preflight attempt is one accepted connection")
	say("  policy: preflight timeout %s, backoff base 100ms/max 400ms, max consecutive failures 3, cool-down %s", preflightTO, coolDown)

	// A watcher proves the broker never holds the claimed port: a successful connect would
	// mean something is listening there, and the broker must never be it while the upstream is
	// unservable (E14).
	start := time.Now()
	stopWatch := make(chan struct{})
	var watchMu sync.Mutex
	accepted := 0
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			conn, err := net.DialTimeout("tcp", loopbackAddr(claimed), 200*time.Millisecond)
			if err == nil {
				watchMu.Lock()
				accepted++
				watchMu.Unlock()
				_ = conn.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	type sample struct {
		at     time.Duration
		state  protocol.CollectorState
		detail protocol.Detail
	}
	var timeline []sample
	lastState, lastDetail := protocol.CollectorState(""), protocol.Detail("")
	for time.Since(start) < runFor {
		h := br.Health()
		if h.State != lastState || h.Detail != lastDetail {
			timeline = append(timeline, sample{at: time.Since(start), state: h.State, detail: h.Detail})
			lastState, lastDetail = h.State, h.Detail
		}
		time.Sleep(25 * time.Millisecond)
	}
	close(stopWatch)
	wg.Wait()

	attempts := black.snapshot()
	say("  preflight attempts accepted by the black hole over %s: %d", runFor, len(attempts))
	var gaps []time.Duration
	for i, a := range attempts {
		rel := a.at.Sub(start).Round(10 * time.Millisecond)
		if i == 0 {
			say("    attempt %d at +%s", i+1, rel)
			continue
		}
		gap := a.at.Sub(attempts[i-1].at)
		gaps = append(gaps, gap)
		say("    attempt %d at +%s (gap %s)", i+1, rel, gap.Round(10*time.Millisecond))
	}
	say("  health timeline (state/detail changes only):")
	for _, s := range timeline {
		say("    +%-8s %s/%s", s.at.Round(10*time.Millisecond), s.state, dash(string(s.detail)))
	}

	maxGap := time.Duration(0)
	for _, g := range gaps {
		if g > maxGap {
			maxGap = g
		}
	}
	cooledDown := false
	for _, s := range timeline {
		if s.detail == protocol.DetailCoolingDown {
			cooledDown = true
		}
	}
	watchMu.Lock()
	watchAccepted := accepted
	watchMu.Unlock()
	say("  the claimed port was never held during the sequence: connections accepted on %s = %d", loopbackAddr(claimed), watchAccepted)

	ln, bindErr := net.Listen("tcp", loopbackAddr(claimed))
	portFree := bindErr == nil
	if portFree {
		_ = ln.Close()
	}
	say("  the claimed port is free after the sequence: %v", portFree)

	// The property: after the failure streak the broker must stop trying for a long cool-down,
	// so attempts must show a gap of at least the cool-down, and it must report cooling_down.
	ok := len(attempts) >= 3 && maxGap >= coolDown-200*time.Millisecond && cooledDown && watchAccepted == 0 && portFree
	verify("e", ok,
		"%d attempts, largest gap %s (>= cool-down %s), reported cooling_down: %v, port never held: %v",
		len(attempts), maxGap.Round(10*time.Millisecond), coolDown, cooledDown, watchAccepted == 0)
}

// ---------------------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------------------

func firstLine(s string) string {
	if i := strings.Index(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// splitHead returns the request line and the header lines of a raw request.
func splitHead(raw []byte) (string, []string) {
	s := string(raw)
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		s = s[:i]
	}
	lines := strings.Split(s, "\r\n")
	if len(lines) == 1 {
		lines = strings.Split(s, "\n")
	}
	if len(lines) == 0 {
		return "", nil
	}
	headers := make([]string, 0, len(lines)-1)
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) != "" {
			headers = append(headers, strings.TrimSpace(l))
		}
	}
	sort.Strings(headers)
	return strings.TrimSpace(lines[0]), dedupStrings(headers)
}

func containsHeader(headers []string, want string) bool {
	for _, h := range headers {
		if strings.EqualFold(h, want) {
			return true
		}
	}
	return false
}

var _ = errors.Is
