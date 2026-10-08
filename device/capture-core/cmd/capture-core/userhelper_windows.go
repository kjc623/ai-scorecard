//go:build windows

package main

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/userhelper"
	"github.com/shadow-ai-capture/device/protocol"
)

// runUserHelper is the user-session helper: it connects to the service, names its session, and
// shows the service's notifications as toasts until the service closes the connection.
func runUserHelper() error {
	pid := windows.GetCurrentProcessId()
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(pid, &sessionID); err != nil {
		return fmt.Errorf("ProcessIdToSessionId: %w", err)
	}
	// A toaster that cannot start answers every notification with its error, so the service sees
	// why nothing was shown.
	toaster, toastErr := userhelper.NewToaster(userhelper.AppUserModelID)
	show := func(n protocol.Notify) error {
		if toastErr != nil {
			return toastErr
		}
		return toaster.Show(n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayConnectBudget)
	conn, err := dialWithRetry(ctx, dialNative)
	cancel()
	if err != nil {
		return fmt.Errorf("the Shadow AI Capture service is not reachable: %w", err)
	}
	defer conn.Close()
	return userhelper.RunHelper(conn, protocol.HelperHello{SessionID: sessionID, PID: pid}, show)
}
