//go:build !windows

package main

import (
	"os"
	"testing"
)

// trustTestServer makes the relay accept a socket served by the test process's own account, which
// is what an in-process service is.
func trustTestServer(t *testing.T) {
	t.Helper()
	self := uint32(os.Getuid())
	prev := nativeServerUIDTrusted
	nativeServerUIDTrusted = func(uid uint32) bool { return uid == self || prev(uid) }
	t.Cleanup(func() { nativeServerUIDTrusted = prev })
}

func distrustTestServer(t *testing.T) {
	t.Helper()
	prev := nativeServerUIDTrusted
	nativeServerUIDTrusted = func(uint32) bool { return false }
	t.Cleanup(func() { nativeServerUIDTrusted = prev })
}
