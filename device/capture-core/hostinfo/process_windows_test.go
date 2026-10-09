//go:build windows

package hostinfo

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// dialEnv names the address the helper process dials. Without it the helper test does nothing.
const dialEnv = "SAC_HOSTINFO_TEST_DIAL"

// TestHelperProcessDials is the client process the TCP owner tests look for: it dials the address
// it is given and holds the connection open until the test closes it.
func TestHelperProcessDials(t *testing.T) {
	addr := os.Getenv(dialEnv)
	if addr == "" {
		t.Skip("run as a helper process only")
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		os.Exit(2)
	}
	_, _ = io.Copy(io.Discard, conn)
	os.Exit(0)
}

// acceptFrom starts cmd, which dials ln, and returns the server's end of its connection.
func acceptFrom(t *testing.T, ln net.Listener, cmd *exec.Cmd) net.Conn {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", cmd.Path, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accepting the connection from %s: %v", cmd.Path, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func ownerOf(t *testing.T, conn net.Conn) uint32 {
	t.Helper()
	local := conn.LocalAddr().(*net.TCPAddr).AddrPort()
	remote := conn.RemoteAddr().(*net.TCPAddr).AddrPort()
	pid, err := OwnerOfLocalTCP(local, remote)
	if err != nil {
		t.Fatalf("OwnerOfLocalTCP(%s, %s): %v", local, remote, err)
	}
	return pid
}

// The owner of a connection is the process that dialled it, not the server that accepted it: the
// test process holds the server's row of the same connection.
func TestOwnerOfLocalTCPFindsTheDiallingProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []struct{ name, addr string }{{"IPv4", "127.0.0.1:0"}, {"IPv6", "[::1]:0"}} {
		t.Run(network.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", network.addr)
			if err != nil {
				t.Fatalf("listen on %s: %v", network.addr, err)
			}
			defer ln.Close()
			cmd := exec.Command(exe, "-test.run=^TestHelperProcessDials$")
			cmd.Env = append(os.Environ(), dialEnv+"="+ln.Addr().String())
			conn := acceptFrom(t, ln, cmd)
			if got, want := ownerOf(t, conn), uint32(cmd.Process.Pid); got != want {
				t.Fatalf("owner = %d, want the dialling process %d (this process is %d)", got, want, os.Getpid())
			}
		})
	}
}

// The test process is a listener of the ports it listens on, in either family, and stops being one
// when it closes them; a port out of range is an error.
func TestListenersOnFindsThisProcess(t *testing.T) {
	me := uint32(os.Getpid())
	for _, network := range []struct{ name, addr string }{{"IPv4", "127.0.0.1:0"}, {"IPv6", "[::1]:0"}} {
		t.Run(network.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", network.addr)
			if err != nil {
				t.Fatalf("listen on %s: %v", network.addr, err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			pids, err := ListenersOn(port)
			if err != nil {
				t.Fatalf("ListenersOn(%d): %v", port, err)
			}
			if !slices.Contains(pids, me) {
				t.Fatalf("ListenersOn(%d) = %v, want this process %d", port, pids, me)
			}
			_ = ln.Close()
			pids, err = ListenersOn(port)
			if err != nil || slices.Contains(pids, me) {
				t.Fatalf("after closing, ListenersOn(%d) = %v, %v; want this process gone", port, pids, err)
			}
		})
	}
	for _, port := range []int{0, 65536} {
		if _, err := ListenersOn(port); err == nil {
			t.Errorf("ListenersOn(%d) has no error", port)
		}
	}
}

// The test binary's own description: its image, its owner, a creation time, and no publisher
// because a test binary is unsigned.
func TestProcessInfoDescribesThisTestBinary(t *testing.T) {
	p, err := ProcessInfo(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("ProcessInfo: %v", err)
	}
	t.Logf("process=%+v user=%+v", p, p.User)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !sameFile(t, p.Image, exe) {
		t.Errorf("image = %q, want this test binary %q", p.Image, exe)
	}
	me, err := processUser()
	if err != nil {
		t.Fatal(err)
	}
	if p.User == nil || p.User.SID != me.SID {
		t.Errorf("user = %+v, want SID %s", p.User, me.SID)
	}
	if p.Started.IsZero() || p.Started.After(time.Now()) {
		t.Errorf("started = %v, want a creation time in the past", p.Started)
	}
	if p.Publisher != "" {
		t.Errorf("publisher = %q, want empty for an unsigned test binary", p.Publisher)
	}
	again, err := ProcessInfo(uint32(os.Getpid()))
	if err != nil || again.Image != p.Image || !again.Started.Equal(p.Started) {
		t.Errorf("second ProcessInfo = %+v, %v; want the same answer", again, err)
	}
}

// curl.exe ships with Windows, signed through a system catalog whose signer is Microsoft; the
// catalog, and so the signer's name, differs between Windows builds.
func TestProcessInfoNamesCurlsPublisher(t *testing.T) {
	curl := filepath.Join(os.Getenv("SystemRoot"), "System32", "curl.exe")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := exec.Command(curl, "-s", "-o", "NUL", "--max-time", "30", "http://"+ln.Addr().String()+"/")
	conn := acceptFrom(t, ln, cmd)
	pid := ownerOf(t, conn)
	if pid != uint32(cmd.Process.Pid) {
		t.Fatalf("owner = %d, want curl.exe's process %d", pid, cmd.Process.Pid)
	}
	p, err := ProcessInfo(pid)
	if err != nil {
		t.Fatalf("ProcessInfo: %v", err)
	}
	t.Logf("process=%+v", p)
	if !sameFile(t, p.Image, curl) {
		t.Errorf("image = %q, want %q", p.Image, curl)
	}
	if !strings.HasPrefix(p.Publisher, "Microsoft") {
		t.Errorf("publisher = %q, want a Microsoft catalog signer", p.Publisher)
	}
	if p.User == nil || !strings.HasPrefix(p.User.SID, "S-1-") {
		t.Errorf("user = %+v, want curl.exe's owner", p.User)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	sa, err := os.Stat(a)
	if err != nil {
		t.Errorf("stat %s: %v", a, err)
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		t.Errorf("stat %s: %v", b, err)
		return false
	}
	return os.SameFile(sa, sb)
}
