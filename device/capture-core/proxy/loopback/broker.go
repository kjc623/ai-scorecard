package loopback

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// Pipeline is the capture-core pipeline as the broker uses it: one call resolves the mode
// *before* the body is buffered, the other hands the observation over. Resolving first is
// §11.2's ordering: at M0 the broker forwards the body without retaining it.
type Pipeline interface {
	ResolveMode(q core.ScopeQuery) core.Resolution
	Process(ctx context.Context, obs core.Observation) (core.Outcome, error)
}

// AgentInfo is what the broker knows about the user the port serves.
type AgentInfo struct {
	Population string
	UserRef    string
}

// Config is the broker's policy data plus its seams. Every timing here is policy data (§6.3)
// so a bad configuration is fixed by a bundle rather than a release.
type Config struct {
	// Ports are the per-tool bundle entries: fingerprint, port, mode, upstream and preflight
	// path. The broker never claims a port that is not in this list.
	Ports []policy.LoopbackPort

	// ProbeInterval is the cheap TCP liveness probe; PreflightInterval is the full HTTP
	// preflight; PreflightTimeout is the deadline for one preflight.
	ProbeInterval     time.Duration
	PreflightInterval time.Duration
	PreflightTimeout  time.Duration

	// MaxConsecutiveFailures and CoolDown are §6.4's repeated-failure threshold and cool-down.
	MaxConsecutiveFailures int
	CoolDown               time.Duration

	// BackoffBase and BackoffMax bound the exponential backoff between attempts.
	BackoffBase time.Duration
	BackoffMax  time.Duration

	// BodyCap bounds a buffered request body. An over-cap body is forwarded unread-as-content
	// and reported degraded (§5.3's rule, applied here for the same reason).
	BodyCap int64

	Pipeline Pipeline
	Agent    AgentInfo

	// Decide supplies the policy decision a prompt event requires. It is a seam because the
	// rules engine lives elsewhere; the default records `logged` under a named default rule, so
	// a decision is never silently absent.
	Decide func(tool string) *protocol.Decision

	Log   core.Logger
	Clock func() time.Time
}

