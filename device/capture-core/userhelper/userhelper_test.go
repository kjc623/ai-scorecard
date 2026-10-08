package userhelper

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/protocol"
)

// fakeProcess is a helper the fake platform started; it exits when the test or Kill says so.
type fakeProcess struct {
	pid  uint32
	once sync.Once
	done chan struct{}
}

func (p *fakeProcess) PID() uint32           { return p.pid }
func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) Kill() error           { p.exit(); return nil }
func (p *fakeProcess) exit()                 { p.once.Do(func() { close(p.done) }) }
func (p *fakeProcess) killed() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// fakePlatform is a machine whose sessions a test sets.
type fakePlatform struct {
	mu        sync.Mutex
	sessions  []Session
	enumErr   error
	launchErr error
	launched  map[uint32][]*fakeProcess
	nextPID   uint32
}

func newFakePlatform(sessions ...Session) *fakePlatform {
	return &fakePlatform{sessions: sessions, launched: map[uint32][]*fakeProcess{}, nextPID: 100}
}

func (f *fakePlatform) set(sessions ...Session) {
	f.mu.Lock()
	f.sessions = sessions
	f.mu.Unlock()
}

func (f *fakePlatform) Sessions() ([]Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Session(nil), f.sessions...), f.enumErr
}

func (f *fakePlatform) Owner(id uint32) (hostinfo.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.ID == id {
			return s.User, nil
		}
	}
	return hostinfo.User{}, errors.New("nobody is signed in to the session")
}

func (f *fakePlatform) Launch(id uint32) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	f.nextPID++
	p := &fakeProcess{pid: f.nextPID, done: make(chan struct{})}
	f.launched[id] = append(f.launched[id], p)
	return p, nil
}

// launches is the helpers started in a session, oldest first.
func (f *fakePlatform) launches(id uint32) []*fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeProcess(nil), f.launched[id]...)
}

// fakeClock is a clock a test moves.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func me(t *testing.T) hostinfo.User {
	t.Helper()
	u, err := hostinfo.SystemUserSources().Process()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

var someoneElse = hostinfo.User{SID: "S-1-12-1-1111111111-2222222222-3333333333-4444444444", Account: `AzureAD\Someone`}

// started is a running provider over the fake platform.
func started(t *testing.T, f *fakePlatform, clock *fakeClock) *Provider {
	t.Helper()
	cfg := Config{Platform: f}
	if clock != nil {
		cfg.Clock = clock.Now
	}
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	return p
}

// endpoint serves the provider on a local endpoint the way the service does: a connection that
// opens with helper_hello is the provider's.
func endpoint(t *testing.T, p *Provider) string {
	t.Helper()
	addr := testAddr(t)
	srv := localipc.NewServer(addr, func(conn net.Conn, peer hostinfo.User) {
		payload, err := localipc.ReadFrame(conn)
		if err != nil {
			return
		}
		var first protocol.NativeMessage
		if json.Unmarshal(payload, &first) == nil && first.Type == protocol.TypeHelperHello {
			p.Serve(conn, peer, first)
		}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return addr
}

// helper runs a helper for session id against addr, recording what it was asked to show.
type helper struct {
	mu      sync.Mutex
	shown   []protocol.Notify
	showErr error
	done    chan error
}

func runTestHelper(t *testing.T, addr string, id uint32) *helper {
	t.Helper()
	h := &helper{done: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialAny(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		h.done <- RunHelper(conn, protocol.HelperHello{SessionID: id, PID: 4242}, func(n protocol.Notify) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.showErr != nil {
				return h.showErr
			}
			h.shown = append(h.shown, n)
			return nil
		})
	}()
	return h
}

func (h *helper) notifications() []protocol.Notify {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.Notify(nil), h.shown...)
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func connected(p *Provider, id uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[id]
	return s != nil && s.conn != nil
}

// A helper_hello from a user who is not signed in to the session it names is refused, and the
// session's notifications never reach that user.
func TestHelperHelloFromAUserWhoDoesNotOwnTheSessionIsRefused(t *testing.T) {
	f := newFakePlatform(Session{ID: 7, User: someoneElse})
	p := started(t, f, nil)
	addr := endpoint(t, p)

	h := runTestHelper(t, addr, 7)
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "not signed in to the session") {
			t.Fatalf("helper ended with %v, want a refused helper_hello", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hello was neither accepted nor refused")
	}
	if connected(p, 7) {
		t.Fatal("the session took a helper run by another user")
	}
	if err := p.Notify(7, protocol.Notify{Title: "t", Body: "b"}); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("Notify = %v, want ErrNoHelper", err)
	}
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailHelperUnavailable {
		t.Fatalf("health = %s/%s, want degraded/helper_unavailable", h.State, h.Detail)
	}
	if p.Health().Counters[protocol.CounterErrors] == 0 {
		t.Fatal("the refusal was not counted")
	}

	// A session nobody is signed in to is refused the same way.
	h = runTestHelper(t, addr, 99)
	if err := <-h.done; err == nil {
		t.Fatal("a hello naming a session with no user was accepted")
	}
}

// The session's own user is accepted, and Notify returns once the helper says it showed the
// notification; a notification it could not show is an error with its reason.
func TestNotifyReachesTheSessionsHelper(t *testing.T) {
	f := newFakePlatform(Session{ID: 3, User: me(t)})
	p := started(t, f, nil)
	addr := endpoint(t, p)
	h := runTestHelper(t, addr, 3)
	waitFor(t, "the helper to connect", func() bool { return connected(p, 3) })
	if got := p.Health(); got.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s with every session's helper connected, want healthy", got.State, got.Detail)
	}

	n := protocol.Notify{Title: "Prompt blocked", Body: "This prompt carries a payment card number.", Link: "https://intranet.example.com/ai"}
	if err := p.Notify(3, n); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := h.notifications(); len(got) != 1 || got[0] != n {
		t.Fatalf("shown = %+v, want %+v", got, n)
	}

	h.mu.Lock()
	h.showErr = errors.New("toasts are switched off")
	h.mu.Unlock()
	if err := p.Notify(3, n); err == nil || !strings.Contains(err.Error(), "toasts are switched off") {
		t.Fatalf("Notify = %v, want the helper's reason", err)
	}
	if err := p.Notify(3, protocol.Notify{Title: strings.Repeat("x", protocol.MaxNotifyTitle+1), Body: "b"}); err == nil {
		t.Fatal("an overlong title was sent")
	}
	if err := p.Notify(4, n); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("Notify to a session with no helper = %v, want ErrNoHelper", err)
	}

	// Stopping the provider disconnects the helper, which then ends cleanly.
	_ = p.Stop(context.Background())
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("helper ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper outlived the provider")
	}
}

