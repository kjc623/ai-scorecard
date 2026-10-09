// Package classifierlink is capture-core's connection to classifier-host, which runs as its child
// process and speaks length-prefixed frames on stdin and stdout. The component package owns the
// process; this package speaks the protocol on its stdio. Framing and the handshake shapes come
// from the protocol package.
//
// The package owns the failure contract: a version mismatch, a hung host or a crashed host marks
// the link degraded and the answer falls back to rules-only with confidence degraded. "No answer"
// is never reported as "no labels found", and a classifier outage never fails a submission.
package classifierlink

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// RulesOnlyVersion is the classifier version recorded when the host could not answer. It matches
// core.RulesOnlyVersion, which the pipeline uses for the same fallback.
const RulesOnlyVersion = "rules-only"

// Dialer connects to the host. The production dialer is the component supervisor's, which hands
// out the child's stdio; a test uses net.Pipe.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client is a resident connection to the classifier host. One connection is kept and reused
// because the host is spawned with the core and stays resident, keeping spawn cost off the
// interactive path.
type Client struct {
	dial        Dialer
	coreVersion string
	budget      time.Duration
	now         func() time.Time

	// request is held by one Classify call from writing its frame until its answer is read or the
	// call gives up: the host answers one request at a time, in order, on one pipe. It is a
	// channel so a waiting caller can give up when its budget or context ends.
	request chan struct{}

	mu        sync.Mutex
	sess      *session
	version   string
	degraded  bool
	reason    protocol.Detail
	attempts  int
	connected time.Time
}

// stallLimit is how long the host may hold an answer before its connection is dropped, which ends
// the child for its supervisor to restart. It is far beyond any request budget: a call that only
// runs out of budget leaves the connection, and the child, alone.
const stallLimit = 10 * time.Second

// NewWithDialer returns a client over dial. coreVersion is sent in the handshake so the host can
// refuse a peer it cannot serve; budget bounds one classification.
func NewWithDialer(dial Dialer, coreVersion string, budget time.Duration) *Client {
	if budget <= 0 {
		budget = 2 * time.Second
	}
	return &Client{dial: dial, coreVersion: coreVersion, budget: budget, now: time.Now, request: make(chan struct{}, 1)}
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
	c.sess = newSession(conn, c.now)
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

	payload, err := json.Marshal(req)
	if err != nil {
		return protocol.ClassifyResponse{}, err
	}

	// The budget covers the wait for the connection as well as the request itself. A call that
	// gives up while waiting has sent nothing, so the link is not degraded by it.
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case c.request <- struct{}{}:
	case <-ctx.Done():
		return c.rulesOnlyFallback(), nil
	case <-timer.C:
		return c.rulesOnlyFallback(), nil
	}
	defer func() { <-c.request }()
	if ctx.Err() != nil {
		return c.rulesOnlyFallback(), nil
	}

	sess := c.session()
	if sess == nil {
		if err := c.Connect(ctx); err != nil {
			return c.rulesOnlyFallback(), nil
		}
		if sess = c.session(); sess == nil {
			// Closed while connecting.
			return c.rulesOnlyFallback(), nil
		}
	}

	cl := &call{answer: make(chan []byte, 1)}
	go sess.send(cl, payload)

	var raw []byte
	select {
	case raw = <-cl.answer:
	case <-sess.ended:
		// The answer may have been read just before the connection ended.
		select {
		case raw = <-cl.answer:
		default:
			c.markDegraded(protocol.DetailHostUnreachable)
			c.forget(sess)
			return c.rulesOnlyFallback(), nil
		}
	case <-ctx.Done():
		c.giveUp(sess, cl)
		return c.rulesOnlyFallback(), nil
	case <-timer.C:
		// A hung host is detected by the request timeout: the child's pipes have no deadlines.
		c.giveUp(sess, cl)
		return c.rulesOnlyFallback(), nil
	}

	var resp protocol.ClassifyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		c.markDegraded(protocol.DetailVersionMismatch)
		return c.rulesOnlyFallback(), nil
	}
	if err := resp.Validate(); err != nil {
		// A defective answer must not widen what the device claims to know.
		c.markDegraded(protocol.DetailVersionMismatch)
		return c.rulesOnlyFallback(), nil
	}
	c.markAnswered()
	return resp, nil
}

