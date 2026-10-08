//go:build windows

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
)

// privacyEndpoint is a private pipe for the rig's native endpoint, never the installed service's.
func privacyEndpoint(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return `\\.\pipe\ShadowAICapture.native.privacy-` + hex.EncodeToString(b)
}

// dialPrivacyEndpoint connects as the browser's relay does, checking the pipe is owned by this
// process's own account.
func dialPrivacyEndpoint(ctx context.Context, addr string) (net.Conn, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	self := tu.User.Sid.String()
	return localipc.Dial(ctx, addr, func(sid string) bool { return sid == self || localipc.ServiceOwner(sid) })
}
