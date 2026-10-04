// Package classifierlink is the capture-core side of the local socket to `classifier-host`
// (docs/01-collectors.md §3.4): Unix domain socket under the service's directory on macOS, named
// pipe on Windows, request/response, length-prefixed, with a protocol version byte.
//
// Framing and the handshake shapes come from device/protocol, which is the single source of
// truth: this package does not define a second wire format. The behaviour it does own is the
// failure contract: a version mismatch, a hung host or a crashed host marks the host `degraded`
// and falls back to rules-only with `confidence: degraded` — "no answer" is never reported as
// "no labels found", and a classifier outage never fails the submission (C21).
package classifierlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// RulesOnlyVersion is the classifier version recorded when the host could not answer: the
// rules-only baseline, from §13.3 rule 6. It matches core.RulesOnlyVersion, which the pipeline
// uses for the same fallback; the value is part of the envelope contract, so the two must agree.
const RulesOnlyVersion = "rules-only"

// Address is where the classifier host listens. It is configuration, not code: deployment
// decides the pipe name or the socket path.
type Address struct {
	// Network is "unix" or "pipe". Anything else is refused rather than guessed at.
	Network string
	// Path is the socket path for "unix", or the pipe name for "pipe".
	Path string
}

// Valid reports whether the address is one this package can dial.
//
// "tcp" is accepted only for a loopback address, and only because a host on a platform without
// AF_UNIX (or a lab running the two components on different machines' loopback interfaces) has no
// other transport. The production transports are "unix" and "pipe" (§3.4); a non-loopback TCP
// address is refused rather than silently becoming a network listener for a component that is
// supposed to be local.
func (a Address) Valid() bool {
	switch a.Network {
	case "unix", "pipe":
		return strings.TrimSpace(a.Path) != ""
	case "tcp":
		return isLoopbackAddr(a.Path)
	case "stdio":
		// Path is the host executable; the connection is made by ChildDialer, not by Dial.
		return strings.TrimSpace(a.Path) != ""
	default:
		return false
	}
}