func (c Config) withDefaults() Config {
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = 5 * time.Second
	}
	if c.PreflightInterval <= 0 {
		c.PreflightInterval = time.Minute
	}
	if c.PreflightTimeout <= 0 {
		c.PreflightTimeout = 3 * time.Second
	}
	if c.PreflightTimeout > PreflightTimeoutCap {
		c.PreflightTimeout = PreflightTimeoutCap
	}
	if c.MaxConsecutiveFailures <= 0 {
		c.MaxConsecutiveFailures = 5
	}
	if c.CoolDown <= 0 {
		c.CoolDown = 10 * time.Minute
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.BodyCap <= 0 {
		c.BodyCap = 4 << 20
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.Decide == nil {
		c.Decide = func(string) *protocol.Decision {
			return &protocol.Decision{RuleID: "policy.default", Action: protocol.ActionLogged, DecidedLocally: true}
		}
	}
	return c
}

// Broker is the `proxy.loopback` provider.
type Broker struct {
	cfg Config

	mu        sync.Mutex
	runners   []*portRunner
	counters  *core.CounterSet
	startedAt time.Time
	started   bool
	stopped   bool
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup

	lastOKMu sync.Mutex
	lastOK   time.Time
}

// New returns a broker. It is not started: Start performs the first preflight attempt for
// every configured port before returning, which is what makes "the broker binds last" a
// property the supervisor can rely on rather than a race.
func New(cfg Config) *Broker {
	cfg = cfg.withDefaults()
	now := cfg.Clock()
	return &Broker{
		cfg:       cfg,
		counters:  core.NewCounterSet(now),
		startedAt: now,
		stopCh:    make(chan struct{}),
	}
}

// Name implements core.Provider. The collector name is the route, so the coverage row cannot
// invent a name the reporting layer does not know.
func (b *Broker) Name() protocol.Route { return protocol.RouteProxyLoopback }

// Start implements core.Provider.
//
// It returns nil even when preflight fails and the port stays RELEASED: §6.2 rule 3 makes
// refusing to bind the *safe* behaviour, and §6.4 reports that condition as `degraded` rather
// than as an absent provider. It returns after every port has reached its first decision, so
// the supervisor's ordering (broker last) means something.
func (b *Broker) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return nil
	}
	b.started = true
	b.stopped = false
	specs := append([]policy.LoopbackPort(nil), b.cfg.Ports...)
	b.mu.Unlock()

	if len(specs) == 0 {
		// No port is configured, so there is nothing to hold and nothing to break. This is a
		// coverage gap, not a healthy provider.
		b.setLastFail(protocol.Detail("no_ports_configured"))
		return nil
	}

	for _, spec := range specs {
		r := newPortRunner(b, spec)
		b.mu.Lock()
		b.runners = append(b.runners, r)
		b.mu.Unlock()
		b.wg.Add(1)
		go func(r *portRunner) {
			defer b.wg.Done()
			r.run()
		}(r)
	}

	// Wait for each port's first decision, bounded by the preflight deadline. A port that is
	// still deciding is reported as released until it decides.
	deadline := time.NewTimer(b.cfg.PreflightTimeout + 2*time.Second)
	defer deadline.Stop()
	b.mu.Lock()
	runners := append([]*portRunner(nil), b.runners...)
	b.mu.Unlock()
	for _, r := range runners {
		select {
		case <-r.ready:
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// Release performs §3.5's shutdown step 2: the loopback port is released before anything else,
// because a broker holding a port without a serving upstream is the one failure that breaks
// the user rather than losing data (E14).
func (b *Broker) Release(ctx context.Context) error {
	b.mu.Lock()
	runners := append([]*portRunner(nil), b.runners...)
	b.mu.Unlock()
	for _, r := range runners {
		r.releaseNow()
	}
	return nil
}

// Stop implements core.Provider. It is idempotent and safe after a failed Start.
func (b *Broker) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	runners := append([]*portRunner(nil), b.runners...)
	b.mu.Unlock()

	b.stopOnce.Do(func() { close(b.stopCh) })
	for _, r := range runners {
		// stopNow closes the runner's own stop channel and releases the listener immediately:
		// the release happens before the goroutine is even asked to wind down, so the port is
		// free the moment Stop begins (§6.2 rule 1, §3.5 step 2).
		r.stopNow()
	}
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Health implements core.Provider. The rule from §4.1 is the important one here: `healthy`
// requires the port held *and* the upstream reachable, because "listening" is not "observing".
func (b *Broker) Health() core.Health {
	b.mu.Lock()
	runners := append([]*portRunner(nil), b.runners...)
	started, stopped := b.started, b.stopped
	b.mu.Unlock()

	lastOK := b.lastSuccess()
	if !started || stopped || len(runners) == 0 {
		return b.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, b.startedAt, lastOK)
	}

	var held, reachable, tampered, cooling int
	var worst protocol.Detail
	for _, r := range runners {
		switch r.snapshotState() {
		case StateHolding:
			held++
			reachable++ // HOLDING is only ever entered from a passing preflight
		case StateBinding:
			worst = firstNonEmpty(worst, protocol.DetailNone)
		}
		if r.tampered() {
			tampered++
		}
		if r.cooling() {
			cooling++
		}
		if d := r.detail(); d != protocol.DetailNone && worst == protocol.DetailNone {
			worst = d
		}
		if r.state() == StateReleased && r.lastFailDetail() != protocol.DetailNone && worst == protocol.DetailNone {
			worst = r.lastFailDetail()
		}
	}

	switch {
	case tampered > 0:
		return b.counters.Snapshot(protocol.StateTampered, protocol.DetailPortHeldByOther, b.startedAt, lastOK)
	case held > 0 && held == reachable:
		// Port held and upstream reachable: mode F is captured. Any port not held is a named
		// coverage gap in the outcome of the next operation, not a silent omission.
		if held < len(runners) {
			return b.counters.Snapshot(protocol.StateDegraded, firstNonEmpty(worst, protocol.DetailUpstreamUnreachable), b.startedAt, lastOK)
		}
		return b.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, b.startedAt, lastOK)
	case cooling > 0:
		return b.counters.Snapshot(protocol.StateDegraded, protocol.DetailCoolingDown, b.startedAt, lastOK)
	default:
		return b.counters.Snapshot(protocol.StateDegraded, firstNonEmpty(worst, protocol.DetailUpstreamUnreachable), b.startedAt, lastOK)
	}
}

