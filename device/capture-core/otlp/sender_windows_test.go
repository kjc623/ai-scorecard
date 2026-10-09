//go:build windows

package otlp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// On Windows the sender is named by the system's TCP owner table and process lookups: a client this
// test process dials to either listener is attributed to this process and the account it runs as.
func TestSenderIsNamedByTheTCPOwnerTable(t *testing.T) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := tu.User.Sid.String()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	f := newReceiver(t, "127.0.0.1:0", "127.0.0.1:0")
	f.r.owner = processOf
	if err := f.r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := dialClients(t, f)
	if code := c.postLogs(t, c.token); code != http.StatusOK {
		t.Fatalf("http export: %d", code)
	}
	c.exportLogs(t)

	senders := f.senders()
	if len(senders) != 2 {
		t.Fatalf("normalizer got %d senders, want 2", len(senders))
	}
	for _, s := range senders {
		t.Logf("sender %+v person %+v", s, s.Person)
		if !s.Resolved || s.PID != uint32(os.Getpid()) {
			t.Errorf("sender pid %d resolved %v, want this process %d", s.PID, s.Resolved, os.Getpid())
		}
		if !strings.EqualFold(imageBase(s.Image), filepath.Base(exe)) {
			t.Errorf("sender image %q, want this test binary %q", s.Image, exe)
		}
		if s.Person == nil || s.Person.UserRef != testPerson(hostinfo.User{SID: sid}).UserRef {
			t.Errorf("sender person %+v, want the account with SID %s", s.Person, sid)
		}
	}
	if n := f.errorCount(); n != 0 {
		t.Fatalf("errors = %d after attributed requests", n)
	}
	if lines := f.connectionLines(); len(lines) != 2 {
		t.Fatalf("logged %d connection lines, want 2: %q", len(lines), f.log.all())
	}
}
