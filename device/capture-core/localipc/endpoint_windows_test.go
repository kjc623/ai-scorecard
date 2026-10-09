//go:build windows

package localipc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// testAddr is a pipe private to the test.
func testAddr(t *testing.T) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return `\\.\pipe\ShadowAICapture.localipc.test-` + hex.EncodeToString(b[:])
}

// dialSelf dials addr trusting the account that owns what this process creates when trust is
// set, and no account otherwise. That is the token's owner: the user, or Administrators when the
// process is elevated.
func dialSelf(ctx context.Context, addr string, trust bool) (net.Conn, error) {
	self, err := tokenOwner()
	if err != nil {
		return nil, err
	}
	return Dial(ctx, addr, func(sid string) bool { return trust && sid == self })
}

func tokenOwner() (string, error) {
	token := windows.GetCurrentProcessToken()
	var n uint32
	_ = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &n)
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], n, &n); err != nil {
		return "", err
	}
	// TOKEN_OWNER is one pointer: the owner SID.
	return (*(**windows.SID)(unsafe.Pointer(&buf[0]))).String(), nil
}

func TestServiceOwnerIsSystemOrAdministrators(t *testing.T) {
	if !ServiceOwner("S-1-5-18") || !ServiceOwner("S-1-5-32-544") || ServiceOwner("S-1-5-21-1-2-3-1001") {
		t.Fatal("only SYSTEM and Administrators own the service's pipe")
	}
}
