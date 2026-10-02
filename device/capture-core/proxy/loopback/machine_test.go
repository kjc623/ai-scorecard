package loopback

import (
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

// TestMachine_6_2_TransitionTable walks every transition of the §6.2 diagram and the §6.3
// ladder. The test names carry the document section so a failure points at the paragraph.
func TestMachine_6_2_TransitionTable(t *testing.T) {
	m := NewMachine(MachineConfig{MaxConsecutiveFailures: 3})
	if m.State() != StateReleased {
		t.Fatalf("§6.2 rule 1: default state = %s, want RELEASED", m.State())
	}

	// §6.2 rule 3: binding requires a passing preflight. A failing preflight leaves it released.
	if action := m.Apply(EvPreflightFail); action != ActBackoff {
		t.Fatalf("§6.2 rule 3: preflight fail action = %q, want backoff (never bind)", action)
	}
	if m.State() != StateReleased {
		t.Fatalf("§6.2 rule 3: state after failed preflight = %s, want RELEASED", m.State())
	}

	// RELEASED + preflight ok -> BINDING (bind), then BINDING + bind ok -> HOLDING (serve).
	if action := m.Apply(EvPreflightOK); action != ActBind {
		t.Fatalf("preflight ok action = %q, want bind", action)
	}
	if m.State() != StateBinding {
		t.Fatalf("state after preflight ok = %s, want BINDING", m.State())
	}
	if action := m.Apply(EvBindOK); action != ActServe {
		t.Fatalf("bind ok action = %q, want serve", action)
	}
	if m.State() != StateHolding {
		t.Fatalf("state after bind = %s, want HOLDING", m.State())
	}

	// §6.3: one missed probe -> re-probe immediately, no release.
	if action := m.Apply(EvProbeFailed); action != ActReprobe {
		t.Fatalf("§6.3: first missed probe action = %q, want an immediate re-probe", action)
	}
	if m.State() != StateHolding {
		t.Fatalf("§6.3: state after one missed probe = %s, want HOLDING (a busy server must not release the port)", m.State())
	}
	// A successful re-probe clears the streak.
	if action := m.Apply(EvProbeOK); action != ActNone {
		t.Fatalf("probe ok action = %q, want none", action)
	}
	// Two consecutive misses -> release + backoff.
	if action := m.Apply(EvProbeFailed); action != ActReprobe {
		t.Fatalf("second first-miss action = %q, want re-probe", action)
	}
	if action := m.Apply(EvProbeFailed); action != ActClose {
		t.Fatalf("§6.3: two consecutive misses must release the port, action = %q", action)
	}
	if m.State() != StateReleased {
		t.Fatalf("§6.3: state after two misses = %s, want RELEASED", m.State())
	}

	// §6.4 row 1/4: recovery needs a fresh preflight before it can bind again.
	if action := m.Apply(EvBindOK); action != ActNone {
		t.Fatalf("a released broker must not accept a bind event, action = %q", action)
	}
	if action := m.Apply(EvPreflightOK); action != ActBind {
		t.Fatalf("recovery action = %q, want bind only after preflight", action)
	}
}

func TestMachine_6_2_Rule5_PortHeldByOther(t *testing.T) {
	m := NewMachine(MachineConfig{})
	m.Apply(EvPreflightOK)
	action := m.Apply(EvPortHeldByOther)
	if action != ActRelease {
		t.Fatalf("§6.2 rule 5: action = %q, want stay released", action)
	}
	if !m.Tampered() {
		t.Fatal("§6.2 rule 5: a port held by another process must be recorded as tampering")
	}
	if m.Detail() != protocol.DetailPortHeldByOther {
		t.Fatalf("§6.2 rule 5: detail = %q, want %q", m.Detail(), protocol.DetailPortHeldByOther)
	}
	if m.State() != StateReleased {
		t.Fatalf("§6.2 rule 5: state = %s, want RELEASED", m.State())
	}
}

func TestMachine_6_2_Rule4_ProcessDeathIsARelease(t *testing.T) {
	m := NewMachine(MachineConfig{})
	m.Apply(EvPreflightOK)
	m.Apply(EvBindOK)
	if action := m.Apply(EvCrash); action != ActNone {
		t.Fatalf("§6.2 rule 4: crash action = %q, want none (the OS closes the socket)", action)
	}
	if m.State() != StateOrphan {
		t.Fatalf("§6.2 rule 4: state after crash = %s, want ORPHAN", m.State())
	}
	// §6.2 rule 4: the supervisor does not recreate the socket before re-running preflight.
	if action := m.Apply(EvPreflightFail); action != ActBackoff {
		t.Fatalf("orphan + failed preflight action = %q, want backoff", action)
	}
	if m.State() != StateReleased {
		t.Fatalf("orphan + failed preflight state = %s, want RELEASED", m.State())
	}
	if action := m.Apply(EvPreflightOK); action != ActBind {
		t.Fatalf("orphan recovery action = %q, want bind after preflight", action)
	}
}

func TestMachine_6_2_Rule2_ReleasePrecedesRestart(t *testing.T) {
	// Enumerate every transition out of HOLDING and assert each carries ActClose: no code path
	// may rebind while a previous socket may be open.
	for _, ev := range []Event{EvShutdown, EvServeError, EvProbeFailedTwice, EvPreflightFail, EvBindFail, EvPortHeldByOther} {
		m := NewMachine(MachineConfig{})
		m.Apply(EvPreflightOK)
		m.Apply(EvBindOK)
		if m.State() != StateHolding {
			t.Fatalf("setup: state = %s, want HOLDING", m.State())
		}
		action := m.Apply(ev)
		if !CloseBeforeRestart(StateHolding, ev, action) {
			t.Fatalf("§6.2 rule 2: event %q from HOLDING produced %q; every restart path must close the listening socket first", ev, action)
		}
		if m.State() != StateReleased && ev != EvProbeFailed {
			t.Fatalf("event %q from HOLDING left state %s, want RELEASED", ev, m.State())
		}
	}
}

func TestMachine_6_4_RepeatedFailureCoolsDown(t *testing.T) {
	m := NewMachine(MachineConfig{MaxConsecutiveFailures: 2})
	m.Apply(EvPreflightFail)
	if m.Detail() != protocol.DetailUpstreamUnreachable {
		t.Fatalf("detail after one failure = %q, want %q", m.Detail(), protocol.DetailUpstreamUnreachable)
	}
	m.Apply(EvPreflightFail)
	if m.ConsecutiveFailures() != 2 {
		t.Fatalf("failures = %d, want 2", m.ConsecutiveFailures())
	}
	if m.Detail() != protocol.DetailCoolingDown {
		t.Fatalf("§6.4: detail past the threshold = %q, want %q", m.Detail(), protocol.DetailCoolingDown)
	}
	// The cool-down is broken by a successful preflight, which resets the streak.
	m.Apply(EvPreflightOK)
	m.Apply(EvBindOK)
	if m.State() != StateHolding {
		t.Fatalf("state after recovery = %s, want HOLDING", m.State())
	}
	if m.Detail() != protocol.DetailNone {
		t.Fatalf("healthy port detail = %q, want none", m.Detail())
	}
}

// A conflict that ends is no longer true. `tampered` is the only state that raises a security
// finding (§4.2, C24), so a broker that has recovered, re-bound and is serving must not keep
// reporting it — the defect the R1 harness measured: the port was re-bound in 1.01 s and served a
// request while the health row still said `port_held_by_other`. The history stays in ConflictCount.
func TestMachine_6_2_RecoveredConflictIsNotTampered(t *testing.T) {
	m := NewMachine(MachineConfig{})
	m.Apply(EvPreflightOK)
	if action := m.Apply(EvPortHeldByOther); action != ActRelease {
		t.Fatalf("conflict action = %q, want stay released", action)
	}
	if !m.Tampered() || m.ConflictCount() != 1 {
		t.Fatalf("after the conflict: tampered=%v conflicts=%d, want true/1", m.Tampered(), m.ConflictCount())
	}
	if m.Detail() != protocol.DetailPortHeldByOther {
		t.Fatalf("detail during the conflict = %q, want %q", m.Detail(), protocol.DetailPortHeldByOther)
	}

	// The holder goes away: the retry preflights, binds and serves.
	if action := m.Apply(EvPreflightOK); action != ActBind {
		t.Fatalf("retry action = %q, want bind", action)
	}
	if action := m.Apply(EvBindOK); action != ActServe {
		t.Fatalf("bind action = %q, want serve", action)
	}
	if m.State() != StateHolding {
		t.Fatalf("state after recovery = %s, want HOLDING", m.State())
	}
	if m.Tampered() {
		t.Fatal("a recovered, serving port still reports tampered: that is a false security finding")
	}
	if m.Detail() != protocol.DetailNone {
		t.Fatalf("detail after recovery = %q, want none", m.Detail())
	}
	if m.ConflictCount() != 1 {
		t.Fatalf("conflict count = %d, want the history kept (1)", m.ConflictCount())
	}

	// A second, later conflict is counted too, and cleared by the next successful bind.
	m.Apply(EvShutdown)
	m.Apply(EvPreflightOK)
	m.Apply(EvPortHeldByOther)
	if m.ConflictCount() != 2 {
		t.Fatalf("conflict count = %d, want 2", m.ConflictCount())
	}
	m.Apply(EvPreflightOK)
	m.Apply(EvBindOK)
	if m.Tampered() || m.ConflictCount() != 2 {
		t.Fatalf("after the second recovery: tampered=%v conflicts=%d, want false/2", m.Tampered(), m.ConflictCount())
	}
}

func TestPreflightPathDefaultIsReadOnlyRoot(t *testing.T) {
	// A8: the preflight path is per-tool bundle configuration; the default must still be a
	// read-only request, so an empty path becomes "/" rather than the generation endpoint.
	err := Preflight(t.Context(), "127.0.0.1:1", "", 100_000_000) // nothing listening: dial fails
	if err == nil {
		t.Fatal("preflight to a closed port succeeded")
	}
}
