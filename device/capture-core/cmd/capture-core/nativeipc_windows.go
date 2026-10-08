//go:build windows

package main

import (
	"context"
	"net"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
)

// nativeServerOwnerTrusted decides whether the pipe's owner is the service.
var nativeServerOwnerTrusted = localipc.ServiceOwner

// dialNative connects a client in the user's session to the service and checks the pipe is the
// service's.
func dialNative(ctx context.Context) (net.Conn, error) {
	return localipc.Dial(ctx, platform.nativeAddr, nativeServerOwnerTrusted)
}
