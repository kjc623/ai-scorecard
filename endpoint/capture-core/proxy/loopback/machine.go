// Package loopback is `proxy.loopback` (docs/01-collectors.md §6): the broker that holds the
// port a local inference server would otherwise bind, forwards to the relocated server, and
// observes the plaintext request bodies that pass through it.
//
// §6.2 is why this provider is shaped differently from every other one: its failure mode is
// inverted. Releasing the port when it cannot serve is the safe behaviour, so "refusing to
// start" is correct here and "starting anyway" is the defect. The state machine in machine.go
// is the single writer of that decision, the watchdog only reports, and binding happens only
// after a passing preflight.
package loopback

import (
	"errors"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// State is §6.2's four states.
type State string

// The four states of the §6.2 diagram.
const (
	// StateReleased is the default: the port is not bound by us, the upstream is untouched.
	StateReleased State = "RELEASED"
	// StateBinding is bind + probe, entered only from a passing preflight.
	StateBinding State = "BINDING"
	// StateHolding is port bound and forwarding.
	StateHolding State = "HOLDING"
	// StateOrphan is a socket closed by the OS on process death (§6.2 rule 4).
	StateOrphan State = "ORPHAN"
)

// Event is an input to the state machine. The watchdog, the preflight and the shutdown path
// produce events; only the machine decides.
type Event string

// Events.
const (
	EvPreflightOK      Event = "preflight_ok"
	EvPreflightFail    Event = "preflight_fail"
	EvBindOK           Event = "bind_ok"
	EvBindFail         Event = "bind_fail"
	EvPortHeldByOther  Event = "port_held_by_other"
	EvProbeFailed      Event = "watchdog_probe_failed"       // one missed probe
	EvProbeFailedTwice Event = "watchdog_probe_failed_twice" // two consecutive
	EvProbeOK          Event = "watchdog_probe_ok"           // liveness restored, streak reset
	EvServeError       Event = "serve_error"
	EvShutdown         Event = "shutdown"
	EvCrash            Event = "crash"
	EvBackoffExpired   Event = "backoff_expired"
	EvPolicyChanged    Event = "policy_changed"
)

// Action is what the machine tells the runtime to do. The machine never binds, closes or
// sleeps itself: it returns an action, and one runtime goroutine per port performs it.
type Action string

// Actions.
const (
	ActNone      Action = ""
	ActPreflight Action = "preflight"
	ActBind      Action = "bind"
	ActClose     Action = "close_listener"
	ActBackoff   Action = "backoff"
	ActReprobe   Action = "re_probe_immediately"
	ActRelease   Action = "stay_released"
)

// MachineConfig carries the two thresholds §6.3 and §6.4 name.
type MachineConfig struct {
	// MaxConsecutiveFailures is the repeated-failure threshold past which the broker stops
	// trying for a long cool-down (§6.4's last row) so a broken configuration does not become
	// an endless bind/release loop against the user's machine.
	MaxConsecutiveFailures int
}

// Machine is §6.2's state machine for one held port. It is pure: no sockets, no clock, no
// goroutines, so every transition in the document's diagram is directly testable.
type Machine struct {
	cfg       MachineConfig
	state     State
	missed    int
	failures  int
	tampered  bool
	conflicts int
	lastFail  Event
}

// NewMachine returns a machine whose default state is RELEASED (§6.2 rule 1).
func NewMachine(cfg MachineConfig) *Machine {
	if cfg.MaxConsecutiveFailures <= 0 {
		cfg.MaxConsecutiveFailures = 5
	}
	return &Machine{cfg: cfg, state: StateReleased}
}

// State returns the current state.
func (m *Machine) State() State { return m.state }

// ConsecutiveFailures returns the current failure streak.
func (m *Machine) ConsecutiveFailures() int { return m.failures }

// Tampered reports whether the port is held by something that is not the expected upstream
// (§6.2 rule 5): the broker does not bind and does not fight for it.
//
// It is **present tense**, and that is a decision rather than a detail: `tampered` is the only state
// that raises a security finding (§4.2, C24), so a conflict that has ended must not keep raising one.
// A sticky flag that outlives its cause made a recovered, serving broker report `tampered` forever —
// measured by the R1 harness, where the port was re-bound in 1.01 s and served a request while the
// health row still said `port_held_by_other`. The history is kept in ConflictCount, because "this
// happened once" is worth knowing without mislabelling a working port.
func (m *Machine) Tampered() bool { return m.tampered }

// ConflictCount is how many times a port conflict has been observed since the process started. It
// is deliberately NOT a protocol.Counter: the closed set of seven is closed (A15), so this travels
// on the provider's coverage row rather than inventing a name the reporting layer cannot group.
func (m *Machine) ConflictCount() int { return m.conflicts }

// Detail is the health cause the machine's current state implies, from §6.3/§6.4's closed
// vocabulary.
func (m *Machine) Detail() protocol.Detail {
	switch {
	case m.tampered:
		return protocol.DetailPortHeldByOther
	case m.state == StateHolding:
		return protocol.DetailNone
	case m.failures >= m.cfg.MaxConsecutiveFailures:
		return protocol.DetailCoolingDown
	case m.lastFail == EvProbeFailed || m.lastFail == EvProbeFailedTwice || m.lastFail == EvPreflightFail:
		return protocol.DetailUpstreamUnreachable
	default:
		return protocol.DetailNone
	}
}

// Apply advances the machine and returns the action the runtime must perform.
//
// The transitions, in the order the document states them:
//
//	RELEASED  + preflight ok                 -> BINDING  (bind; never before preflight)
//	RELEASED  + preflight fail               -> RELEASED (back off)
//	BINDING   + bind ok                      -> HOLDING  (serve)
//	BINDING   + bind fail                    -> RELEASED (back off)
//	BINDING   + port held by another process -> RELEASED (tampered; never fight for it)
//	HOLDING   + one missed probe             -> HOLDING  (re-probe immediately)
//	HOLDING   + two consecutive missed       -> RELEASED (release, back off)
//	HOLDING   + serve error                  -> RELEASED (release, back off)
//	HOLDING   + shutdown                     -> RELEASED (release)
//	HOLDING   + process crash                -> ORPHAN   (OS closed the socket)
//	ORPHAN    + preflight ok                 -> BINDING  (re-bind only after preflight)
//	ORPHAN    + preflight fail               -> RELEASED (back off)
//
// Release precedes restart on every path: an action that leaves HOLDING always carries
// ActClose, so no code path can rebind while a previous socket may be open (§6.2 rule 2).
func (m *Machine) Apply(ev Event) Action {
	switch ev {
	case EvShutdown:
		prev := m.state
		m.state = StateReleased
		m.missed = 0
		if prev == StateHolding || prev == StateBinding {
			return ActClose
		}
		return ActRelease

	case EvCrash:
		if m.state == StateHolding {
			m.state = StateOrphan
			m.missed = 0
		}
		return ActNone

	case EvPolicyChanged:
		// A policy change is a restart path, so it releases first like every other one.
		prev := m.state
		m.state = StateReleased
		m.missed = 0
		m.tampered = false
		if prev == StateHolding || prev == StateBinding {
			return ActClose
		}
		return ActPreflight
	}

	switch m.state {
	case StateReleased, StateOrphan:
		switch ev {
		case EvPreflightOK:
			m.state = StateBinding
			return ActBind
		case EvPreflightFail:
			m.failures++
			m.lastFail = ev
			// FROM ORPHAN this is also the return to RELEASED: process death is a release, and
			// the supervisor does not recreate the socket before re-running preflight (§6.2
			// rule 4).
			m.state = StateReleased
			return ActBackoff
		case EvBackoffExpired:
			return ActPreflight
		case EvBindFail, EvServeError, EvProbeFailedTwice, EvBindOK:
			// Stray events: a released broker cannot fail to serve.
			return ActNone
		}
	case StateBinding:
		switch ev {
		case EvBindOK:
			m.state = StateHolding
			m.missed = 0
			// The bind is the positive observation that the conflict, if there was one, is over:
			// the port is ours and serving. Clearing it here rather than leaving it sticky is what
			// keeps `tampered` present-tense, so a working port does not raise a security finding
			// forever. ConflictCount keeps the history.
			m.tampered = false
			return "serve"
		case EvBindFail:
			m.failures++
			m.lastFail = ev
			m.state = StateReleased
			return ActBackoff
		case EvPortHeldByOther:
			m.tampered = true
			m.conflicts++
			m.state = StateReleased
			return ActRelease
		case EvPreflightFail:
			m.state = StateReleased
			m.failures++
			m.lastFail = ev
			return ActBackoff
		}
	case StateHolding:
		switch ev {
		case EvProbeFailed:
			// One missed probe: re-probe immediately, do not release. A momentarily busy
			// server must not trigger a release (§6.3).
			m.missed++
			if m.missed >= 2 {
				m.failures++
				m.lastFail = EvProbeFailedTwice
				m.state = StateReleased
				m.missed = 0
				return ActClose
			}
			return ActReprobe
		case EvProbeFailedTwice:
			m.failures++
			m.lastFail = EvProbeFailedTwice
			m.state = StateReleased
			m.missed = 0
			return ActClose
		case EvServeError:
			m.failures++
			m.lastFail = EvServeError
			m.state = StateReleased
			return ActClose
		case EvPreflightFail:
			// The long-interval preflight is the stronger signal: release on it directly.
			m.failures++
			m.lastFail = ev
			m.state = StateReleased
			return ActClose
		case EvBindFail, EvPortHeldByOther:
			// Cannot happen while holding; treat as tampering rather than ignoring it.
			m.tampered = true
			m.state = StateReleased
			return ActClose
		case EvPreflightOK:
			m.failures = 0
			m.missed = 0
			return ActNone
		case EvProbeOK:
			// A successful probe clears the missed-probe streak: the ladder only releases on
			// *consecutive* misses, so a momentarily busy server never causes a release.
			m.missed = 0
			return ActNone
		}
	}
	return ActNone
}

// ActServe is the action that enters the serving loop. It is a constant rather than a literal
// so the machine's vocabulary is enumerable.
const ActServe Action = "serve"

// Healthy reports whether this port is in the one state where mode F is captured: port held
// *and* the upstream reachable (§6.3).
func (m *Machine) Healthy() bool { return m.state == StateHolding }

// CloseBeforeRestart asserts the ordering rule as a property of a transition: any transition
// that leaves HOLDING must carry ActClose. It exists so a test can enumerate transitions and
// fail if one violates §6.2 rule 2.
func CloseBeforeRestart(from State, ev Event, action Action) bool {
	if from != StateHolding {
		return true
	}
	switch ev {
	case EvShutdown, EvServeError, EvProbeFailedTwice, EvPreflightFail, EvBindFail, EvPortHeldByOther:
		return action == ActClose
	default:
		return true
	}
}

// ErrPortUnavailable is returned by the runtime when the port is held by something that is not
// the expected upstream. It is not a Start failure: §6.4 reports it as `tampered` with
// `detail=port_held_by_other` and the broker stays released.
var ErrPortUnavailable = errors.New("loopback: port is held by another process")

// PreflightTimeoutCap bounds a preflight even when policy asks for a longer one: §6.3 wants a
// short deadline, because a slow preflight delays recovery and a generous one lets a dead
// server look alive.
const PreflightTimeoutCap = 10 * time.Second
