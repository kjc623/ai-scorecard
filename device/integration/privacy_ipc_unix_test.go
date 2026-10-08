//go:build !windows

package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
)

// privacyEndpoint is a private socket for the rig's native endpoint, never the installed
// service's. Its directory is short, because a socket path is limited to about a hundred bytes.
func privacyEndpoint(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sacpriv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "native.sock")
}

// dialPrivacyEndpoint connects as the browser's relay does, checking the socket is served by this
// process's own account.
func dialPrivacyEndpoint(ctx context.Context, addr string) (net.Conn, error) {
	self := uint32(os.Getuid())
	return localipc.Dial(ctx, addr, func(uid uint32) bool { return uid == self || localipc.ServiceUID(uid) })
}
