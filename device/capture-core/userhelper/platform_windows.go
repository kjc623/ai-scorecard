//go:build windows

package userhelper

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// errNoUser: nobody is signed in to the session (WTSQueryUserToken answers ERROR_NO_TOKEN).
var errNoUser = errors.New("userhelper: nobody is signed in to the session")

// SystemPlatform starts helpers as exe with args, in the signed-in sessions of this machine. The
// service needs SE_TCB_NAME (LocalSystem has it) to read a session's user token.
func SystemPlatform(exe string, args ...string) Platform {
	return windowsPlatform{exe: exe, args: args}
}

type windowsPlatform struct {
	exe  string
	args []string
}

// Sessions lists the sessions in state WTSActive or WTSDisconnected that have a signed-in user. A
// fast-user-switched user is disconnected but still signed in, and still runs tools. Session 0 is
// the services' session and never has one.
func (windowsPlatform) Sessions() ([]Session, error) {
	var info *windows.WTS_SESSION_INFO
	var count uint32
	// WTS_CURRENT_SERVER_HANDLE is 0; the reserved argument is 0 and the version 1.
	if err := windows.WTSEnumerateSessions(0, 0, 1, &info, &count); err != nil {
		return nil, fmt.Errorf("WTSEnumerateSessionsW: %w", err)
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))
	var out []Session
	var errs []error
	for _, s := range unsafe.Slice(info, count) {
		if s.SessionID == 0 || (s.State != windows.WTSActive && s.State != windows.WTSDisconnected) {
			continue
		}
		u, err := sessionUser(s.SessionID)
		switch {
		case errors.Is(err, errNoUser):
		case err != nil:
			errs = append(errs, err)
		default:
			out = append(out, Session{ID: s.SessionID, User: u})
		}
	}
	return out, errors.Join(errs...)
}

// Owner names the user signed in to the session.
func (windowsPlatform) Owner(sessionID uint32) (hostinfo.User, error) {
	return sessionUser(sessionID)
}

// Launch starts the helper in the session with the signed-in user's token and environment, on the
// interactive desktop, with no window.
func (p windowsPlatform) Launch(sessionID uint32) (Process, error) {
	tok, err := userToken(sessionID)
	if err != nil {
		return nil, err
	}
	defer tok.Close()
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, tok, false); err != nil {
		return nil, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(env)

	app, err := windows.UTF16PtrFromString(p.exe)
	if err != nil {
		return nil, err
	}
	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{p.exe}, p.args...)))
	if err != nil {
		return nil, err
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(p.exe))
	if err != nil {
		return nil, err
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return nil, err
	}
	si := windows.StartupInfo{Desktop: desktop}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcessAsUser(tok, app, cmdLine, nil, nil, false, flags, env, dir, &si, &pi); err != nil {
		return nil, fmt.Errorf("CreateProcessAsUserW in session %d: %w", sessionID, err)
	}
	_ = windows.CloseHandle(pi.Thread)
	proc := &winProcess{h: pi.Process, pid: pi.ProcessId, done: make(chan struct{})}
	go proc.wait()
	return proc, nil
}

// userToken is the primary token of the user signed in to the session.
func userToken(sessionID uint32) (windows.Token, error) {
	var tok windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &tok); err != nil {
		if errors.Is(err, windows.ERROR_NO_TOKEN) {
			return 0, errNoUser
		}
		return 0, fmt.Errorf("WTSQueryUserToken(%d): %w", sessionID, err)
	}
	return tok, nil
}

// sessionUser names the user signed in to the session by SID and DOMAIN\user.
func sessionUser(sessionID uint32) (hostinfo.User, error) {
	tok, err := userToken(sessionID)
	if err != nil {
		return hostinfo.User{}, err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return hostinfo.User{}, fmt.Errorf("the user of session %d: %w", sessionID, err)
	}
	u := hostinfo.User{SID: tu.User.Sid.String()}
	if account, domain, _, err := tu.User.Sid.LookupAccount(""); err == nil {
		u.Account = strings.TrimPrefix(domain+`\`+account, `\`)
	}
	return u, nil
}

// winProcess is a started helper. Its handle is held until the process exits.
type winProcess struct {
	pid  uint32
	done chan struct{}

	mu     sync.Mutex
	h      windows.Handle
	exited bool
}

func (p *winProcess) PID() uint32           { return p.pid }
func (p *winProcess) Done() <-chan struct{} { return p.done }

func (p *winProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return nil
	}
	err := windows.TerminateProcess(p.h, 1)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// The process is already exiting.
		return nil
	}
	return err
}

func (p *winProcess) wait() {
	_, _ = windows.WaitForSingleObject(p.h, windows.INFINITE)
	p.mu.Lock()
	p.exited = true
	_ = windows.CloseHandle(p.h)
	p.mu.Unlock()
	close(p.done)
}