// Coverage is §6.5's coverage row: ports configured N, held M, upstream reachable K. `K < M` is
// an alarm rather than a footnote, which is why it is a separate accessor rather than prose.
func (b *Broker) Coverage() (configured, held, reachable int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	configured = len(b.runners)
	for _, r := range b.runners {
		if r.state() == StateHolding {
			held++
			reachable++
		}
	}
	return configured, held, reachable
}

// Counters exposes the broker's counter set for tests and the coverage row.
func (b *Broker) Counters() *core.CounterSet { return b.counters }

// ApplyPolicy implements core.Provider: a diff, never a restart. A port whose entry changed is
// released first (the configuration-change restart path of §6.2 rule 2) and re-preflighted.
func (b *Broker) ApplyPolicy(bundle policy.Bundle) error {
	b.mu.Lock()
	started := b.started
	b.mu.Unlock()
	if !started {
		// Not started yet: the new bundle is picked up by Start.
		b.mu.Lock()
		b.cfg.Ports = append([]policy.LoopbackPort(nil), bundle.Loopback.Ports...)
		b.mu.Unlock()
		return nil
	}

	byTool := map[string]policy.LoopbackPort{}
	for _, p := range bundle.Loopback.Ports {
		byTool[p.ToolFingerprint] = p
	}

	b.mu.Lock()
	runners := append([]*portRunner(nil), b.runners...)
	b.mu.Unlock()

	seen := map[string]bool{}
	for _, r := range runners {
		spec, ok := byTool[r.currentSpec().ToolFingerprint]
		if !ok {
			r.stopNow()
			continue
		}
		seen[r.currentSpec().ToolFingerprint] = true
		if spec != r.currentSpec() {
			r.updateSpec(spec)
		}
	}
	for tool, spec := range byTool {
		if seen[tool] {
			continue
		}
		nr := newPortRunner(b, spec)
		b.mu.Lock()
		b.runners = append(b.runners, nr)
		b.mu.Unlock()
		b.wg.Add(1)
		go func(r *portRunner) {
			defer b.wg.Done()
			r.run()
		}(nr)
	}
	return nil
}

func (b *Broker) lastSuccess() time.Time {
	b.lastOKMu.Lock()
	defer b.lastOKMu.Unlock()
	return b.lastOK
}

func (b *Broker) markSuccess(t time.Time) {
	b.lastOKMu.Lock()
	b.lastOK = t
	b.lastOKMu.Unlock()
}

func (b *Broker) setLastFail(d protocol.Detail) {
	_ = d // recorded through the runners' details; kept for the no-ports case
}

func firstNonEmpty(a, b protocol.Detail) protocol.Detail {
	if a != protocol.DetailNone {
		return a
	}
	return b
}

// portRunner owns one held port. It is the single writer of that port's state: the watchdog
// and the preflight only *report* events, and this goroutine decides what happens (§6.3: "the
// watchdog never binds").
type portRunner struct {
	broker *Broker

	specMu sync.RWMutex
	spec   policy.LoopbackPort

	mu        sync.Mutex
	machine   *Machine
	ln        net.Listener
	lastFail  protocol.Detail
	coolUntil time.Time

	events chan Event
	conns  chan net.Conn
	specs  chan policy.LoopbackPort
	ready  chan struct{}
	stopCh chan struct{}
	once   sync.Once
}

// currentSpec is the policy entry this runner is acting on. The runtime goroutine writes it;
// connection handlers read it, so the access is locked rather than assumed safe.
func (r *portRunner) currentSpec() policy.LoopbackPort {
	r.specMu.RLock()
	defer r.specMu.RUnlock()
	return r.spec
}

