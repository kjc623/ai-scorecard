//go:build !windows

package main

import (
	"context"
	"net"
	"os"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
)

// nativeServerUIDTrusted decides whether the socket's server is the service, which runs as root.
var nativeServerUIDTrusted = localipc.ServiceUID

// dialNative connects a client in the user's session to the service and checks the server runs as
// root.
func dialNative(ctx context.Context) (net.Conn, error) {
	return localipc.Dial(ctx, platform.nativeAddr, nativeServerUIDTrusted)
}

// dialOwnAccount connects to the socket at addr and checks the server runs as this process's own
// account or as the service's.
func dialOwnAccount(ctx context.Context, addr string) (net.Conn, error) {
	self := uint32(os.Getuid())
	return localipc.Dial(ctx, addr, func(uid uint32) bool { return uid == self || localipc.ServiceUID(uid) })
}
