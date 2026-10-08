//go:build windows

package etwsession

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// kernelProcess is Microsoft-Windows-Kernel-Process with WINEVENT_KEYWORD_PROCESS.
var kernelProcess = Provider{GUID: "{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}", Level: 4, Keywords: 0x10, EventIDs: []uint16{1, 2}}

// requireElevation skips a test that starts a real session: StartTrace needs administrative rights.
func requireElevation(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("starting an ETW session needs an elevated (administrator) process; run this test elevated")
	}
}

func testName(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "test-" + hex.EncodeToString(b[:])
}

// waitClosed waits for a session's events to end.
func waitClosed(t *testing.T, s *Session, why string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("the session's events did not end %s", why)
		}
	}
}

// A session left running under the same name, as a crashed service leaves it, is stopped and
// replaced: its reader sees its events end, and the new session opens.
func TestOpenReplacesAStaleSession(t *testing.T) {
	requireElevation(t)
	name := testName(t)
	stale, err := Open(name, []Provider{kernelProcess})
	if err != nil {
		t.Fatalf("opening %s%s: %v", Prefix, name, err)
	}
	defer stale.Close()

	fresh, err := Open(name, []Provider{kernelProcess})
	if err != nil {
		t.Fatalf("opening %s%s over a stale session: %v", Prefix, name, err)
	}
	defer fresh.Close()
	waitClosed(t, stale, "when it was replaced")
}

// Close ends a session's events, and a second Close does nothing.
func TestCloseEndsEvents(t *testing.T) {
	requireElevation(t)
	s, err := Open(testName(t), []Provider{kernelProcess})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close()
	waitClosed(t, s, "on Close")
}
