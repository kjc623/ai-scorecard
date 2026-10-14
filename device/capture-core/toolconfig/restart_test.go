package toolconfig

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// lines is a logger that keeps what it is given.
type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, fmt.Sprintf(format, args...))
}

func (l *lines) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.all {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// shown is one notification the provider asked to show.
type shown struct {
	session uint32
	n       protocol.Notify
}

// restartRig is a running Claude Code provider over a managed file whose last change is at changed,
// with the processes procs running and notifications recorded in sent, or refused while refuse is
// set.
type restartRig struct {
	p       *Provider
	path    string
	changed time.Time
	log     *lines

	mu     sync.Mutex
	procs  []AppProcess
	asked  [][]string
	sent   []shown
	refuse bool
}

func newRestartRig(t *testing.T, notice policy.RestartNotice) *restartRig {
	t.Helper()
	w, path := newTestWriter(t)
	r := &restartRig{path: path, log: &lines{}}
	r.p = newProvider(supportedTool, w, Config{
		Token:      func() string { return testToken },
		Executable: func() (string, error) { return testExe, nil },
		Log:        r.log,
		Processes: func(apps []string) ([]AppProcess, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.asked = append(r.asked, apps)
			return slices.Clone(r.procs), nil
		},
		Notify: func(session uint32, n protocol.Notify) error {
			if err := n.Validate(); err != nil {
				return err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.refuse {
				return errors.New("no helper runs in the session")
			}
			r.sent = append(r.sent, shown{session, n})
			return nil
		},
	})
	r.setNotice(t, notice)
	if err := r.p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.p.Stop(context.Background()) })
	r.change(t, time.Date(2026, 10, 14, 5, 50, 0, 0, time.UTC))
	return r
}

func (r *restartRig) setNotice(t *testing.T, notice policy.RestartNotice) {
	t.Helper()
	b := otelBundle(protocol.ModeM1)
	b.Endpoint.RestartNotice = notice
	if err := r.p.ApplyPolicy(b); err != nil {
		t.Fatal(err)
	}
}

// change sets the managed file's last change to at.
func (r *restartRig) change(t *testing.T, at time.Time) {
	t.Helper()
	if err := os.Chtimes(r.path, at, at); err != nil {
		t.Fatal(err)
	}
	r.changed = at
}

