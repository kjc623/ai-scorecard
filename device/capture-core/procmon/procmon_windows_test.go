//go:build windows

package procmon

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// childEnv marks the test binary's run as the child the real-session test watches.
const childEnv = "SAC_PROCMON_TEST_CHILD"

// TestHelperChild is the child process: it waits briefly so its start and its stop are two
// distinct events, and exits.
func TestHelperChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("run as a helper process only")
	}
	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
}

// A real Kernel-Process session delivers the start of the test's own child process, with its
// process id and image name, and then its stop. It needs an elevated process.
func TestRealSessionSeesChildProcess(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("starting an ETW session needs an elevated (administrator) process; run this test elevated")
	}
	src, err := KernelEvents()
	if err != nil {
		t.Fatalf("opening the Kernel-Process session: %v", err)
	}
	defer src.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperChild$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := uint32(cmd.Process.Pid)
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	base := baseName(exe)
	var sawStart, sawStop bool
	deadline := time.After(15 * time.Second)
	for !sawStart || !sawStop {
		select {
		case ev, ok := <-src.Events():
			if !ok {
				t.Fatal("the session stopped delivering events")
			}
			if !strings.EqualFold(ev.Provider, KernelProcess.GUID) {
				t.Fatalf("event from provider %s, want %s", ev.Provider, KernelProcess.GUID)
			}
			got, ok := uintProperty(ev, "ProcessID")
			if !ok || got != pid {
				continue
			}
			switch ev.ID {
			case eventStart:
				if image := baseName(ev.Properties["ImageName"]); !strings.EqualFold(image, base) {
					t.Fatalf("ProcessStart names image %q (%q), want %q", image, ev.Properties["ImageName"], base)
				}
				if parent, ok := uintProperty(ev, "ParentProcessID"); !ok || parent != uint32(os.Getpid()) {
					t.Fatalf("ProcessStart names parent %d, want %d", parent, os.Getpid())
				}
				if ev.Time.IsZero() {
					t.Fatal("ProcessStart has no time")
				}
				sawStart = true
			case eventStop:
				if !sawStart {
					t.Fatal("ProcessStop arrived before ProcessStart")
				}
				sawStop = true
			}
		case <-deadline:
			t.Fatalf("within 15 s the session delivered start %v and stop %v for child %d", sawStart, sawStop, pid)
		}
	}
	if err := <-waitErr; err != nil {
		t.Fatalf("the child: %v", err)
	}
}

// The process list includes the test process itself, by its image name.
func TestSnapshotListsThisProcess(t *testing.T) {
	procs, err := snapshot()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.pid == uint32(os.Getpid()) {
			if !strings.EqualFold(p.base, baseName(exe)) {
				t.Fatalf("the process list names this process %q, want %q", p.base, baseName(exe))
			}
			return
		}
	}
	t.Fatalf("the process list of %d processes does not include this process", len(procs))
}

// A system image's version resource reads as four numbers; a file without one reads empty.
func TestFileVersion(t *testing.T) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if v := fileVersion(dir + `\kernel32.dll`); strings.Count(v, ".") != 3 {
		t.Fatalf("kernel32.dll's version = %q, want major.minor.build.revision", v)
	}
	if v := fileVersion(dir + `\drivers\etc\hosts`); v != "" {
		t.Fatalf("hosts has version %q, want none", v)
	}
}
