//go:build !windows

package userhelper

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
)

// testAddr is a socket private to the test.
func testAddr(t *testing.T) string {
	// A socket path is limited to about 100 bytes, which t.TempDir can exceed.
	dir, err := os.MkdirTemp("", "sac-helper-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "native.sock")
}

// dialAny connects to the test's own endpoint, whoever serves it.
func dialAny(ctx context.Context, addr string) (net.Conn, error) {
	return localipc.Dial(ctx, addr, func(uint32) bool { return true })
}

// Elsewhere than Windows there is no helper yet: the provider reports absent.
func TestSystemPlatformIsAbsentHere(t *testing.T) {
	if SystemPlatform("/opt/shadow-ai-capture/bin/capture-core", "--user-helper") != nil {
		t.Fatal("a platform without helpers offers one")
	}
}
