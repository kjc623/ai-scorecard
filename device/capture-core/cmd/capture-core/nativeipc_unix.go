//go:build !windows

package main

import (
	"context"
	"net"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
)

// nativeServerUIDTrusted decides whether the socket's server is the service, which runs as root.
var nativeServerUIDTrusted = localipc.ServiceUID

// dialNative connects a client in the user's session to the service and checks the server runs as
// root.
func dialNative(ctx context.Context) (net.Conn, error) {
	return localipc.Dial(ctx, platform.nativeAddr, nativeServerUIDTrusted)
}
