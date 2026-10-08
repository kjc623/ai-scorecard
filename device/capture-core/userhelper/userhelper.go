// Package userhelper runs one capture-core helper process in each signed-in user session and
// talks to it over the local endpoint.
//
// capture-core runs as LocalSystem (root elsewhere), and some work only a process in the user's
// own session can do, such as showing that user a notification. The provider enumerates the
// interactive sessions that have a signed-in user, starts `capture-core --user-helper` in each
// session that has none, restarts one that exits (at most maxRestarts times in restartWindow per
// session), and accepts a helper's connection only from the user signed in to the session it
// names. The helper collects nothing, so the provider is not switched by policy.
package userhelper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	// maxRestarts helpers are started again in one session within restartWindow; then the session
	// is left without one until the window allows another.
	maxRestarts   = 5
	restartWindow = 10 * time.Minute
	// notifyTimeout bounds how long Notify waits for the helper's answer.
	notifyTimeout = 10 * time.Second
)

var (
	// ErrNoHelper is returned by Notify for a session with no connected helper.
	ErrNoHelper = errors.New("userhelper: the session has no connected helper")
	// ErrNotSessionOwner refuses a helper_hello from a user who is not signed in to the session.
	ErrNotSessionOwner = errors.New("userhelper: the connecting user is not signed in to the session")
)

// Session is an interactive session with a signed-in user.
type Session struct {
	ID   uint32
	User hostinfo.User
}

// Process is a helper the provider started.
type Process interface {
	PID() uint32
	// Done is closed when the process has exited.
	Done() <-chan struct{}
	Kill() error
}

// Platform is what the provider needs from the operating system.
type Platform interface {
	// Sessions lists the interactive sessions that have a signed-in user.
	Sessions() ([]Session, error)
	// Owner names the user signed in to a session.
	Owner(sessionID uint32) (hostinfo.User, error)
	// Launch starts a helper in a session as the user signed in to it.
	Launch(sessionID uint32) (Process, error)
}

// Config is the provider's wiring.
type Config struct {
	// Platform is nil where helpers are not supported; the provider then reports absent.
	Platform Platform
	Log      core.Logger
	Clock    func() time.Time
}

// Provider is the user_helper collector.
type Provider struct {
	cfg      Config
	counters *core.CounterSet
	since    time.Time

	// reconcile serialises Reconcile, so two passes never start two helpers in one session.
	reconcile sync.Mutex

	mu          sync.Mutex
	running     bool
	sessions    map[uint32]*session
	enumErr     error
	enumerated  bool
	lastSuccess time.Time
}

// session is one signed-in session's helper.
type session struct {
	id        uint32
	user      hostinfo.User
	proc      Process
	launched  bool        // a helper was started here at least once
	restarts  []time.Time // restarts within restartWindow
	exhausted bool
	conn      *helperConn
}

// New returns the provider.
func New(cfg Config) *Provider {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	now := cfg.Clock()
	return &Provider{cfg: cfg, counters: core.NewCounterSet(now), since: now, sessions: map[uint32]*session{}}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

func (p *Provider) Name() protocol.Collector { return protocol.CollectorUserHelper }

// Start starts a helper in every signed-in session.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()
	p.Reconcile(ctx)
	return nil
}

// Stop disconnects and ends every helper.
func (p *Provider) Stop(context.Context) error {
	p.mu.Lock()
	p.running = false
	sessions := p.sessions
	p.sessions = map[uint32]*session{}
	p.mu.Unlock()
	for _, s := range sessions {
		s.end()
	}
	return nil
}

// ApplyPolicy changes nothing: the helper collects nothing, so no setting governs it.
func (p *Provider) ApplyPolicy(policy.Bundle) error { return nil }

// Health is healthy when every signed-in session has a connected helper.
func (p *Provider) Health() core.Health {
	if p.cfg.Platform == nil {
		return core.Absent(protocol.DetailHelperUnavailable, p.since, p.counters)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return core.Absent(protocol.DetailNone, p.since, p.counters)
	}
	if p.enumErr != nil || !p.enumerated {
		return core.Degraded(protocol.DetailHelperUnavailable, p.since, p.lastSuccess, p.counters)
	}
	for _, s := range p.sessions {
		if s.conn == nil {
			return core.Degraded(protocol.DetailHelperUnavailable, p.since, p.lastSuccess, p.counters)
		}
	}
	return core.Healthy(protocol.DetailNone, p.since, p.lastSuccess, p.counters)
}

