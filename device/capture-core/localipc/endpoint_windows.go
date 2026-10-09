//go:build windows

package localipc

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// Endpoint is the named pipe the service listens on.
const Endpoint = `\\.\pipe\ShadowAICapture.native`

// pipeSDDL lets SYSTEM and Administrators do anything with the pipe and interactive users read and
// write it, which is what a process in a signed-in user's session needs. go-winio creates the first
// instance exclusively (so a pipe someone else created first makes the service fail to listen
// rather than share the name) and refuses remote clients.
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)"

// Listen creates the pipe at addr.
func Listen(addr string) (net.Listener, error) {
	return winio.ListenPipe(addr, &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

// pipeHandle returns the handle under a go-winio pipe connection.
func pipeHandle(conn net.Conn) (windows.Handle, error) {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return 0, fmt.Errorf("connection %T is not a named pipe", conn)
	}
	return windows.Handle(f.Fd()), nil
}

// PeerUser names the account the connecting process runs as, from the client process id the pipe
// reports and that process's token.
func PeerUser(conn net.Conn) (hostinfo.User, error) {
	h, err := pipeHandle(conn)
	if err != nil {
		return hostinfo.User{}, err
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(h, &pid); err != nil {
		return hostinfo.User{}, fmt.Errorf("GetNamedPipeClientProcessId: %w", err)
	}
	return hostinfo.UserOfProcess(pid)
}

// Dial connects to the endpoint at addr and checks that ownerTrusted accepts the SID that owns the
// pipe. The client connects at the anonymous impersonation level, so the server cannot act as the
// client's user.
func Dial(ctx context.Context, addr string, ownerTrusted func(sid string) bool) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, addr)
	if err != nil {
		return nil, err
	}
	if err := verifyServer(conn, ownerTrusted); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// ServiceOwner reports whether a pipe owned by sid was created by the service. The pipe is owned
// by the account that created it; the service runs as LocalSystem, whose objects are owned by
// SYSTEM or Administrators. A pipe another user created first is owned by that user, so a client
// never hands anything to it.
func ServiceOwner(sid string) bool {
	return sid == "S-1-5-18" || sid == "S-1-5-32-544"
}

func verifyServer(conn net.Conn, ownerTrusted func(string) bool) error {
	h, err := pipeHandle(conn)
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the pipe's owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("reading the pipe's owner: %w", err)
	}
	if !ownerTrusted(owner.String()) {
		return errors.New("the native messaging endpoint is not owned by the Shadow AI Capture service")
	}
	return nil
}
