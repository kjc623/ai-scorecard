//go:build windows

package userhelper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/protocol"
)

// testHelperArg makes this test binary act as the helper: the integration test starts it in the
// console session with the test's pipe as the next argument.
const testHelperArg = "--user-helper"

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == testHelperArg {
		if err := runTestBinaryHelper(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "userhelper test helper: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runTestBinaryHelper is the production helper's steps against the test's pipe.
func runTestBinaryHelper(addr string) error {
	pid := windows.GetCurrentProcessId()
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(pid, &sessionID); err != nil {
		return err
	}
	toaster, err := NewToaster(AppUserModelID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := localipc.Dial(ctx, addr, localipc.ServiceOwner)
	if err != nil {
		return err
	}
	defer conn.Close()
	return RunHelper(conn, protocol.HelperHello{SessionID: sessionID, PID: pid}, toaster.Show)
}

// testAddr is a pipe private to the test.
func testAddr(t *testing.T) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return `\\.\pipe\ShadowAICapture.userhelper.test-` + hex.EncodeToString(b[:])
}

// dialAny connects to the test's own endpoint, whoever serves it.
func dialAny(ctx context.Context, addr string) (net.Conn, error) {
	return localipc.Dial(ctx, addr, func(string) bool { return true })
}

// canQuerySessions reports whether this process may read a session's user token, which only
// LocalSystem (SE_TCB_NAME) may.
func canQuerySessions(t *testing.T) (console uint32, ok bool) {
	t.Helper()
	console = windows.WTSGetActiveConsoleSessionId()
	if console == 0xFFFFFFFF {
		return 0, false
	}
	var tok windows.Token
	err := windows.WTSQueryUserToken(console, &tok)
	if err != nil {
		return console, false
	}
	tok.Close()
	return console, true
}

// The real platform lists the signed-in sessions, or says plainly why it may not.
func TestSystemPlatformListsTheSignedInSessions(t *testing.T) {
	exe, _ := os.Executable()
	p := SystemPlatform(exe, testHelperArg)
	sessions, err := p.Sessions()
	if _, ok := canQuerySessions(t); !ok {
		if err != nil && !strings.Contains(err.Error(), "WTSQueryUserToken") {
			t.Fatalf("Sessions as a process without SE_TCB_NAME = %v, want a WTSQueryUserToken error or none", err)
		}
		t.Skipf("not LocalSystem: the session users cannot be read (%v)", err)
	}
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	for _, s := range sessions {
		if s.ID == 0 || s.User.SID == "" {
			t.Errorf("session %+v is not a signed-in interactive session", s)
		}
		owner, err := p.Owner(s.ID)
		if err != nil || !sameUser(owner, s.User) {
			t.Errorf("Owner(%d) = %+v, %v; want %+v", s.ID, owner, err, s.User)
		}
	}
}

func TestToasterInitialisesTheWindowsRuntime(t *testing.T) {
	if _, err := NewToaster(AppUserModelID); err != nil {
		t.Fatalf("NewToaster: %v", err)
	}
}

// consoleOnly is the real platform restricted to the console session.
type consoleOnly struct {
	Platform
	console uint32
}

func (c consoleOnly) Sessions() ([]Session, error) {
	all, err := c.Platform.Sessions()
	var out []Session
	for _, s := range all {
		if s.ID == c.console {
			out = append(out, s)
		}
	}
	return out, err
}

// The integration test of Notify on a real device: as LocalSystem, it starts this test binary as
// the helper in the console session with CreateProcessAsUserW, accepts its helper_hello as the
// console user's, and shows one toast under the installed shortcut's AppUserModelID. It needs the
// MSI's Start-menu shortcut, and runs only as LocalSystem.
func TestNotifyShowsAToastInTheConsoleSession(t *testing.T) {
	console, ok := canQuerySessions(t)
	if !ok {
		t.Skip("needs LocalSystem and a signed-in console user (WTSQueryUserToken)")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr := testAddr(t)
	p := New(Config{Platform: consoleOnly{Platform: SystemPlatform(exe, testHelperArg, addr), console: console}})
	srv := localipc.NewServer(addr, func(conn net.Conn, peer hostinfo.User) {
		payload, err := localipc.ReadFrame(conn)
		if err != nil {
			return
		}
		var first protocol.NativeMessage
		if json.Unmarshal(payload, &first) == nil && first.Type == protocol.TypeHelperHello {
			p.Serve(conn, peer, first)
		}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())

	deadline := time.Now().Add(20 * time.Second)
	for {
		err = p.Notify(console, protocol.Notify{
			Title: "Shadow AI Capture",
			Body:  "userhelper integration test: this notification was sent by the service to the console session's helper.",
		})
		if !errors.Is(err, ErrNoHelper) || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Notify in console session %d: %v", console, err)
	}
	launched := 0
	p.mu.Lock()
	if s := p.sessions[console]; s != nil && s.proc != nil {
		launched = int(s.proc.PID())
	}
	p.mu.Unlock()
	t.Logf("toast shown by helper pid %d in console session %d", launched, console)
}
