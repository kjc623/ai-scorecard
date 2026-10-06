//go:build windows

package main

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// serviceName is the Windows service the installer registers. The Service Control Manager
// ignores the name for a service that owns its process, but it is the name the log and the
// documentation use.
const serviceName = "ShadowAICapture"

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
				cancel()
			}
		case err := <-done:
			if err != nil {
				w.log.Error("the agent stopped with an error", "error", err)
				return true, 1
			}
			return false, 0
		}
	}
}
