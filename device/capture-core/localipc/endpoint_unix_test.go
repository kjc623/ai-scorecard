//go:build !windows

package localipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// testAddr is a socket private to the test.
func testAddr(t *testing.T) string {
	// A socket path is limited to about 100 bytes, which t.TempDir can exceed.
	dir, err := os.MkdirTemp("", "sac-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "native.sock")
}

// dialSelf dials addr trusting this process's own account when trust is set, and no account
// otherwise.
func dialSelf(ctx context.Context, addr string, trust bool) (net.Conn, error) {
	self := uint32(os.Getuid())
	return Dial(ctx, addr, func(uid uint32) bool { return trust && uid == self })
}

func TestServiceUIDIsRoot(t *testing.T) {
	if !ServiceUID(0) || ServiceUID(1000) {
		t.Fatal("only root is the service's account")
	}
}

// A socket left behind is replaced, anything else at the path is refused, and the socket is
// connectable by every account.
func TestListenReplacesAStaleSocketOnly(t *testing.T) {
	addr := testAddr(t)
	ln, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ln, err = Listen(addr)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	defer ln.Close()
	if st, err := os.Stat(addr); err != nil || st.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode = %v, %v; want 0666", st.Mode(), err)
	}
	if st, err := os.Stat(filepath.Dir(addr)); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("directory mode = %v, %v; want 0755", st.Mode(), err)
	}

	other := filepath.Join(filepath.Dir(addr), "file.sock")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen(other); err == nil {
		ln.Close()
		t.Fatal("a regular file at the endpoint was replaced")
	}
}