func (r *portRunner) setSpec(s policy.LoopbackPort) {
	r.specMu.Lock()
	r.spec = s
	r.specMu.Unlock()
}

func newPortRunner(b *Broker, spec policy.LoopbackPort) *portRunner {
	return &portRunner{
		broker: b,
		spec:   spec,
		machine: NewMachine(MachineConfig{
			MaxConsecutiveFailures: b.cfg.MaxConsecutiveFailures,
		}),
		events: make(chan Event, 16),
		conns:  make(chan net.Conn, 32),
		specs:  make(chan policy.LoopbackPort, 1),
		ready:  make(chan struct{}),
		stopCh: make(chan struct{}),
	}
}

func (r *portRunner) state() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.machine.State()
}

func (r *portRunner) snapshotState() State { return r.state() }

func (r *portRunner) tampered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.machine.Tampered()
}

func (r *portRunner) cooling() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.coolUntil.IsZero() && r.broker.cfg.Clock().Before(r.coolUntil)
}

func (r *portRunner) detail() protocol.Detail {
	if r.cooling() {
		return protocol.DetailCoolingDown
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.machine.Detail()
}

func (r *portRunner) lastFailDetail() protocol.Detail {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastFail
}

// releaseNow closes the listening socket immediately. Every restart path goes through here
// first, which is §6.2 rule 2 expressed in code rather than in intention.
func (r *portRunner) releaseNow() {
	r.mu.Lock()
	ln := r.ln
	r.ln = nil
	r.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

func (r *portRunner) stopNow() {
	r.once.Do(func() { close(r.stopCh) })
	r.releaseNow()
}

func (r *portRunner) updateSpec(spec policy.LoopbackPort) {
	select {
	case r.specs <- spec:
	default:
		// A pending change is already queued; the newest one wins on the next loop.
		select {
		case <-r.specs:
		default:
		}
		select {
		case r.specs <- spec:
		default:
		}
	}
}

func (r *portRunner) upstreamAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", r.currentSpec().UpstreamPort)
}

func (r *portRunner) portAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", r.currentSpec().Port)
}

// run is the state machine's driver for one port.
func (r *portRunner) run() {
	var readyOnce sync.Once
	signalReady := func() { readyOnce.Do(func() { close(r.ready) }) }
	defer signalReady() // a runner that exits early must still release Start's wait
	defer r.releaseNow()

	// The watchdog is a separate goroutine (§6.3): it reports, it never binds.
	watchdogDone := make(chan struct{})
	go r.watchdog(watchdogDone)
	defer close(watchdogDone)

	backoffTimer := time.NewTimer(time.Hour)
	backoffTimer.Stop()

	// The first input is "backoff expired", which the machine answers with ActPreflight: a
	// fresh broker preflights before it can reach BINDING, and nothing else can bind it.
	ev := EvBackoffExpired
	firstDecision := true
	shuttingDown := false

	wait := func() {
		select {
		case e := <-r.events:
			ev = e
		case e := <-r.specs:
			r.setSpec(e)
			ev = EvPolicyChanged
		case <-r.stopCh:
			shuttingDown = true
			ev = EvShutdown
		case <-backoffTimer.C:
			ev = EvBackoffExpired
		}
	}

	for {
		if shuttingDown {
			r.apply(EvShutdown)
			r.releaseNow()
			return
		}
		action := r.apply(ev)
		if firstDecision && (action == ActServe || action == ActBackoff || action == ActRelease || action == ActNone) {
			firstDecision = false
			signalReady()
		}
		switch action {
		case ActServe:
			r.startAccepting()
			wait()
			continue
		case ActBackoff:
			backoffTimer.Reset(r.nextBackoff())
			wait()
			continue
		case ActPreflight:
			ev = r.preflightEvent()
			continue
		case ActClose:
			// A release is a restart path: the port is closed, and the next attempt happens
			// after a backoff. Without arming the timer here a released broker would wait for
			// an event that never comes and never recover (§6.4 row 3).
			backoffTimer.Reset(r.nextBackoff())
		case ActRelease:
			// Something else holds the port. The broker does not fight for it, and re-checks
			// only after a cool-down rather than in a tight loop.
			backoffTimer.Reset(r.broker.cfg.CoolDown)
		default:
			// ActNone: nothing to do but wait for input.
		}
		wait()
	}
}

