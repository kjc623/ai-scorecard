package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/loopback"
	"github.com/shadow-ai-capture/device/protocol"
)

// ---------------------------------------------------------------------------------------
// transcript
// ---------------------------------------------------------------------------------------

type result struct {
	claim  string
	ok     bool
	detail string
}

// finding is an observation about the component that is not one of the claims under test. It
// is printed with the same prominence but does not change the harness's exit code: the
// harness measures, and a claim it was asked to measure can pass while the run still shows
// something the component owner needs to see.
type finding struct {
	title  string
	detail string
}

var (
	resultsMu sync.Mutex
	results   []result
	findings  []finding
)

func noteFinding(title, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	say("  FINDING: %s - %s", title, detail)
	resultsMu.Lock()
	findings = append(findings, finding{title: title, detail: detail})
	resultsMu.Unlock()
}

func say(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func verify(claim string, ok bool, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	mark := "PASS"
	if !ok {
		mark = "FAIL"
	}
	say("  VERDICT (%s): %s - %s", claim, mark, detail)
	resultsMu.Lock()
	results = append(results, result{claim: claim, ok: ok, detail: detail})
	resultsMu.Unlock()
}

func summary() bool {
	resultsMu.Lock()
	defer resultsMu.Unlock()
	say("")
	say("================ summary ================")
	allOK := true
	for _, r := range results {
		mark := "PASS"
		if !r.ok {
			mark = "FAIL"
			allOK = false
		}
		say("  [%s] %-4s %s", mark, r.claim, r.detail)
	}
	for _, f := range findings {
		say("  [FINDING] %s - %s", f.title, f.detail)
	}
	say("=========================================")
	return allOK
}

// ---------------------------------------------------------------------------------------
// ports
// ---------------------------------------------------------------------------------------

// freePort asks the kernel for an unused loopback port and releases it. No fixed port and no
// vendor default is ever used: the whole measurement is deliberately independent of which
// port a tool would really choose.
//
// There is a small race between closing the listener and the broker binding it; on a quiet
// loopback interface that is the standard technique, and a collision would surface as a bind
// failure in the transcript rather than as a silent wrong result.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func loopbackAddr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// wsaECONNREFUSED is Windows' WSAECONNREFUSED. Go's net package surfaces it as a
// syscall.Errno inside a *net.OpError, and on Windows it is *not* equal to
// syscall.ECONNREFUSED, so a typed check has to name both. The message match is the
// documented fallback for an error that hides its errno.
const wsaECONNREFUSED = syscall.Errno(10061)

// isRefused reports whether a dial failed because nothing is listening, as opposed to timing
// out. A release window must produce the first, never the second (§6.2, assumption A7).
func isRefused(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == wsaECONNREFUSED || errno == syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "refused")
}

// refusalKind describes how a refusal was recognised, so the transcript shows whether the
// check was typed (an errno) or fell back to the message.
func refusalKind(err error) string {
	if err == nil {
		return "no error"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case wsaECONNREFUSED:
			return fmt.Sprintf("typed errno WSAECONNREFUSED(%d)", uint32(errno))
		case syscall.ECONNREFUSED:
			return fmt.Sprintf("typed errno ECONNREFUSED(%d)", uint32(errno))
		default:
			return fmt.Sprintf("errno %d (not a refusal)", uint32(errno))
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		return "message match"
	}
	return "unrecognised error"
}

// ---------------------------------------------------------------------------------------
// stand-in upstream inference server
// ---------------------------------------------------------------------------------------

// upstream is the stand-in local inference server. It records the *raw* bytes it receives, so
// the forwarding claim is measured against the byte stream and not against a re-parsed
// request. It can be stopped and restarted on a different port, which is the only relocation
// strategy this host can exercise (server-side relocation).
type upstream struct {
	mu        sync.Mutex
	ln        net.Listener
	port      int
	requests  []recorded
	conns     []net.Conn
	blackhole bool
	closed    chan struct{}
	wg        sync.WaitGroup
}

type recorded struct {
	at   time.Time
	raw  []byte
	body []byte
}

func startUpstream(blackhole bool) (*upstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u := &upstream{
		ln:        ln,
		port:      ln.Addr().(*net.TCPAddr).Port,
		blackhole: blackhole,
		closed:    make(chan struct{}),
	}
	u.wg.Add(1)
	go u.accept()
	return u, nil
}

