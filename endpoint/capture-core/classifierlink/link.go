// Package classifierlink is capture-core's connection to classifier-host, which runs as its child
// process and speaks length-prefixed frames on stdin and stdout. Framing and the handshake shapes
// come from the protocol package.
//
// The package owns the failure contract: a version mismatch, a hung host or a crashed host marks
// the link degraded and the answer falls back to rules-only with confidence degraded. "No answer"
// is never reported as "no labels found", and a classifier outage never fails a submission.
package classifierlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// RulesOnlyVersion is the classifier version recorded when the host could not answer. It matches
// core.RulesOnlyVersion, which the pipeline uses for the same fallback.
const RulesOnlyVersion = "rules-only"

// Dialer connects to the host. The production dialer starts the child; a test uses net.Pipe.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client is a resident connection to the classifier host. One connection is kept and reused
// because the host is spawned with the core and stays resident, keeping spawn cost off the
// interactive path.
type Client struct {
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

// New returns a client that runs the classifier host exe with args as its child. coreVersion is
// sent in the handshake so the host can refuse a peer it cannot serve; budget bounds one
// classification; the child's log lines go to stderr.
func New(exe string, args []string, stderr io.Writer, coreVersion string, budget time.Duration) *Client {
	return NewWithDialer(ChildDialer(exe, args, stderr), coreVersion, budget)
}

// NewWithDialer returns a client over dial.
func NewWithDialer(dial Dialer, coreVersion string, budget time.Duration) *Client {
	if budget <= 0 {
		budget = 2 * time.Second
	}
	return &Client{dial: dial, coreVersion: coreVersion, budget: budget, now: time.Now}
}

// Connect dials and performs the version handshake. A handshake the host refuses, or a framing
// version mismatch, marks the link degraded and returns the cause — it never panics the caller
// and never blocks a submission.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		c.markDegraded(protocol.DetailHostUnreachable)
		return fmt.Errorf("classifierlink: starting the classifier host: %w", err)
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
		// A framing version mismatch is a degraded handshake, never a crash.
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
// unavailable host: it returns a degraded response that satisfies the response's cross-field
// rules, so the caller emits an event with confidence degraded rather than nothing (which would
// read as "no sensitive data found").
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
		readErr = json.Unmarshal(raw, &resp)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		c.markDegraded(protocol.DetailHostUnreachable)
		c.reset()
		return c.rulesOnlyFallback(), nil
	case <-time.After(budget):
		// A hung host is detected by the request timeout: the child's pipes have no deadlines.
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

// reset drops the resident connection, which ends the child; the next call starts a new one.
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

// rulesOnlyFallback is the answer when the host could not classify: no labels, confidence
// degraded, and the stage that did not run named, because a degraded answer that names no failed
// stage is refused by protocol.ClassifyResponse.Validate.
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