// A helper is started in every signed-in session, a helper that exits is started again at most
// five times in ten minutes, and then the session reports helper_unavailable until the window
// allows another start.
func TestHelpersAreStartedAndRestartedWithinTheBudget(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
	f := newFakePlatform(Session{ID: 1, User: me(t)}, Session{ID: 2, User: someoneElse})
	p := started(t, f, clock)
	if len(f.launches(1)) != 1 || len(f.launches(2)) != 1 {
		t.Fatalf("launches at start = %d, %d; want one per session", len(f.launches(1)), len(f.launches(2)))
	}
	// A live helper is not started twice.
	p.Reconcile(context.Background())
	if len(f.launches(1)) != 1 {
		t.Fatalf("a running helper was started again: %d launches", len(f.launches(1)))
	}
	for i := 1; i <= maxRestarts; i++ {
		f.launches(1)[i-1].exit()
		clock.advance(15 * time.Second)
		p.Reconcile(context.Background())
		if got := len(f.launches(1)); got != i+1 {
			t.Fatalf("after exit %d: launches = %d, want %d", i, got, i+1)
		}
	}
	f.launches(1)[maxRestarts].exit()
	clock.advance(15 * time.Second)
	p.Reconcile(context.Background())
	if got := len(f.launches(1)); got != maxRestarts+1 {
		t.Fatalf("the helper was restarted past the budget: %d launches", got)
	}
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailHelperUnavailable {
		t.Fatalf("health = %s/%s, want degraded/helper_unavailable", h.State, h.Detail)
	}
	if len(f.launches(2)) != 1 {
		t.Fatal("one session's restarts were charged to another")
	}
	clock.advance(restartWindow)
	p.Reconcile(context.Background())
	if got := len(f.launches(1)); got != maxRestarts+2 {
		t.Fatalf("no restart once the window passed: %d launches", got)
	}
}

// The helper of a session that ended, or whose user changed, is ended; a new user gets a new
// helper and a fresh restart budget.
func TestHelpersFollowTheSessions(t *testing.T) {
	f := newFakePlatform(Session{ID: 1, User: me(t)}, Session{ID: 2, User: someoneElse})
	p := started(t, f, nil)
	first1, first2 := f.launches(1)[0], f.launches(2)[0]

	f.set(Session{ID: 2, User: me(t)})
	p.Reconcile(context.Background())
	if !first1.killed() {
		t.Fatal("the helper of a signed-out session is still running")
	}
	if !first2.killed() || len(f.launches(2)) != 2 {
		t.Fatalf("a session whose user changed kept its helper (killed=%v, launches=%d)", first2.killed(), len(f.launches(2)))
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.launches(2)[1].killed() {
		t.Fatal("Stop left a helper running")
	}
}

// A failure to list the sessions or start a helper is degraded and counted, never healthy.
func TestFailuresAreDegraded(t *testing.T) {
	f := newFakePlatform(Session{ID: 1, User: me(t)})
	f.launchErr = errors.New("CreateProcessAsUserW: access denied")
	p := started(t, f, nil)
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailHelperUnavailable || h.Counters[protocol.CounterErrors] != 1 {
		t.Fatalf("health after a failed start = %s/%s errors=%d", h.State, h.Detail, h.Counters[protocol.CounterErrors])
	}
	f.mu.Lock()
	f.enumErr = errors.New("WTSEnumerateSessionsW: access denied")
	f.mu.Unlock()
	p.Reconcile(context.Background())
	if h := p.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailHelperUnavailable {
		t.Fatalf("health after a failed enumeration = %s/%s", h.State, h.Detail)
	}
}

// With no signed-in session there is nothing to serve, which is healthy once the sessions were
// listed.
func TestNoSessionIsHealthy(t *testing.T) {
	p := started(t, newFakePlatform(), nil)
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health = %s/%s", h.State, h.Detail)
	}
}