func isLoopbackAddr(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DefaultAddress returns the platform's address for a service directory and host name.
func DefaultAddress(serviceDir, name string) Address {
	return addressFor(runtime.GOOS, serviceDir, name)
}

// addressFor is the platform decision, separated from runtime.GOOS so it can be asserted for both
// platforms on any host: a test of DefaultAddress alone could only ever check the host it ran on,
// which is how the Windows assertion silently became a false failure on Linux.
func addressFor(goos, serviceDir, name string) Address {
	if goos == "windows" {
		return Address{Network: "pipe", Path: `\\.\pipe\` + name}
	}
	return Address{Network: "unix", Path: strings.TrimRight(serviceDir, "/") + "/" + name}
}

// Dialer connects to the host. It is a seam so a test can use net.Pipe and never bind anything.
type Dialer func(ctx context.Context, addr Address) (net.Conn, error)

// Dial is the real dialer: a Unix socket, a Windows named pipe opened through CreateFile, or a
// loopback TCP address for a platform without AF_UNIX.
// (The named-pipe *server* side is out of scope here: it belongs to classifier-host.)
func Dial(ctx context.Context, addr Address) (net.Conn, error) {
	switch addr.Network {
	case "unix":
		d := net.Dialer{}
		return d.DialContext(ctx, "unix", addr.Path)
	case "tcp":
		if !isLoopbackAddr(addr.Path) {
			return nil, fmt.Errorf("classifierlink: refusing non-loopback TCP address %q; the classifier channel is local", addr.Path)
		}
		d := net.Dialer{}
		return d.DialContext(ctx, "tcp", addr.Path)
	case "pipe":
		// os.OpenFile reaches CreateFile on Windows; a named pipe path is a file path there.
		f, err := os.OpenFile(addr.Path, os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("classifierlink: opening %s: %w", addr.Path, err)
		}
		return pipeConn{File: f}, nil
	case "stdio":
		return nil, fmt.Errorf("classifierlink: a stdio host is spawned, not dialled; set ChildDialer for %s", addr.Path)
	default:
		return nil, fmt.Errorf("classifierlink: network %q is not dialable", addr.Network)
	}
}

// pipeConn adapts an *os.File to net.Conn for a Windows named pipe. The addresses are
// descriptive rather than meaningful: a pipe has no network endpoints. Deadlines are
// best-effort, which is why Classify also guards its own budget with a timer.
type pipeConn struct{ *os.File }

func (p pipeConn) LocalAddr() net.Addr  { return pipeAddr(p.File.Name()) }
func (p pipeConn) RemoteAddr() net.Addr { return pipeAddr(p.File.Name()) }

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// Client is a resident connection to the classifier host. One connection is kept and reused
// because the host is spawned with the core and stays resident, keeping spawn cost off the
// interactive path.
type Client struct {
	addr        Address
	dial        Dialer
	coreVersion string
	budget      time.Duration
	now         func() time.Time

	mu        sync.Mutex
	conn      net.Conn
	version   string
	degraded  bool
	reason    protocol.Detail
	attempts  int
	connected time.Time
}

// New returns a client. coreVersion is sent in the handshake so the host can refuse a peer it
// cannot serve; budget bounds one classification.
func New(addr Address, coreVersion string, budget time.Duration) (*Client, error) {
	if !addr.Valid() {
		return nil, fmt.Errorf("classifierlink: address %+v is not dialable", addr)
	}
	if budget <= 0 {
		budget = 2 * time.Second
	}
	return &Client{
		addr:        addr,
		dial:        Dial,
		coreVersion: coreVersion,
		budget:      budget,
		now:         time.Now,
	}, nil
}

// SetDialer replaces the transport, for a test or for a future transport.
func (c *Client) SetDialer(d Dialer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dial = d
}

// Connect dials and performs the version handshake. A handshake the host refuses, or a framing
// version mismatch, marks the link degraded and returns the cause — it never panics the caller
// and never blocks a submission.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	dial := c.dial
	c.mu.Unlock()

	conn, err := dial(ctx, c.addr)
	if err != nil {
		c.markDegraded(protocol.DetailHostUnreachable)
		return fmt.Errorf("classifierlink: dial %s: %w", c.addr.Path, err)
	}

	req := protocol.HandshakeRequest{CoreVersion: c.coreVersion, ProtocolVersion: protocol.Version}
	handshakeBytes, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("classifierlink: encoding handshake: %w", err)
	}
	if err := protocol.WriteFrame(conn, handshakeBytes); err != nil {
		_ = conn.Close()
		c.markDegraded(protocol.DetailHostUnreachable)
		return fmt.Errorf("classifierlink: writing handshake: %w", err)
	}
	_ = conn.SetReadDeadline(c.now().Add(c.budget))
	payload, err := protocol.ReadFrameChecked(conn)
	if err != nil {
		_ = conn.Close()
		// A framing version mismatch is a degraded handshake, never a crash (§3.4, frames.go).
		c.markDegraded(protocol.DetailVersionMismatch)
		return fmt.Errorf("classifierlink: handshake: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	var resp protocol.HandshakeResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		_ = conn.Close()
		c.markDegraded(protocol.DetailVersionMismatch)
		return fmt.Errorf("classifierlink: handshake response: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !resp.OK {
		c.degraded = true
		c.reason = resp.Reason
		if c.reason == protocol.DetailNone {
			c.reason = protocol.DetailVersionMismatch
		}
		_ = conn.Close()
		return fmt.Errorf("classifierlink: host refused the handshake: %s", c.reason)
	}
	c.conn = conn
	c.version = resp.ClassifierVersion
	c.degraded = false
	c.reason = protocol.DetailNone
	c.connected = c.now()
	return nil
}

// Classify sends one request and returns the host's answer. It never returns an error for an
// unavailable host: it returns a degraded response that satisfies the contract's cross-field
// rules, so the caller can emit an event with `confidence: degraded` rather than emitting
// nothing at all (which would read as "no sensitive data found").
func (c *Client) Classify(ctx context.Context, req protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	if err := req.Validate(); err != nil {
		// The caller asked the classifier to do something the mode forbids. That is a defect
		// upstream, not an outage: it is refused rather than degraded.
		return protocol.ClassifyResponse{}, err
	}
	budget, ok := req.EffectiveBudget()
	if !ok {
		budget = c.budget
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		if err := c.Connect(ctx); err != nil {
			return c.rulesOnlyFallback(), nil
		}
		c.mu.Lock()
		conn = c.conn
		c.mu.Unlock()
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return protocol.ClassifyResponse{}, err
	}
	done := make(chan struct{})
	var (
		resp    protocol.ClassifyResponse
		readErr error
	)
	go func() {
		defer close(done)
		c.mu.Lock()
		cn := c.conn
		c.mu.Unlock()
		if cn == nil {
			readErr = errors.New("classifierlink: not connected")
			return
		}
		_ = cn.SetDeadline(c.now().Add(budget))
		if err := protocol.WriteFrame(cn, payload); err != nil {
			readErr = err
			return
		}
		raw, err := protocol.ReadFrameChecked(cn)
		if err != nil {
			readErr = err
			return
		}
		_ = cn.SetDeadline(time.Time{})
		readErr = decodeClassifyFrame(raw, &resp)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		c.markDegraded(protocol.DetailHostUnreachable)
		c.reset()
		return c.rulesOnlyFallback(), nil
	case <-time.After(budget):
		// A hung host is detected by request timeout (§3.4). The deadline above also fires; this
		// is the belt for a transport that ignores deadlines.
		c.markDegraded(protocol.DetailHostUnreachable)
		c.reset()
		return c.rulesOnlyFallback(), nil
	}

	if readErr != nil {
		c.markDegraded(protocol.DetailHostUnreachable)
		c.reset()
		return c.rulesOnlyFallback(), nil
	}
	if err := resp.Validate(); err != nil {
		// A defective answer must not widen what the device claims to know.
		c.markDegraded(protocol.DetailVersionMismatch)
		return c.rulesOnlyFallback(), nil
	}
	return resp, nil
}

// decodeClassifyFrame reads the host's answer. classifier-host frames a verdict — the response
// under `response`, with the enforcement half alongside — while a bare ClassifyResponse is what
// protocol defines; both are accepted, and only the response is taken.
func decodeClassifyFrame(raw []byte, resp *protocol.ClassifyResponse) error {
	var verdict struct {
		Response *protocol.ClassifyResponse `json:"response"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return err
	}
	if verdict.Response != nil {
		*resp = *verdict.Response
		return nil
	}
	return json.Unmarshal(raw, resp)
}

// reset drops the resident connection so the next call re-dials. A host that crashed is
// restarted by the supervisor with backoff; this side simply stops reusing a dead socket.
func (c *Client) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *Client) markDegraded(d protocol.Detail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The first cause of a degradation episode wins: a retry that fails for a different reason
	// (a refused handshake followed by an unreachable socket, say) must not erase the reason that
	// explains why the device is not classifying. A successful handshake clears it.
	if c.reason == protocol.DetailNone {
		c.reason = d
	}
	c.degraded = true
	c.attempts++
}

// Degraded reports whether the link is currently degraded and why.
func (c *Client) Degraded() (bool, protocol.Detail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.degraded, c.reason
}

// ClassifierVersion returns the version the host reported, or the rules-only baseline when the
// link is degraded.
func (c *Client) ClassifierVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version == "" || c.degraded {
		return RulesOnlyVersion
	}
	return c.version
}

// rulesOnlyFallback is §13.3 rule 6's fallback: rules-only classification from the baseline with
// `confidence: degraded` — never unclassified-and-silent.
//
// The response names the stage that did not run, because protocol.ClassifyResponse.Validate
// refuses a degraded answer that cannot be attributed to a failed or truncated stage: an
// unattributable "degraded" is exactly the "no labels found" confusion this contract prevents.
func (c *Client) rulesOnlyFallback() protocol.ClassifyResponse {
	degraded, reason := c.Degraded()
	if !degraded {
		reason = protocol.DetailHostUnreachable
	}
	return protocol.ClassifyResponse{
		Labels:            []protocol.Label{},
		ClassifierVersion: RulesOnlyVersion,
		Confidence:        protocol.ConfidenceDegraded,
		Stages: []protocol.StageResult{{
			Stage:  "model",
			Ran:    false,
			Failed: true,
			Detail: reason,
			Err:    "classifier host unavailable; rules-only fallback",
		}},
	}
}

// Close releases the resident connection. It is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}