func (u *upstream) addr() string { return loopbackAddr(u.port) }

func (u *upstream) accept() {
	defer u.wg.Done()
	for {
		conn, err := u.ln.Accept()
		if err != nil {
			select {
			case <-u.closed:
				return
			default:
				return
			}
		}
		u.mu.Lock()
		u.conns = append(u.conns, conn)
		u.mu.Unlock()
		u.wg.Add(1)
		go func(c net.Conn) {
			defer u.wg.Done()
			defer c.Close()
			u.serve(c)
		}(conn)
	}
}

// serve reads one request and answers it. In black-hole mode it accepts the connection and
// reads until the peer gives up, which is how a *preflight attempt* is counted from outside
// the broker: the broker never reaches BINDING, so no port is ever bound, and the only
// observable is the connection.
func (u *upstream) serve(conn net.Conn) {
	if u.blackhole {
		u.mu.Lock()
		u.requests = append(u.requests, recorded{at: time.Now()})
		u.mu.Unlock()
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	raw := &bytes.Buffer{}
	tee := io.TeeReader(conn, raw)
	req, err := http.ReadRequest(bufio.NewReader(tee))
	if err != nil {
		return
	}
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	u.mu.Lock()
	u.requests = append(u.requests, recorded{at: time.Now(), raw: raw.Bytes(), body: body})
	u.mu.Unlock()

	sum := sha256.Sum256(body)
	payload := fmt.Sprintf(`{"received":true,"method":%q,"path":%q,"body_sha256":%q,"body_len":%d}`,
		req.Method, req.URL.Path, hex.EncodeToString(sum[:]), len(body))
	fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(payload), payload)
}

func (u *upstream) snapshot() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recorded(nil), u.requests...)
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *upstream) stop() {
	select {
	case <-u.closed:
	default:
		close(u.closed)
	}
	_ = u.ln.Close()
	u.mu.Lock()
	for _, c := range u.conns {
		_ = c.Close()
	}
	u.mu.Unlock()
	u.wg.Wait()
}

// relocate stops this server and starts a new one on a different port: the upstream "moved",
// and nothing tells the broker.
func (u *upstream) relocate() (*upstream, error) {
	u.stop()
	return startUpstream(false)
}

// ---------------------------------------------------------------------------------------
// stand-in client
// ---------------------------------------------------------------------------------------

// sendRaw writes a request verbatim and reads whatever comes back. It is a raw client on
// purpose: "forwarded byte-identically" can only be checked against bytes, and a parsing
// client would hide exactly the differences the claim is about.
func sendRaw(addr string, request []byte, timeout time.Duration) ([]byte, time.Duration, error) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, time.Since(start), err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(request); err != nil {
		return nil, time.Since(start), err
	}
	resp, err := io.ReadAll(conn)
	return resp, time.Since(start), err
}

func buildProbe(addr string, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "POST /v1/chat/completions HTTP/1.1\r\n")
	fmt.Fprintf(&b, "Host: %s\r\n", addr)
	fmt.Fprintf(&b, "Content-Type: application/json\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(&b, "X-R1-Probe: harness\r\n")
	fmt.Fprintf(&b, "\r\n")
	b.WriteString(body)
	return []byte(b.String())
}

// dialRefused measures how long a plain client takes to be told "nothing is listening".
func dialRefused(addr string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return time.Since(start), err
	}
	_ = conn.Close()
	return time.Since(start), nil
}

// ---------------------------------------------------------------------------------------
// the broker under test
// ---------------------------------------------------------------------------------------

// recordingPipeline is the capture-core pipeline as the broker sees it. It records what the
// broker handed over, which is how "the request was captured" is measured independently of
// what the upstream received.
type recordingPipeline struct {
	mu     sync.Mutex
	obs    []core.Observation
	bodies [][]byte
}

func (p *recordingPipeline) ResolveMode(q core.ScopeQuery) core.Resolution {
	// M1: content may be read, so the broker retains the body and hands over a reader. This is
	// the mode in which capture is actually observable.
	return core.Resolution{Mode: protocol.ModeM1, Reasons: []string{"r1-harness"}}
}

