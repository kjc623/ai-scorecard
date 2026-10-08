//go:build windows

package localipc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"testing"

	"golang.org/x/sys/windows"
)

// testAddr is a pipe private to the test.
func testAddr(t *testing.T) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return `\\.\pipe\ShadowAICapture.localipc.test-` + hex.EncodeToString(b[:])
}

// dialSelf dials addr trusting this process's own account when trust is set, and no account
// otherwise.
func dialSelf(ctx context.Context, addr string, trust bool) (net.Conn, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	self := tu.User.Sid.String()
	return Dial(ctx, addr, func(sid string) bool { return trust && sid == self })
}

func TestServiceOwnerIsSystemOrAdministrators(t *testing.T) {
	if !ServiceOwner("S-1-5-18") || !ServiceOwner("S-1-5-32-544") || ServiceOwner("S-1-5-21-1-2-3-1001") {
		t.Fatal("only SYSTEM and Administrators own the service's pipe")
	}
}