// Where the platform has no helper the provider is absent with helper_unavailable, and is never
// switched by policy.
func TestNoPlatformIsAbsent(t *testing.T) {
	p := New(Config{})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailHelperUnavailable {
		t.Fatalf("health = %s/%s, want absent/helper_unavailable", h.State, h.Detail)
	}
	if p.Name() != protocol.CollectorUserHelper {
		t.Fatalf("collector = %q", p.Name())
	}
	var provider any = p
	if _, toggled := provider.(core.Toggled); toggled {
		t.Fatal("the helper provider is switched by policy")
	}
}

// The helper answers a notification it was not asked to show correctly with the reason, and shows
// nothing.
func TestHelperRefusesAnInvalidNotify(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	var shown atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- RunHelper(client, protocol.HelperHello{SessionID: 1, PID: 2}, func(protocol.Notify) error { shown.Add(1); return nil })
	}()
	hello, err := localipc.ReadFrame(server)
	if err != nil {
		t.Fatal(err)
	}
	var msg protocol.NativeMessage
	if json.Unmarshal(hello, &msg) != nil || msg.Type != protocol.TypeHelperHello {
		t.Fatalf("first frame = %s", hello)
	}
	_ = localipc.WriteFrame(server, message(protocol.TypeAck, msg.ID, protocol.Ack{}))
	_ = localipc.WriteFrame(server, message(protocol.TypeNotify, "n1", protocol.Notify{Title: "t", Body: "a\x07b"}))
	answer, err := localipc.ReadFrame(server)
	if err != nil {
		t.Fatal(err)
	}
	var res protocol.NotifyResult
	if json.Unmarshal(answer, &msg) != nil || msg.Type != protocol.TypeNotifyResult || msg.ID != "n1" || json.Unmarshal(msg.Body, &res) != nil {
		t.Fatalf("answer = %s", answer)
	}
	if res.Shown || res.Error == "" || shown.Load() != 0 {
		t.Fatalf("result = %+v, shown %d times; want refused and not shown", res, shown.Load())
	}
	server.Close()
	if err := <-done; err != nil {
		t.Fatalf("helper ended with %v", err)
	}
}

// The toast document is ASCII, well-formed, carries the title and the body as its two text lines,
// and offers the link as a protocol-activated button.
func TestToastXML(t *testing.T) {
	n := protocol.Notify{Title: `Blocked <"AI"> & 'co' ✓`, Body: "Line one\nLine two – 😀", Link: "https://intranet.example.com/ai?a=1&b=2"}
	doc := toastXML(n)
	for i := 0; i < len(doc); i++ {
		if doc[i] < 0x20 || doc[i] > 0x7e {
			t.Fatalf("byte %d of the document is %#x, not printable ASCII", i, doc[i])
		}
	}
	var parsed struct {
		XMLName xml.Name `xml:"toast"`
		Binding struct {
			Template string   `xml:"template,attr"`
			Text     []string `xml:"text"`
		} `xml:"visual>binding"`
		Actions []struct {
			Content        string `xml:"content,attr"`
			ActivationType string `xml:"activationType,attr"`
			Arguments      string `xml:"arguments,attr"`
		} `xml:"actions>action"`
	}
	if err := xml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, doc)
	}
	if parsed.Binding.Template != "ToastGeneric" || len(parsed.Binding.Text) != 2 || parsed.Binding.Text[0] != n.Title || parsed.Binding.Text[1] != n.Body {
		t.Fatalf("binding = %+v", parsed.Binding)
	}
	if len(parsed.Actions) != 1 || parsed.Actions[0].ActivationType != "protocol" || parsed.Actions[0].Arguments != n.Link {
		t.Fatalf("actions = %+v", parsed.Actions)
	}
	if strings.Contains(toastXML(protocol.Notify{Title: "t", Body: "b"}), "<actions>") {
		t.Fatal("a notification without a link has a button")
	}
	if AppUserModelID == "" || strings.ContainsAny(AppUserModelID, " \\/") || len(AppUserModelID) > 128 {
		t.Fatalf("AppUserModelID %q is not a valid AppUserModelID", AppUserModelID)
	}
}