// Reconcile matches the helpers to the signed-in sessions: it ends the helpers of sessions that
// are gone and starts one in each session that has none, within the restart budget. The service
// calls it on its console-user tick.
func (p *Provider) Reconcile(context.Context) {
	if p.cfg.Platform == nil {
		return
	}
	p.reconcile.Lock()
	defer p.reconcile.Unlock()
	current, err := p.cfg.Platform.Sessions()

	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.enumerated = true
	p.enumErr = err
	if err != nil {
		p.mu.Unlock()
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("userhelper: listing the signed-in sessions failed: %v", err)
		return
	}
	var ended []*session
	present := map[uint32]bool{}
	for _, c := range current {
		present[c.ID] = true
		s := p.sessions[c.ID]
		// A session whose user changed is a new session that happens to reuse the id.
		if s != nil && !sameUser(s.user, c.User) {
			ended = append(ended, s)
			s = nil
		}
		if s == nil {
			s = &session{id: c.ID, user: c.User}
			p.sessions[c.ID] = s
		}
	}
	for id, s := range p.sessions {
		if !present[id] {
			ended = append(ended, s)
			delete(p.sessions, id)
		}
	}
	var launch []*session
	now := p.cfg.Clock()
	for _, s := range p.sessions {
		if s.proc != nil && exited(s.proc) {
			s.proc = nil
		}
		if s.proc != nil || s.conn != nil {
			continue
		}
		if s.launched {
			s.restarts = within(s.restarts, now.Add(-restartWindow))
			if len(s.restarts) >= maxRestarts {
				if !s.exhausted {
					s.exhausted = true
					p.cfg.Log.Printf("userhelper: the helper in session %d exited %d times in %s; not restarting it for now", s.id, maxRestarts, restartWindow)
				}
				continue
			}
			s.restarts = append(s.restarts, now)
		}
		s.exhausted = false
		s.launched = true
		launch = append(launch, s)
	}
	p.mu.Unlock()

	for _, s := range ended {
		s.end()
	}
	for _, s := range launch {
		proc, err := p.cfg.Platform.Launch(s.id)
		if err != nil {
			p.counters.Add(protocol.CounterErrors)
			p.cfg.Log.Printf("userhelper: starting the helper in session %d failed: %v", s.id, err)
			continue
		}
		p.mu.Lock()
		if p.sessions[s.id] == s && p.running {
			s.proc = proc
			proc = nil
		}
		p.mu.Unlock()
		if proc != nil {
			// The session ended, or the provider stopped, while the helper was starting.
			_ = proc.Kill()
		}
	}
}

// Serve takes over a connection whose first frame was helper_hello, and holds it until either side
// closes it. A hello naming a session the peer is not signed in to is refused.
func (p *Provider) Serve(conn net.Conn, peer hostinfo.User, first protocol.NativeMessage) {
	var hello protocol.HelperHello
	err := json.Unmarshal(first.Body, &hello)
	if err != nil {
		err = fmt.Errorf("helper_hello body: %w", err)
	} else {
		err = p.checkOwner(hello.SessionID, peer)
	}
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		p.cfg.Log.Printf("userhelper: helper_hello refused: %v", err)
		_ = localipc.WriteFrame(conn, refusal(first.ID, err))
		return
	}
	hc := &helperConn{conn: conn, results: make(chan protocol.NativeMessage, 1), done: make(chan struct{})}
	if !p.attach(hello.SessionID, peer, hc) {
		_ = localipc.WriteFrame(conn, refusal(first.ID, errors.New("the user-session helper is not running")))
		return
	}
	if err := localipc.WriteFrame(conn, message(protocol.TypeAck, first.ID, protocol.Ack{ID: first.ID, Detail: fmt.Sprintf("session=%d", hello.SessionID)})); err != nil {
		p.detach(hello.SessionID, hc)
		return
	}
	p.cfg.Log.Printf("userhelper: helper connected in session %d (pid %d)", hello.SessionID, hello.PID)
	hc.read()
	p.detach(hello.SessionID, hc)
	p.cfg.Log.Printf("userhelper: helper in session %d disconnected", hello.SessionID)
}

// checkOwner accepts a peer that is the user signed in to the session.
func (p *Provider) checkOwner(sessionID uint32, peer hostinfo.User) error {
	if p.cfg.Platform == nil {
		return errors.New("user-session helpers are not supported on this platform")
	}
	owner, err := p.cfg.Platform.Owner(sessionID)
	if err != nil {
		return fmt.Errorf("session %d: %w", sessionID, err)
	}
	if !sameUser(owner, peer) {
		return fmt.Errorf("%w: session %d", ErrNotSessionOwner, sessionID)
	}
	return nil
}