// apply runs the machine once and performs the parts of the action that are synchronous
// (closing a socket, binding, running a preflight). It returns the action it executed plus any
// follow-up action, so the loop can decide whether to wait.
func (r *portRunner) apply(ev Event) Action {
	r.mu.Lock()
	action := r.machine.Apply(ev)
	tampered := r.machine.Tampered()
	r.mu.Unlock()

	switch action {
	case ActClose, ActRelease:
		r.releaseNow()
		if ev == EvPortHeldByOther || tampered {
			r.broker.counters.Add(protocol.CounterErrors)
		}
		return action
	case ActBind:
		// Release precedes bind on every path (§6.2 rule 2): no code path rebinds while a
		// previous socket may be open.
		r.releaseNow()
		// Rule 5: if something is listening on the port we intend to hold and it is not us, the
		// broker does not bind and does not fight for the holder — it may be the user's own
		// server, started on its default port because relocation failed.
		if tcpProbe(r.portAddr(), 200*time.Millisecond) == nil {
			r.mu.Lock()
			r.stateFollow(EvPortHeldByOther)
			r.lastFail = protocol.DetailPortHeldByOther
			r.mu.Unlock()
			return ActRelease
		}
		ln, err := net.Listen("tcp", r.portAddr())
		if err != nil {
			// If the port answers a probe it is held by another process; otherwise this is a
			// plain bind failure. The two are different coverage facts and are reported
			// differently (§6.4).
			if tcpProbe(r.portAddr(), 200*time.Millisecond) == nil {
				r.mu.Lock()
				r.stateFollow(EvPortHeldByOther)
				r.lastFail = protocol.DetailPortHeldByOther
				r.mu.Unlock()
				return ActRelease
			}
			r.mu.Lock()
			r.stateFollow(EvBindFail)
			r.lastFail = protocol.DetailUpstreamUnreachable
			r.mu.Unlock()
			return ActBackoff
		}
		r.mu.Lock()
		r.ln = ln
		// The bind is the positive observation that moves BINDING -> HOLDING. Without it the
		// port would be open while the machine believed it was still binding, which is exactly
		// the "listening is not observing" confusion §4.1 forbids.
		r.stateFollow(EvBindOK)
		r.mu.Unlock()
		return ActServe
	case ActReprobe:
		if err := tcpProbe(r.upstreamAddr(), r.broker.cfg.PreflightTimeout); err != nil {
			r.mu.Lock()
			a := r.machine.Apply(EvProbeFailed)
			r.mu.Unlock()
			if a == ActClose {
				r.releaseNow()
			}
			return a
		}
		r.mu.Lock()
		r.machine.Apply(EvProbeOK)
		r.mu.Unlock()
		return ActNone
	}
	return action
}

// stateFollow applies an event without re-entering the action dispatcher. Callers hold r.mu.
func (r *portRunner) stateFollow(ev Event) { r.machine.Apply(ev) }

func (r *portRunner) nextBackoff() time.Duration {
	r.mu.Lock()
	failures := r.machine.ConsecutiveFailures()
	cooling := failures >= r.broker.cfg.MaxConsecutiveFailures
	r.mu.Unlock()

	if cooling {
		r.mu.Lock()
		r.coolUntil = r.broker.cfg.Clock().Add(r.broker.cfg.CoolDown)
		r.lastFail = protocol.DetailCoolingDown
		r.mu.Unlock()
		return r.broker.cfg.CoolDown
	}
	d := r.broker.cfg.BackoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= r.broker.cfg.BackoffMax {
			return r.broker.cfg.BackoffMax
		}
	}
	if d > r.broker.cfg.BackoffMax {
		d = r.broker.cfg.BackoffMax
	}
	return d
}

