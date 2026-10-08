// Package component supervises a child process that capture-core runs from its install directory.
// A Supervisor starts the child, starts it again with exponential backoff when it exits, stops
// restarting it for a while when it keeps exiting, reports its health row, and kills it, with
// anything it started, when it stops or when the service dies.
//
// A child's output is never logged, because it could carry content: its stdin and stdout are
// either its protocol channel, reached through Dial, or discarded, and its stderr is discarded.
package component

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	// The delay before a restart starts at backoffBase and doubles with each restart up to
	// backoffCap. It starts again from backoffBase after a child has run for restartWindow.
	backoffBase = time.Second
	backoffCap  = time.Minute
	// At most maxRestarts restarts in restartWindow; then the component is degraded with
	// component_crash_loop and started again once restartWindow has passed.
	maxRestarts   = 5
	restartWindow = 10 * time.Minute
	// readyTimeout bounds one Ready check. A child that has not passed it by then is killed.
	readyTimeout = 30 * time.Second
)

var (
	// ErrNotReady refuses a Dial made outside the child's Ready check.
	ErrNotReady = errors.New("component: the child's stdio is reached only from its Ready check")
	// ErrNoStdio refuses a Dial on a component whose stdio is not its protocol channel.
	ErrNoStdio = errors.New("component: the child's stdio is not its protocol channel")
)

// Spec describes one supervised component.
type Spec struct {
	// Collector names the component's health row.
	Collector protocol.Collector
	// Path is the executable, which the installer lays down in the install directory.
	Path string
	Args []string
	// Stdio makes the child's stdin and stdout its protocol channel, which Ready reaches through
	// Dial. Otherwise both are discarded.
	Stdio bool
	// Ready, when set, runs after each start; the child counts as up once it returns nil. A child
	// that fails it is killed and restarted like one that exited.
	Ready func(ctx context.Context) error
}

// Supervisor keeps one component running. It is the component's core.Provider.
type Supervisor struct {
	spec Spec
	log  core.Logger
	now  func() time.Time
	// after is the restart timer.
	after    func(time.Duration) <-chan time.Time
	counters *core.CounterSet

	mu          sync.Mutex
	running     bool
	cancel      context.CancelFunc
	done        chan struct{} // closed when the restart loop has ended
	child       *child
	ready       bool
	crashLoop   bool
	since       time.Time
	lastSuccess time.Time
	restarts    int
	recent      []time.Time // restarts within restartWindow
}

// New returns a stopped supervisor for spec.
func New(spec Spec, log core.Logger) *Supervisor {
	if log == nil {
		log = nopLogger{}
	}
	s := &Supervisor{spec: spec, log: log, now: time.Now, after: time.After}
	s.since = s.now()
	s.counters = core.NewCounterSet(s.since)
	return s
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Name is the component's collector.
func (s *Supervisor) Name() protocol.Collector { return s.spec.Collector }

// ApplyPolicy changes nothing: a supervised component is not switched by policy.
func (s *Supervisor) ApplyPolicy(policy.Bundle) error { return nil }

// Start starts the child and returns once it has passed Ready or failed. A failure is handled
// like an exit, by a restart with backoff, so Start itself never fails.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	// The restart loop outlives the context that started it; Stop ends it.
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.running = true
	s.cancel = cancel
	done := make(chan struct{})
	s.done = done
	s.since = s.now()
	s.restarts, s.recent, s.crashLoop = 0, nil, false
	s.mu.Unlock()

	c := s.launch(life)
	go s.loop(life, c, done)
	return nil
}

// Stop kills the child and everything it started, and ends the restart loop.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	cancel, done, c := s.cancel, s.done, s.child
	s.mu.Unlock()

	cancel()
	if c != nil {
		c.kill()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("component: %s did not stop: %w", s.spec.Collector, ctx.Err())
	}
}

// Restarts is how many times the child has been started again since Start.
func (s *Supervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// Health is healthy while a child that passed Ready runs, degraded with component_crash_loop
// while restarts are suspended, degraded with host_unreachable while the child is starting or
// waiting for a restart, and absent when the supervisor is not running.
func (s *Supervisor) Health() core.Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.running:
		return core.Absent(protocol.DetailNone, s.since, s.counters)
	case s.crashLoop:
		return core.Degraded(protocol.DetailComponentCrashLoop, s.since, s.lastSuccess, s.counters)
	case s.child != nil && s.ready:
		return core.Healthy(protocol.DetailNone, s.since, s.lastSuccess, s.counters)
	default:
		return core.Degraded(protocol.DetailHostUnreachable, s.since, s.lastSuccess, s.counters)
	}
}

