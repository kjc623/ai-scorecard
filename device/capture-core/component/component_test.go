package component

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// The test binary is its own fake child: run with childArg first, it behaves as the mode after it
// says instead of running the tests.
const childArg = "component-test-child"

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == childArg {
		os.Exit(runChild(os.Args[2]))
	}
	os.Exit(m.Run())
}

func runChild(mode string) int {
	switch mode {
	case "exit":
		return 3
	case "sleep":
		time.Sleep(time.Hour)
		return 0
	case "echo":
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			fmt.Fprintln(os.Stdout, in.Text())
		}
		return 0
	default:
		return 2
	}
}

func childSpec(t *testing.T, mode string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return Spec{Collector: protocol.CollectorClassifierHost, Path: exe, Args: []string{childArg, mode}}
}

// fakeClock stands in for the restart timer: each wait the supervisor starts is handed to the
// test, which advances the clock by it and lets it fire.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	waits chan pendingWait
}

type pendingWait struct {
	d    time.Duration
	fire chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC), waits: make(chan pendingWait, 16)}
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.waits <- pendingWait{d: d, fire: ch}
	return ch
}

// next returns the wait the supervisor starts next.
func (f *fakeClock) next(t *testing.T) pendingWait {
	t.Helper()
	select {
	case w := <-f.waits:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor started no restart timer")
		return pendingWait{}
	}
}

// fire advances the clock by the wait and ends it.
func (f *fakeClock) fire(w pendingWait) {
	f.mu.Lock()
	f.t = f.t.Add(w.d)
	now := f.t
	f.mu.Unlock()
	w.fire <- now
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) { l.t.Logf(format, args...) }

func startSupervised(t *testing.T, spec Spec) (*Supervisor, *fakeClock) {
	t.Helper()
	s := New(spec, testLogger{t})
	clock := newFakeClock()
	s.now, s.after = clock.now, clock.after
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return s, clock
}

func assertHealth(t *testing.T, s *Supervisor, state protocol.CollectorState, detail protocol.Detail) {
	t.Helper()
	if h := s.Health(); h.State != state || h.Detail != detail {
		t.Fatalf("health = %s/%q, want %s/%q", h.State, h.Detail, state, detail)
	}
}

// A child that exits is started again after a delay that doubles from 1 s.
func TestAnExitedChildIsRestartedWithBackoff(t *testing.T) {
	s, clock := startSupervised(t, childSpec(t, "exit"))
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		w := clock.next(t)
		if w.d != want {
			t.Fatalf("restart %d waits %s, want %s", i+1, w.d, want)
		}
		if got := s.Restarts(); got != i {
			t.Fatalf("restarts before restart %d = %d", i+1, got)
		}
		assertHealth(t, s, protocol.StateDegraded, protocol.DetailHostUnreachable)
		clock.fire(w)
	}
}

// The sixth exit in ten minutes suspends restarts: the row is degraded with component_crash_loop
// until restarts resume ten minutes later. The delay keeps doubling up to 60 s, and five more
// restarts suspend them again.
func TestTheCrashLoopLimitDegradesTheComponent(t *testing.T) {
	s, clock := startSupervised(t, childSpec(t, "exit"))
	for range maxRestarts {
		clock.fire(clock.next(t))
	}
	w := clock.next(t)
	if w.d != restartWindow {
		t.Fatalf("after %d restarts the supervisor waits %s, want the %s suspension", maxRestarts, w.d, restartWindow)
	}
	if got := s.Restarts(); got != maxRestarts {
		t.Fatalf("restarts = %d, want %d", got, maxRestarts)
	}
	assertHealth(t, s, protocol.StateDegraded, protocol.DetailComponentCrashLoop)

	clock.fire(w)
	for i, want := range []time.Duration{32 * time.Second, time.Minute, time.Minute, time.Minute} {
		w := clock.next(t)
		if got := s.Restarts(); got != maxRestarts+1+i {
			t.Fatalf("restarts after the suspension = %d, want %d", got, maxRestarts+1+i)
		}
		if w.d != want {
			t.Fatalf("restart %d after the suspension waits %s, want %s", i+1, w.d, want)
		}
		// The child started after the suspension came up before it exited, which ends the crash loop.
		assertHealth(t, s, protocol.StateDegraded, protocol.DetailHostUnreachable)
		clock.fire(w)
	}
	if w := clock.next(t); w.d != restartWindow {
		t.Fatalf("five more restarts wait %s, want the %s suspension again", w.d, restartWindow)
	}
}

// Stop kills a running child and ends the restarts.
func TestStopKillsTheChild(t *testing.T) {
	s := New(childSpec(t, "sleep"), testLogger{t})
	clock := newFakeClock()
	s.now, s.after = clock.now, clock.after
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertHealth(t, s, protocol.StateHealthy, protocol.DetailNone)
	s.mu.Lock()
	c := s.child
	s.mu.Unlock()
	if c == nil {
		t.Fatal("no child is running")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-c.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the child is still running after Stop")
	}
	if c.cmd.ProcessState == nil || c.cmd.ProcessState.Success() {
		t.Fatalf("the child ended with %v, want killed", c.cmd.ProcessState)
	}
	select {
	case w := <-clock.waits:
		t.Fatalf("a stopped supervisor scheduled a restart in %s", w.d)
	default:
	}
	if got := s.Restarts(); got != 0 {
		t.Fatalf("restarts = %d after Stop, want 0", got)
	}
	assertHealth(t, s, protocol.StateAbsent, protocol.DetailNone)
}

// Ready reaches the child's stdio through Dial; nothing else can.
func TestReadyReachesTheChildOverStdio(t *testing.T) {
	var s *Supervisor
	spec := childSpec(t, "echo")
	spec.Stdio = true
	spec.Ready = func(ctx context.Context) error {
		conn, err := s.Dial(ctx)
		if err != nil {
			return err
		}
		if _, err := s.Dial(ctx); err == nil {
			return errors.New("the child's stdio was handed out twice")
		}
		if _, err := fmt.Fprintln(conn, "ping"); err != nil {
			return err
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return err
		}
		if line != "ping\n" {
			return fmt.Errorf("the child answered %q", line)
		}
		return nil
	}
	s = New(spec, testLogger{t})
	clock := newFakeClock()
	s.now, s.after = clock.now, clock.after
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(context.Background())
	assertHealth(t, s, protocol.StateHealthy, protocol.DetailNone)
	if _, err := s.Dial(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Dial outside Ready = %v, want ErrNotReady", err)
	}
}

// A child that fails its Ready check is killed and restarted like one that exited.
func TestAFailedReadyCheckRestartsTheChild(t *testing.T) {
	spec := childSpec(t, "sleep")
	spec.Ready = func(context.Context) error { return errors.New("handshake refused") }
	s, clock := startSupervised(t, spec)
	w := clock.next(t)
	if w.d != backoffBase {
		t.Fatalf("restart waits %s, want %s", w.d, backoffBase)
	}
	assertHealth(t, s, protocol.StateDegraded, protocol.DetailHostUnreachable)
}
