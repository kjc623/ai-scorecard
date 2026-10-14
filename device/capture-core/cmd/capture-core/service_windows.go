//go:build windows

package main

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// serviceName is the Windows service the installer registers. The Service Control Manager
// ignores the name when it starts a service that owns its process; the service opens its own
// configuration by it.
const serviceName = "ShadowAICapture"

// recoveryActions restart the agent after a failure, as systemd and launchd do: 5 seconds after
// the first and second failure in a day, a minute after each later one.
var recoveryActions = []mgr.RecoveryAction{
	{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	{Type: mgr.ServiceRestart, Delay: time.Minute},
}

// recoveryResetPeriod is how long the agent runs without failing before the next failure counts as
// the first again.
const recoveryResetPeriod = 24 * time.Hour

// runPlatformService runs the agent under the Service Control Manager when the SCM started this
// process, and reports false otherwise. A service has no console, so it logs to capture-core.log
// in the state directory.
func runPlatformService(cfg Config) (bool, error) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, err
	}
	dir, err := state.Open(cfg.StateDir)
	if err != nil {
		return true, err
	}
	logFile := newRotatingLog(dir.Path(state.LogFile))
	defer logFile.Close()
	log := newLogger(cfg.LogLevel, logFile)
	log.Info("service starting", "name", serviceName, "version", version)
	if err := setRecovery(); err != nil {
		log.Warn("service: recovery actions not set; the agent is not restarted after it fails", "error", err)
	}
	if err := svc.Run(serviceName, &windowsService{cfg: cfg, log: log}); err != nil {
		log.Error("service failed", "error", err)
		return true, err
	}
	return true, nil
}

// windowsService implements svc.Handler: RUNNING is reported only once the agent is up, and a stop
// or shutdown request runs the bounded shutdown while STOP_PENDING carries its budget.
type windowsService struct {
	cfg Config
	log *slog.Logger
}

func (w *windowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending, WaitHint: uint32((startupBound + 10*time.Second).Milliseconds())}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running := make(chan struct{})
	stopping := false
	done := make(chan error, 1)
	go func() {
		done <- runServiceContext(ctx, w.cfg, w.log, func() { close(running) })
	}()
	for {
		select {
		case <-running:
			running = nil
			status <- svc.Status{State: svc.Running, Accepts: accepts}
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(serviceStopBudget.Milliseconds())}
				stopping = true
				cancel()
			}
		case err := <-done:
			// An error exit makes the Service Control Manager restart the agent, so a stop it was
			// asked for exits cleanly whatever the shutdown reported.
			if err != nil {
				w.log.Error("the agent stopped with an error", "error", err)
				if !stopping {
					return true, 1
				}
			}
			return false, 0
		}
	}
}

// setRecovery has the Service Control Manager run recoveryActions when the agent crashes or stops
// with an error.
func setRecovery() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.SetRecoveryActions(recoveryActions, uint32(recoveryResetPeriod.Seconds())); err != nil {
		return err
	}
	return s.SetRecoveryActionsOnNonCrashFailures(true)
}