// Dial returns the stdio of the child whose Ready check is running, as a connection; ctx must be
// the context the supervisor passed to Ready. Each child's stdio is handed out once, and closing
// it kills the child, which the supervisor then restarts. A Dial from anywhere else fails, so a
// caller reconnecting on its own cannot take a restarted child's stdio before its Ready check.
func (s *Supervisor) Dial(ctx context.Context) (net.Conn, error) {
	if !s.spec.Stdio {
		return nil, ErrNoStdio
	}
	c, _ := ctx.Value(readyKey{}).(*child)
	if c == nil {
		return nil, ErrNotReady
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dialled {
		return nil, ErrNotReady
	}
	c.dialled = true
	return c.conn, nil
}

type readyKey struct{}

// loop waits for each child to exit and starts the next one, within the restart budget.
func (s *Supervisor) loop(ctx context.Context, c *child, done chan struct{}) {
	defer close(done)
	delay := backoffBase
	for {
		if c != nil {
			select {
			case <-c.exited:
			case <-ctx.Done():
				c.kill()
				<-c.exited
				s.forget(c)
				return
			}
			s.forget(c)
			if s.now().Sub(c.started) >= restartWindow {
				delay = backoffBase
			}
		}
		if ctx.Err() != nil {
			return
		}
		if s.exhausted() {
			s.log.Printf("component: %s was restarted %d times in %s; restarts resume in %s", s.spec.Collector, maxRestarts, restartWindow, restartWindow)
			if !s.wait(ctx, restartWindow) {
				return
			}
			s.mu.Lock()
			s.recent = nil
			s.mu.Unlock()
		} else {
			if !s.wait(ctx, delay) {
				return
			}
			delay = min(delay*2, backoffCap)
		}
		s.mu.Lock()
		s.restarts++
		s.recent = append(s.recent, s.now())
		s.mu.Unlock()
		c = s.launch(ctx)
	}
}

// exhausted drops the restarts that have left the window and reports whether the budget is spent,
// in which case restarts are suspended.
func (s *Supervisor) exhausted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-restartWindow)
	kept := s.recent[:0]
	for _, t := range s.recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.recent = kept
	if len(s.recent) < maxRestarts {
		return false
	}
	s.crashLoop = true
	return true
}

func (s *Supervisor) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-s.after(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// forget records that c has exited.
func (s *Supervisor) forget(c *child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == c {
		s.child = nil
		s.ready = false
	}
}

// launch starts one child and runs its Ready check. It returns nil when the child did not start.
func (s *Supervisor) launch(ctx context.Context) *child {
	c, err := s.startChild()
	if err != nil {
		s.log.Printf("component: %s did not start: %v", s.spec.Collector, err)
		return nil
	}
	s.mu.Lock()
	s.child = c
	s.ready = false
	s.mu.Unlock()

	if s.spec.Ready != nil {
		readyCtx, cancel := context.WithTimeout(context.WithValue(ctx, readyKey{}, c), readyTimeout)
		// A child that hangs in its Ready check is killed, which ends any read from it.
		stopKill := context.AfterFunc(readyCtx, c.kill)
		err := s.spec.Ready(readyCtx)
		stopKill()
		cancel()
		if err != nil {
			s.log.Printf("component: %s did not become ready; it is restarted: %v", s.spec.Collector, err)
			c.kill()
			return c
		}
	}
	s.mu.Lock()
	if s.child == c {
		s.ready = true
		s.crashLoop = false
		s.lastSuccess = s.now()
	}
	s.mu.Unlock()
	return c
}

// startChild starts the process, contained so that it dies with the service.
func (s *Supervisor) startChild() (*child, error) {
	cmd := exec.Command(s.spec.Path, s.spec.Args...)
	cmd.SysProcAttr = sysProcAttr()
	// A nil Stdin, Stdout or Stderr is the null device: output not used as the protocol channel is
	// discarded, never logged.
	var conn *stdioConn
	if s.spec.Stdio {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = stdin.Close()
			return nil, err
		}
		conn = &stdioConn{stdin: stdin, stdout: stdout, name: s.spec.Path}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	box, err := contain(cmd.Process)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	c := &child{cmd: cmd, box: box, conn: conn, exited: make(chan struct{}), started: s.now()}
	if conn != nil {
		conn.kill = c.kill
	}
	go func() {
		_ = cmd.Wait()
		c.mu.Lock()
		c.box.release()
		c.released = true
		c.mu.Unlock()
		close(c.exited)
	}()
	return c, nil
}

// child is one started process.
type child struct {
	cmd     *exec.Cmd
	conn    *stdioConn
	exited  chan struct{}
	started time.Time

	mu       sync.Mutex
	box      containment
	released bool
	dialled  bool
}

// kill ends the child and everything it started. It does nothing once the child has been reaped.
func (c *child) kill() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return
	}
	if err := c.box.kill(); err != nil {
		_ = c.cmd.Process.Kill()
	}
}

// containment ties a child, and the processes it starts, to the service.
type containment interface {
	kill() error
	release()
}