func (p *recordingPipeline) Process(ctx context.Context, obs core.Observation) (core.Outcome, error) {
	var body []byte
	if obs.Content != nil {
		b, err := obs.Content.Read(ctx)
		if err == nil {
			body = b
		}
	}
	p.mu.Lock()
	p.obs = append(p.obs, obs)
	p.bodies = append(p.bodies, body)
	p.mu.Unlock()
	return core.Outcome{Route: obs.Route, Mode: protocol.ModeM1, Emitted: true, Reason: core.ReasonEmitted}, nil
}

func (p *recordingPipeline) snapshot() ([]core.Observation, [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.Observation(nil), p.obs...), append([][]byte(nil), p.bodies...)
}

// brokerConfig is the policy data the measurement runs with. Every timing is short so the
// matrix runs in seconds; the production defaults are policy data (§6.3) and are not what is
// under test here.
type brokerConfig struct {
	claimedPort   int
	upstreamPort  int
	preflightPath string
	probeInterval time.Duration
	fullInterval  time.Duration
	preflightTO   time.Duration
	maxFailures   int
	coolDown      time.Duration
	backoffBase   time.Duration
	backoffMax    time.Duration
}

func (bc brokerConfig) build(p *recordingPipeline) *loopback.Broker {
	return loopback.New(loopback.Config{
		Ports: []policy.LoopbackPort{{
			ToolFingerprint: "r1.stand-in-inference",
			Port:            bc.claimedPort,
			UpstreamPort:    bc.upstreamPort,
			PreflightPath:   bc.preflightPath,
			Mode:            protocol.ModeM1,
		}},
		ProbeInterval:          bc.probeInterval,
		PreflightInterval:      bc.fullInterval,
		PreflightTimeout:       bc.preflightTO,
		MaxConsecutiveFailures: bc.maxFailures,
		CoolDown:               bc.coolDown,
		BackoffBase:            bc.backoffBase,
		BackoffMax:             bc.backoffMax,
		Pipeline:               p,
		Agent:                  loopback.AgentInfo{Population: "r1-pilot", UserRef: "r1-user"},
	})
}

func healthLine(h core.Health) string {
	return fmt.Sprintf("state=%s detail=%s counters=%s", h.State, dash(string(h.Detail)), counterLine(h.Counters))
}

func dash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func counterLine(c map[protocol.Counter]uint64) string {
	keys := make([]string, 0, len(c))
	for k, v := range c {
		if v != 0 {
			keys = append(keys, fmt.Sprintf("%s=%d", k, v))
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "all zero"
	}
	return strings.Join(keys, " ")
}

// waitFor polls until cond is true or the deadline passes, and reports how long it took.
func waitFor(timeout time.Duration, cond func() bool) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < timeout {
		if cond() {
			return time.Since(start), true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start), false
}

// ---------------------------------------------------------------------------------------
// child processes
// ---------------------------------------------------------------------------------------

// childMarker is how a child tells the parent it has reached the interesting moment. A marker
// file rather than a pipe: no buffering to reason about, and it survives the child being
// killed immediately afterwards.
func markerPath(tag string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("r1-harness-%s-%d.ready", tag, os.Getpid()))
}

func signalMarker(path string, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "r1 harness child: marker: %v\n", err)
	}
}

func waitForMarker(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("child never signalled %s", path)
}

// startChild runs this binary in one of its child modes and waits for the marker.
func startChild(tag string, args ...string) (*exec.Cmd, string, error) {
	marker := markerPath(tag)
	_ = os.Remove(marker)
	full := append([]string{os.Args[0]}, args...)
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Env = append(os.Environ(), childEnvMarker+"="+marker)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return nil, marker, err
	}
	if err := waitForMarker(marker, 15*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, marker, fmt.Errorf("%w (child output: %s)", err, strings.TrimSpace(out.String()))
	}
	return cmd, marker, nil
}

// killChild is the real kill: TerminateProcess on Windows, SIGKILL on POSIX. Neither runs a
// deferred function, which is the point - the OS closing the socket is the property under
// test, and a graceful shutdown would test something else entirely.
func killChild(cmd *exec.Cmd) error {
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	return cmd.Wait()
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
