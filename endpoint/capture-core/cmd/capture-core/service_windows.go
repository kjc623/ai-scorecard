//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"unsafe"
)

// The Windows service host. capture-core is otherwise a console application, and a console program
// cannot be registered directly as a service: the SCM launches it and waits for the process to call
// StartServiceCtrlDispatcher, which a console app never does, so the start fails. --service makes
// this process the service itself, implementing the SCM contract directly rather than requiring an
// external wrapper (NSSM/WinSW).
//
// It uses the standard library's syscall package, not golang.org/x/sys/windows/svc, because this
// module builds offline against an empty module cache with GOPROXY=off; adding x/sys would break
// that. The layout and the dispatch loop mirror x/sys/windows/svc (BSD-3-Clause, The Go Authors),
// reduced to a single service: SERVICE_TABLE_ENTRY -> StartServiceCtrlDispatcherW -> serviceMain ->
// RegisterServiceCtrlHandlerExW -> SetServiceStatus.
const (
	serviceWin32OwnProcess = 0x00000010

	serviceAcceptStop     = 0x00000001
	serviceAcceptShutdown = 0x00000004

	serviceStateStopped      = 0x00000001
	serviceStateStartPending = 0x00000002
	serviceStateStopPending  = 0x00000003
	serviceStateRunning      = 0x00000004

	serviceControlStop     = 0x00000001
	serviceControlShutdown = 0x00000005

	// errorFailedServiceControllerConnect is what StartServiceCtrlDispatcher returns when the
	// process was not started by the Service Control Manager (a --service run at a prompt).
	errorFailedServiceControllerConnect = 1063
	// errorExceptionInService is what the SCM sees when the handler itself fails.
	errorExceptionInService = 1064

	noError = 0
)

// serviceStatus mirrors the Win32 SERVICE_STATUS structure field for field.
type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

// serviceTableEntry mirrors SERVICE_TABLE_ENTRY.
type serviceTableEntry struct {
	ServiceName *uint16
	ServiceProc uintptr
}

var (
	advapi32                          = syscall.NewLazyDLL("advapi32.dll")
	procStartServiceCtrlDispatcherW   = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerExW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus              = advapi32.NewProc("SetServiceStatus")
)

// serviceHost is this process's one hosted service. The SCM dispatch table holds a single entry, so
// one service per process is the only thing that can exist; keeping it in a package variable is
// honest about that rather than pretending otherwise.
type serviceHost struct {
	cfg     Config
	log     *slog.Logger
	namePtr *uint16
	handle  syscall.Handle

	mu       sync.Mutex
	cancel   context.CancelFunc
	stopOnce sync.Once
}

var host serviceHost

// runAsService hosts the agent under the Service Control Manager and blocks until the SCM stops it.
// A process started with --service outside the SCM is refused with a clear message rather than
// hanging.
func runAsService(cfg Config, log *slog.Logger) error {
	namePtr, err := syscall.UTF16PtrFromString(cfg.ServiceName)
	if err != nil {
		return fmt.Errorf("service name %q is not encodable: %w", cfg.ServiceName, err)
	}
	host = serviceHost{cfg: cfg, log: log, namePtr: namePtr}

	table := []serviceTableEntry{
		{ServiceName: namePtr, ServiceProc: syscall.NewCallback(serviceMain)},
		{ServiceName: nil, ServiceProc: 0},
	}
	r1, _, callErr := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r1 != 0 {
		return nil
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorFailedServiceControllerConnect {
		return errors.New("--service was given but this process was not started by the Service Control Manager; install it as a service or run without --service")
	}
	return fmt.Errorf("StartServiceCtrlDispatcher: %w", callErr)
}

// serviceMain is called by the SCM on its own thread once the dispatcher is running. It reports
// START_PENDING, registers the control handler, reports RUNNING only once the agent graph is up,
// then reports STOP_PENDING and STOPPED around the graceful shutdown.
func serviceMain(argc uint32, argv **uint16) uintptr {
	handle, _, callErr := procRegisterServiceCtrlHandlerExW.Call(
		uintptr(unsafe.Pointer(host.namePtr)),
		syscall.NewCallback(serviceControlHandler),
		0,
	)
	if handle == 0 {
		host.log.Error("RegisterServiceCtrlHandlerEx failed", "error", callErr)
		return errorExceptionInService
	}
	host.handle = syscall.Handle(handle)

	setStatus(serviceStateStartPending, 0, 30000)

	ctx, cancel := context.WithCancel(context.Background())
	host.mu.Lock()
	host.cancel = cancel
	host.mu.Unlock()

	onRunning := func() { setStatus(serviceStateRunning, serviceAcceptStop|serviceAcceptShutdown, 0) }
	err := runServiceContext(ctx, host.cfg, host.log, onRunning)
	cancel()

	if err != nil {
		host.log.Error("service run failed", "error", err)
	}
	setStatus(serviceStateStopPending, 0, 5000)
	setStatus(serviceStateStopped, 0, 0)
	return noError
}

// serviceControlHandler services SCM control requests. Only stop and shutdown are meaningful; any
// other control is accepted and ignored, which keeps the SCM from logging errors.
func serviceControlHandler(control, eventType, eventData, context uintptr) uintptr {
	switch uint32(control) {
	case serviceControlStop, serviceControlShutdown:
		host.stopOnce.Do(func() {
			host.mu.Lock()
			cancel := host.cancel
			host.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		})
	}
	return noError
}

// setStatus reports the service state to the SCM. A zero handle means the handler is not registered
// yet (or has been torn down) and there is nobody to report to.
func setStatus(state, accepted, waitHint uint32) {
	if host.handle == 0 {
		return
	}
	st := serviceStatus{
		ServiceType:      serviceWin32OwnProcess,
		CurrentState:     state,
		ControlsAccepted: accepted,
		Win32ExitCode:    noError,
		WaitHint:         waitHint,
	}
	procSetServiceStatus.Call(uintptr(host.handle), uintptr(unsafe.Pointer(&st)))
}