// giveUp ends a call that ran out of time before its answer. The connection stays, and the host's
// late answer is read and discarded when it comes: dropping the connection would end the child.
// Only a host that has answered nothing for stallLimit is dropped.
func (c *Client) giveUp(sess *session, call *call) {
	c.markDegraded(protocol.DetailHostUnreachable)
	if sess.abandon(call) {
		c.drop(sess)
	}
}

// session returns the resident connection, forgetting it once it has ended.
func (c *Client) session() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil && c.sess.isEnded() {
		c.sess = nil
	}
	return c.sess
}

// forget stops using sess if it is still the resident connection; the next connection is made
// through Connect.
func (c *Client) forget(sess *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == sess {
		c.sess = nil
	}
}

// drop closes sess, which ends the child, and forgets it.
func (c *Client) drop(sess *session) {
	_ = sess.conn.Close()
	c.forget(sess)
}

// markAnswered clears the degradation a timed-out or defective answer recorded: the host answers.
func (c *Client) markAnswered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.degraded = false
	c.reason = protocol.DetailNone
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
	if c.sess == nil {
		return nil
	}
	err := c.sess.conn.Close()
	c.sess = nil
	return err
}

// session is one connection to the host. The host reads request frames in order and answers each
// one once, in the same order, so the n-th answer on a connection is the answer to the n-th
// request. An answer whose caller gave up is read here and discarded, and the connection stays.
type session struct {
	conn net.Conn
	now  func() time.Time
	// write is held while a frame is written. A write can outlive the call that started it, since
	// a child's stdin has no deadlines.
	write sync.Mutex
	// ended is closed when the connection can no longer be read.
	ended chan struct{}

	mu      sync.Mutex
	sent    uint64 // request frames written, or being written
	read    uint64 // answers read
	waiting *call  // the call that waits for its answer, if any
	// busy is when the host last answered, or last had a request to answer after answering all.
	busy time.Time
}

// call is one request on a session.
type call struct {
	n      uint64      // the request's place on the connection
	answer chan []byte // buffered, so the reader never blocks on a caller
	gone   bool        // the caller gave up
}

func newSession(conn net.Conn, now func() time.Time) *session {
	s := &session{conn: conn, now: now, ended: make(chan struct{})}
	go s.readAnswers()
	return s
}

// send writes call's request unless its caller has already given up. A failed write closes the
// connection: a partly written frame cannot be resynchronised.
func (s *session) send(call *call, payload []byte) {
	s.write.Lock()
	defer s.write.Unlock()
	s.mu.Lock()
	if call.gone || s.isEnded() {
		s.mu.Unlock()
		return
	}
	if s.read == s.sent {
		s.busy = s.now()
	}
	call.n = s.sent
	s.sent++
	s.waiting = call
	s.mu.Unlock()
	if err := protocol.WriteFrame(s.conn, payload); err != nil {
		_ = s.conn.Close()
	}
}

// readAnswers hands each answer to the call that waits for it and discards the rest. An answer to
// no request breaks the pairing, so it ends the connection like a read error does.
func (s *session) readAnswers() {
	defer close(s.ended)
	for {
		raw, err := protocol.ReadFrameChecked(s.conn)
		s.mu.Lock()
		if err != nil || s.read == s.sent {
			s.mu.Unlock()
			_ = s.conn.Close()
			return
		}
		n := s.read
		s.read++
		s.busy = s.now()
		if s.waiting != nil && s.waiting.n == n {
			s.waiting.answer <- raw
			s.waiting = nil
		}
		s.mu.Unlock()
	}
}

// abandon records that call's caller gave up, so that its answer is discarded, and reports whether
// the host has held an answer for stallLimit.
func (s *session) abandon(call *call) (stalled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call.gone = true
	if s.waiting == call {
		s.waiting = nil
	}
	return s.read < s.sent && s.now().Sub(s.busy) >= stallLimit
}

func (s *session) isEnded() bool {
	select {
	case <-s.ended:
		return true
	default:
		return false
	}
}