// attach makes hc the session's helper connection; a newer connection replaces an older one.
func (p *Provider) attach(sessionID uint32, peer hostinfo.User, hc *helperConn) bool {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return false
	}
	s := p.sessions[sessionID]
	if s == nil {
		s = &session{id: sessionID, user: peer}
		p.sessions[sessionID] = s
	}
	old := s.conn
	s.conn = hc
	p.lastSuccess = p.cfg.Clock()
	p.mu.Unlock()
	if old != nil {
		old.close()
	}
	return true
}

func (p *Provider) detach(sessionID uint32, hc *helperConn) {
	hc.close()
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.sessions[sessionID]; s != nil && s.conn == hc {
		s.conn = nil
	}
}

// Notify shows n to the user signed in to the session, through that session's helper, and returns
// once the helper says whether it was shown.
func (p *Provider) Notify(sessionID uint32, n protocol.Notify) error {
	if err := n.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	var hc *helperConn
	if s := p.sessions[sessionID]; s != nil {
		hc = s.conn
	}
	p.mu.Unlock()
	if hc == nil {
		return ErrNoHelper
	}
	res, err := hc.notify(n)
	if err != nil {
		p.counters.Add(protocol.CounterErrors)
		return err
	}
	if !res.Shown {
		p.counters.Add(protocol.CounterErrors)
		return fmt.Errorf("userhelper: the notification was not shown: %s", res.Error)
	}
	p.mu.Lock()
	p.lastSuccess = p.cfg.Clock()
	p.mu.Unlock()
	return nil
}

// end disconnects and ends the session's helper.
func (s *session) end() {
	if s.conn != nil {
		s.conn.close()
	}
	if s.proc != nil {
		_ = s.proc.Kill()
	}
}

// helperConn is one connected helper. One notification is in flight at a time.
type helperConn struct {
	conn    net.Conn
	mu      sync.Mutex
	results chan protocol.NativeMessage
	done    chan struct{}
	once    sync.Once
}

// read delivers the helper's answers until the connection ends.
func (h *helperConn) read() {
	for {
		payload, err := localipc.ReadFrame(h.conn)
		if err != nil {
			return
		}
		var msg protocol.NativeMessage
		if json.Unmarshal(payload, &msg) != nil || msg.Type != protocol.TypeNotifyResult {
			continue
		}
		// An answer nobody waits for any more (its Notify timed out) is replaced by the next.
		select {
		case <-h.results:
		default:
		}
		h.results <- msg
	}
}

func (h *helperConn) notify(n protocol.Notify) (protocol.NotifyResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := newID()
	if err := localipc.WriteFrame(h.conn, message(protocol.TypeNotify, id, n)); err != nil {
		return protocol.NotifyResult{}, fmt.Errorf("userhelper: sending the notification: %w", err)
	}
	timer := time.NewTimer(notifyTimeout)
	defer timer.Stop()
	for {
		select {
		case msg := <-h.results:
			if msg.ID != id {
				continue
			}
			var res protocol.NotifyResult
			if err := json.Unmarshal(msg.Body, &res); err != nil {
				return res, fmt.Errorf("userhelper: notify_result body: %w", err)
			}
			return res, nil
		case <-h.done:
			return protocol.NotifyResult{}, ErrNoHelper
		case <-timer.C:
			return protocol.NotifyResult{}, errors.New("userhelper: the helper did not answer in time")
		}
	}
}

func (h *helperConn) close() {
	h.once.Do(func() {
		close(h.done)
		_ = h.conn.Close()
	})
}

func exited(p Process) bool {
	select {
	case <-p.Done():
		return true
	default:
		return false
	}
}

func within(ts []time.Time, after time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(after) {
			out = append(out, t)
		}
	}
	return out
}

// sameUser compares accounts by SID (the uid elsewhere), which Windows compares without case.
func sameUser(a, b hostinfo.User) bool {
	return a.SID != "" && strings.EqualFold(a.SID, b.SID)
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func message(typ, id string, body any) []byte {
	raw, _ := json.Marshal(body)
	out, _ := json.Marshal(protocol.NativeMessage{Type: typ, Version: protocol.Version, ID: id, Body: raw})
	return out
}

func refusal(id string, err error) []byte {
	return message(protocol.TypeRefusal, id, protocol.Refusal{Reason: protocol.RefusalMalformed, Message: err.Error()})
}
