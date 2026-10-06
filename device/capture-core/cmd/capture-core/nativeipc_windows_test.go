//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

// trustTestServer makes the relay accept a pipe served by the test process's own account, which
// is what an in-process service is.
func trustTestServer(t *testing.T) {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	self := tu.User.Sid.String()
	prev := nativeServerOwnerTrusted
	nativeServerOwnerTrusted = func(sid string) bool { return sid == self || prev(sid) }
	t.Cleanup(func() { nativeServerOwnerTrusted = prev })
}

func distrustTestServer(t *testing.T) {
	t.Helper()
	prev := nativeServerOwnerTrusted
	nativeServerOwnerTrusted = func(string) bool { return false }
	t.Cleanup(func() { nativeServerOwnerTrusted = prev })
}
