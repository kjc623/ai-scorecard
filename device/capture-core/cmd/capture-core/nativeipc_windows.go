//go:build windows

package main

import (
	"context"
	"net"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
)

// nativeServerOwnerTrusted decides whether the pipe's owner is the service.
var nativeServerOwnerTrusted = localipc.ServiceOwner

// dialNative connects a client in the user's session to the service and checks the pipe is the
// service's.
func dialNative(ctx context.Context) (net.Conn, error) {
	return localipc.Dial(ctx, platform.nativeAddr, nativeServerOwnerTrusted)
}

// dialOwnAccount connects to the pipe at addr and checks it is owned by this process's own account
// or by the service's. An elevated process's pipes are owned by Administrators.
func dialOwnAccount(ctx context.Context, addr string) (net.Conn, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	self := tu.User.Sid.String()
	return localipc.Dial(ctx, addr, func(sid string) bool { return sid == self || localipc.ServiceOwner(sid) })
}