// preflightEvent runs one preflight and converts it to an event. Any well-formed HTTP
// response, including 4xx, is success: a 4xx proves a server is listening and speaking HTTP,
// and requiring 2xx would fail on a server wanting authentication (§6.3).
func (r *portRunner) preflightEvent() Event {
	ctx, cancel := context.WithTimeout(context.Background(), r.broker.cfg.PreflightTimeout)
	defer cancel()
	if err := Preflight(ctx, r.upstreamAddr(), r.currentSpec().PreflightPath, r.broker.cfg.PreflightTimeout); err != nil {
		r.mu.Lock()
		r.lastFail = protocol.DetailUpstreamUnreachable
		r.mu.Unlock()
		return EvPreflightFail
	}
	r.mu.Lock()
	r.lastFail = protocol.DetailNone
	r.mu.Unlock()
	r.broker.markSuccess(r.broker.cfg.Clock())
	return EvPreflightOK
}

func (r *portRunner) startAccepting() {
	r.mu.Lock()
	ln := r.ln
	r.mu.Unlock()
	if ln == nil {
		return
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-r.stopCh:
					return
				default:
				}
				select {
				case r.events <- EvServeError:
				case <-r.stopCh:
				}
				return
			}
			select {
			case r.conns <- conn:
			case <-r.stopCh:
				_ = conn.Close()
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case conn := <-r.conns:
				go r.handleConn(context.Background(), conn)
			case <-r.stopCh:
				return
			}
		}
	}()
}

// watchdog is the short-interval liveness probe and the long-interval full preflight. It never
// binds, closes or decides: it sends events.
func (r *portRunner) watchdog(done chan struct{}) {
	probe := time.NewTicker(r.broker.cfg.ProbeInterval)
	defer probe.Stop()
	full := time.NewTicker(r.broker.cfg.PreflightInterval)
	defer full.Stop()
	for {
		select {
		case <-done:
			return
		case <-probe.C:
			if r.state() != StateHolding {
				continue
			}
			if err := tcpProbe(r.upstreamAddr(), r.broker.cfg.PreflightTimeout); err != nil {
				r.emit(EvProbeFailed)
			} else {
				r.emit(EvProbeOK)
			}
		case <-full.C:
			if r.state() != StateHolding {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), r.broker.cfg.PreflightTimeout)
			err := Preflight(ctx, r.upstreamAddr(), r.currentSpec().PreflightPath, r.broker.cfg.PreflightTimeout)
			cancel()
			if err != nil {
				r.emit(EvPreflightFail)
			}
		}
	}
}

func (r *portRunner) emit(ev Event) {
	select {
	case r.events <- ev:
	case <-r.stopCh:
	default:
		// The event queue is full, which means the runner is busy deciding; dropping a
		// liveness event is safe because the next tick re-probes.
	}
}

// Preflight is §6.3's single loopback HTTP request: minimal, read-only, short deadline, then a
// close. The path is per-tool bundle configuration (A8) because invoking a generation endpoint
// to test health would consume the user's resources and could itself look like usage.
func Preflight(ctx context.Context, upstreamAddr, path string, timeout time.Duration) error {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", upstreamAddr)
	if err != nil {
		return fmt.Errorf("loopback: preflight dial %s: %w", upstreamAddr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	req := &http.Request{
		Method:     http.MethodGet,
		URL:        &url.URL{Scheme: "http", Host: upstreamAddr, Path: path},
		Host:       upstreamAddr,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"User-Agent": []string{"shadow-ai-capture-preflight/1"}},
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("loopback: preflight write: %w", err)
	}
	resp, err := http.ReadResponse(newBufReader(conn), req)
	if err != nil {
		return fmt.Errorf("loopback: preflight response: %w", err)
	}
	defer resp.Body.Close()
	// Any well-formed response, including 4xx, means a server is listening and speaking HTTP.
	return nil
}

// tcpProbe is the cheap liveness check: connect, close, no request, no side effects.
func tcpProbe(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// ErrNotConfigured is returned when a tool has no port entry: the port is not claimed, and the
// coverage row shows it as a named gap.
var ErrNotConfigured = errors.New("loopback: tool has no port entry in the bundle")
