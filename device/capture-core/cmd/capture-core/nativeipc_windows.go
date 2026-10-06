//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// nativeEndpoint is the named pipe the service listens on for the browser relay.
const nativeEndpoint = `\\.\pipe\ShadowAICapture.native`

// nativePipeSDDL lets SYSTEM and Administrators do anything with the pipe and interactive users
// read and write it, which is what a relay running in a signed-in user's browser needs. go-winio
// creates the first instance exclusively (so a pipe someone else created first makes the service
// fail to listen rather than share the name) and refuses remote clients.
const nativePipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)"

func listenNative(addr string) (net.Listener, error) {
	return winio.ListenPipe(addr, &winio.PipeConfig{
		SecurityDescriptor: nativePipeSDDL,
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

// peerUser names the account the connecting process runs as, from the client process id the pipe
// reports and that process's token.
func peerUser(conn net.Conn) (hostinfo.User, error) {
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

// dialNative connects the relay to the service. The client connects at the anonymous
// impersonation level, so the server cannot act as the browser's user.
func dialNative(ctx context.Context) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, platform.nativeAddr)
	if err != nil {
		return nil, err
	}
	if err := verifyNativeServer(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// nativeServerOwnerTrusted decides whether the pipe's owner is the service. The pipe is owned by
// the account that created it; the service runs as LocalSystem, whose objects are owned by SYSTEM
// or Administrators. A pipe another user created first is owned by that user, so the relay never
// hands the browser's observations to it.
var nativeServerOwnerTrusted = func(sid string) bool {
	return sid == "S-1-5-18" || sid == "S-1-5-32-544"
}

func verifyNativeServer(conn net.Conn) error {
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
	if !nativeServerOwnerTrusted(owner.String()) {
		return errors.New("the native messaging endpoint is not owned by the Shadow AI Capture service")
	}
	return nil
}
