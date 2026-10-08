//go:build !windows

package localipc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// Endpoint is the Unix socket the service listens on. Its directory is created by the service
// (running as root) with mode 0755, so no other account can place a socket there, and the socket
// itself is 0666 so a process in any user's session can connect.
const Endpoint = "/var/run/shadow-ai-capture/native.sock"

// Listen creates the socket at addr.
func Listen(addr string) (net.Listener, error) {
	dir := filepath.Dir(addr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, err
	}
	// A socket left by a previous run is replaced; anything else at the path is refused.
	if st, err := os.Lstat(addr); err == nil {
		if st.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", addr)
		}
		if err := os.Remove(addr); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(addr, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// connUID returns the uid of the process at the other end of a Unix socket.
func connUID(conn net.Conn) (uint32, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("connection %T is not a Unix socket", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var credErr error
	if err := raw.Control(func(fd uintptr) { uid, credErr = socketPeerUID(int(fd)) }); err != nil {
		return 0, err
	}
	return uid, credErr
}

// PeerUser names the account of the connecting process from the socket's peer credentials.
func PeerUser(conn net.Conn) (hostinfo.User, error) {
	uid, err := connUID(conn)
	if err != nil {
		return hostinfo.User{}, err
	}
	return hostinfo.UserOfUID(uid)
}

// ServiceUID reports whether a socket served by uid is the service's, which runs as root.
func ServiceUID(uid uint32) bool { return uid == 0 }

// Dial connects to the socket at addr and checks that uidTrusted accepts the uid serving it.
func Dial(ctx context.Context, addr string, uidTrusted func(uid uint32) bool) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", addr)
	if err != nil {
		return nil, err
	}
	uid, err := connUID(conn)
	if err == nil && !uidTrusted(uid) {
		err = errors.New("the native messaging endpoint is not served by the Shadow AI Capture service")
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
