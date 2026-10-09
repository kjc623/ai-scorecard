//go:build windows

package inventory

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// thisUserHost is the machine, with one profile: the test process's own user, whose profile folder
// is a fixture.
type thisUserHost struct {
	ModelHost
	user hostinfo.User
	home string
}

func (h thisUserHost) Profiles() ([]Profile, error) {
	return []Profile{{User: h.user, Dir: h.home, Loaded: true}}, nil
}

// The machine's process list includes the test process, by its image's base name.
func TestSystemModelHostListsThisProcess(t *testing.T) {
	procs, err := SystemModelHost().Processes()
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == uint32(os.Getpid()) {
			if p.Name != filepath.Base(self) {
				t.Fatalf("this process is named %q, want %q", p.Name, filepath.Base(self))
			}
			return
		}
	}
	t.Fatalf("this process (%d) is not among %d processes", os.Getpid(), len(procs))
}

// A user value that is not set, or of a hive that is not loaded, is empty.
func TestSystemModelHostUserEnvAbsent(t *testing.T) {
	host := SystemModelHost()
	if v, err := host.UserEnv("S-1-5-21-0-0-0-999999", ollamaModelsVar); v != "" || err != nil {
		t.Errorf("an unloaded hive: %q, %v", v, err)
	}
}

// The real seams together: a catalog runtime whose executable is the test binary and whose port the
// test listens on is found running as the test's user, with basis port_listen, and with the test
// binary's (empty) file version.
func TestModelScannerFindsThisProcessListening(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := hostinfo.ProcessInfo(uint32(os.Getpid()))
	if err != nil || info.User == nil {
		t.Fatalf("ProcessInfo: %+v, %v", info, err)
	}
	if !userSID(info.User.SID) {
		t.Skipf("the test runs as %s, which is not a person's account", info.User.SID)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	b := &policy.Bundle{Catalog: []policy.CatalogApp{{AppKey: "fixture_runtime", Category: "local_runtime", Signals: []policy.CatalogSignal{
		{Platform: "windows", Kind: policy.SignalWindowsExe, Value: filepath.Base(self)},
		{Platform: "any", Kind: policy.SignalListenPort, Value: strconv.Itoa(port)},
	}}}}
	host := thisUserHost{ModelHost: SystemModelHost(), user: *info.User, home: t.TempDir()}
	recs, errs := NewModelScanner(host, person).Scan(context.Background(), b)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(recs) != 1 || recs[0].AppKey != "fixture_runtime" || recs[0].Basis != protocol.DetectionBasisPortListen ||
		recs[0].Version != "" || recs[0].Type != protocol.DiscoveryTypeLocalModel {
		t.Fatalf("records %+v, want the test process listening", recs)
	}
}
