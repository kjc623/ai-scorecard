package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/state"
)

// edgeTrustArg starts the edge trust check: `capture-core --check-edge-trust <endpoint> [<ca-file>]`
// connects to the device edge and exits 0 when its certificate verifies, 1 when it does not. The
// service runs it to learn whether the edge verifies in a new process.
const edgeTrustArg = "--check-edge-trust"

const (
	// edgeTrustGrace is how long every certificate check of the edge may fail, at least
	// edgeTrustFailures times, before the service asks a new process whether the edge verifies
	// there.
	edgeTrustGrace    = 2 * time.Minute
	edgeTrustFailures = 3
	// edgeTrustRecheck is how long the service waits before asking again when the edge did not
	// verify in the new process either.
	edgeTrustRecheck = 15 * time.Minute
	// edgeTrustTick is how often the service looks at the edge's verification state.
	edgeTrustTick = 30 * time.Second
	// edgeTrustTimeout bounds one check, in the new process and around it.
	edgeTrustTimeout = 30 * time.Second
	// edgeTrustRestartGap is the least time between two restarts for the edge's certificate, so a
	// fault that comes back with every start does not keep restarting the agent.
	edgeTrustRestartGap = time.Hour
)

// errRestart is how the service stops when it asks to be started again: the Service Control
// Manager, systemd and launchd restart a service that exits with an error.
var errRestart = errors.New("stopped to be restarted")

// runEdgeTrustCheck runs the edge trust check and returns its exit code. It writes why the edge did
// not verify to stderr.
func runEdgeTrustCheck(args []string, stderr io.Writer) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintf(stderr, "usage: capture-core %s <endpoint> [<ca-file>]\n", edgeTrustArg)
		return 2
	}
	caFile := ""
	if len(args) == 2 {
		caFile = args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), edgeTrustTimeout)
	defer cancel()
	if err := drain.VerifyEdge(ctx, args[0], caFile); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// checkEdgeInNewProcess runs the edge trust check in a new process of this executable.
func checkEdgeInNewProcess(ctx context.Context, endpoint, caFile string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, edgeTrustTimeout+5*time.Second)
	defer cancel()
	args := []string{edgeTrustArg, endpoint}
	if caFile != "" {
		args = append(args, caFile)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// edgeTrustWatch decides when to ask a new process whether the edge verifies: once every check in
// this process has failed for edgeTrustGrace and at least edgeTrustFailures times, and then every
// edgeTrustRecheck while they still fail.
type edgeTrustWatch struct {
	next time.Time
}

// due reports whether to ask now, given when the edge's certificate first failed verification
// after the last check that passed and how many checks have failed since.
func (w *edgeTrustWatch) due(now, since time.Time, failures int) bool {
	if failures == 0 {
		w.next = time.Time{}
		return false
	}
	return failures >= edgeTrustFailures && now.Sub(since) >= edgeTrustGrace && !now.Before(w.next)
}

// asked records that a new process was asked at now.
func (w *edgeTrustWatch) asked(now time.Time) { w.next = now.Add(edgeTrustRecheck) }

// watchEdgeTrust restarts the agent when no certificate check of the edge has passed in this
// process for edgeTrustGrace and the edge verifies in a new process: the fault is then state this
// process holds (on Windows, its certificate chain engine), which a restart clears. When the edge
// does not verify in a new process either, the device does not trust it, and a restart would not
// help.
func (s *service) watchEdgeTrust(check func(ctx context.Context, endpoint, caFile string) error, stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-stop
		cancel()
	}()
	t := time.NewTicker(edgeTrustTick)
	defer t.Stop()
	var w edgeTrustWatch
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if s.edgeTrustStep(ctx, &w, time.Now(), check) {
			s.requestRestart()
			return
		}
	}
}

// edgeTrustStep looks at the edge's verification state at now and reports whether to restart.
func (s *service) edgeTrustStep(ctx context.Context, w *edgeTrustWatch, now time.Time, check func(ctx context.Context, endpoint, caFile string) error) bool {
	since, failures := s.drainer.UnverifiedSince()
	if !w.due(now, since, failures) {
		return false
	}
	w.asked(now)
	if err := check(ctx, s.cfg.DeviceEndpoint, s.cfg.CAFile); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("edge: the edge's certificate does not verify on this device", "failing_since", since, "failures", failures, "error", err)
		}
		return false
	}
	if last, ok := s.lastEdgeRestart(); ok && now.Sub(last) < edgeTrustRestartGap {
		s.log.Warn("edge: no certificate check of the edge has passed in this process since it failed, though one passes in a new process; the agent restarted for this recently and does not restart again yet", "failing_since", since, "failures", failures, "last_restart", last)
		return false
	}
	if err := s.recordEdgeRestart(now); err != nil {
		s.log.Warn("edge: the restart is not recorded; it does not hold back the next one", "error", err)
	}
	s.log.Warn("edge: no certificate check of the edge has passed in this process since it failed, and one passes in a new process; restarting the agent", "failing_since", since, "failures", failures)
	return true
}

// lastEdgeRestart is when the agent last restarted itself for the edge's certificate.
func (s *service) lastEdgeRestart() (time.Time, bool) {
	b, err := os.ReadFile(s.dir.Path(state.EdgeRestartFile))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t, err == nil
}

// recordEdgeRestart records a restart for the edge's certificate at now.
func (s *service) recordEdgeRestart(now time.Time) error {
	return state.WriteFile(s.dir.Path(state.EdgeRestartFile), []byte(now.UTC().Format(time.RFC3339Nano)))
}

// requestRestart asks runServiceContext to stop the agent with errRestart.
func (s *service) requestRestart() {
	s.restartOnce.Do(func() { close(s.restart) })
}