func (r *restartRig) run(procs ...AppProcess) {
	r.mu.Lock()
	r.procs = procs
	r.mu.Unlock()
	r.p.checkRestart()
	// Notifications are shown in the background; wait for every one to be recorded.
	for {
		r.p.mu.Lock()
		n := len(r.p.notifying)
		r.p.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// take returns the notifications shown since the last take, by session.
func (r *restartRig) take() map[uint32]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[uint32]string{}
	for _, s := range r.sent {
		out[s.session] += s.n.Title + "|" + s.n.Body
	}
	r.sent = nil
	return out
}

func (r *restartRig) health(t *testing.T, when string, want protocol.CollectorState, detail protocol.Detail) {
	t.Helper()
	if h := r.p.Health(); h.State != want || h.Detail != detail {
		t.Fatalf("%s: health = %s/%s, want %s/%s", when, h.State, h.Detail, want, detail)
	}
}

// A process of Claude Code or the Claude desktop app that started before the managed file last
// changed is reported on the row while the notice includes the dashboard, and its session's person
// is told once which app to restart while it includes notify. A process started since, a process of
// another app, and the services' session are left alone.
func TestProviderReportsAppsThatNeedARestart(t *testing.T) {
	r := newRestartRig(t, policy.RestartNoticeDashboard)
	before, after := r.changed.Add(-10*time.Hour), r.changed.Add(time.Minute)
	terminal := AppProcess{PID: 10, Started: before, Session: 2, App: "claude_code"}
	desktop := AppProcess{PID: 20, Started: before, Session: 3, App: "claude_desktop"}
	codeTab := AppProcess{PID: 21, Started: before.Add(time.Minute), Session: 3, App: "claude_code", Parent: "claude_desktop"}
	fresh := AppProcess{PID: 30, Started: after, Session: 4, App: "claude_code"}
	other := AppProcess{PID: 40, Started: before, Session: 5, App: "codex"}
	service := AppProcess{PID: 50, Started: before, Session: 0, App: "claude_code"}

	r.run(fresh, other)
	r.health(t, "only a process started since the change", protocol.StateHealthy, protocol.DetailNone)
	if len(r.asked) != 1 || !slices.Equal(r.asked[0], []string{"claude_code", "claude_desktop"}) {
		t.Fatalf("asked for the processes of %v, want Claude Code's and the desktop app's", r.asked)
	}

	r.run(terminal, desktop, codeTab, fresh, other, service)
	r.health(t, "processes from before the change", protocol.StateDegraded, protocol.DetailRestartRequired)
	if got := r.take(); len(got) != 0 {
		t.Fatalf("the dashboard notice notified %v", got)
	}

	r.setNotice(t, policy.RestartNoticeNotify)
	r.health(t, "notify alone", protocol.StateHealthy, protocol.DetailNone)
	r.run(terminal, desktop, codeTab, fresh, other, service)
	want := map[uint32]string{
		2: "Restart Claude Code|Your organization has updated settings for Claude Code. Restart Claude Code to apply them.",
		3: "Restart Claude|Your organization has updated settings for Claude. Restart Claude to apply them.",
	}
	if got := r.take(); !maps.Equal(got, want) {
		t.Fatalf("notified %v\nwant     %v", got, want)
	}
	r.run(terminal, desktop, codeTab, fresh, other, service)
	if got := r.take(); len(got) != 0 {
		t.Fatalf("notified again for the same change: %v", got)
	}

	// A later change is told about again; one that is not shown is tried at each check, and its
	// failure logged once.
	r.change(t, r.changed.Add(time.Hour))
	r.mu.Lock()
	r.refuse = true
	r.mu.Unlock()
	r.run(terminal)
	r.run(terminal)
	if n := r.log.count("could not tell session 2 to restart Claude Code"); n != 1 {
		t.Fatalf("the failure was logged %d times, want once", n)
	}
	r.mu.Lock()
	r.refuse = false
	r.mu.Unlock()
	r.run(terminal)
	if got := r.take(); got[2] == "" || len(got) != 1 {
		t.Fatalf("after the helper came back, notified %v; want session 2 alone", got)
	}

	r.setNotice(t, policy.RestartNoticeBoth)
	r.run(terminal)
	r.health(t, "both", protocol.StateDegraded, protocol.DetailRestartRequired)
	if got := r.take(); len(got) != 0 {
		t.Fatalf("notified again for the same change: %v", got)
	}

	restarted := AppProcess{PID: 11, Started: r.changed.Add(time.Minute), Session: 2, App: "claude_code"}
	r.run(restarted)
	r.health(t, "the stale processes ended", protocol.StateHealthy, protocol.DetailNone)

	r.setNotice(t, policy.RestartNoticeOff)
	asked := len(r.asked)
	r.run(terminal)
	r.health(t, "notice off", protocol.StateHealthy, protocol.DetailNone)
	if len(r.asked) != asked {
		t.Fatal("the processes were listed with the notice off")
	}
}

// Without the configuration in place nothing is looked for: a restart would not help.
func TestProviderLooksForRestartsOnlyWithTheConfigurationInPlace(t *testing.T) {
	r := newRestartRig(t, policy.RestartNoticeBoth)
	stale := AppProcess{PID: 10, Started: r.changed.Add(-time.Hour), Session: 2, App: "claude_code"}
	_ = r.p.Stop(context.Background())
	r.run(stale)
	if len(r.asked) != 0 {
		t.Fatal("a stopped provider listed the processes")
	}
	if err := r.p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.p.mu.Lock()
	r.p.inPlaceSince = time.Time{}
	r.p.mu.Unlock()
	r.run(stale)
	if len(r.asked) != 0 || len(r.take()) != 0 {
		t.Fatal("processes were listed while the configuration was not in place")
	}
}

// A process list that cannot be read is logged once until it can be read again.
func TestProviderLogsAnUnreadableProcessListOnce(t *testing.T) {
	w, _ := newTestWriter(t)
	log := &lines{}
	fail := true
	p := newProvider(supportedTool, w, Config{
		Token:      func() string { return testToken },
		Executable: func() (string, error) { return testExe, nil },
		Log:        log,
		Processes: func([]string) ([]AppProcess, error) {
			if fail {
				return nil, errors.New("access is denied")
			}
			return nil, nil
		},
	})
	b := otelBundle(protocol.ModeM1)
	b.Endpoint.RestartNotice = policy.RestartNoticeDashboard
	_ = p.ApplyPolicy(b)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	p.checkRestart()
	p.checkRestart()
	fail = false
	p.checkRestart()
	fail = true
	p.checkRestart()
	if n := log.count("listing the running processes"); n != 2 {
		t.Fatalf("logged %d times, want once per run of failures", n)
	}
}

// Each restart app's name makes a notification the helper can show.
func TestRestartNotificationsAreValid(t *testing.T) {
	for _, tl := range []tool{claudeCodeTool, codexTool, copilotTool, cursorTool} {
		for app, name := range tl.restartApps {
			if err := restartNotification(name).Validate(); err != nil {
				t.Errorf("%s %s: %v", tl.key, app, err)
			}
		}
	}
}
