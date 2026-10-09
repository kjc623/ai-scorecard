package winproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/policy"
)

func recordingServer(s *seam, recordFile string) *Server {
	return New(Config{
		Bundles:      func() *policy.Bundle { return testBundle() },
		ProxyAddr:    func() string { return "127.0.0.1:8843" },
		Users:        func(context.Context) ([]string, error) { return s.users, nil },
		OpenSettings: s.open,
		FetchPAC:     s.fetch,
		RecordFile:   recordFile,
	})
}

// A user's previous AutoConfigURL is recorded before the PAC replaces it and forgotten once it is
// restored, so the record holds exactly the users the running service has not restored.
func TestServerRecordsTheOriginalUntilRestored(t *testing.T) {
	s := newSeam()
	s.regs["S-1-5-21-1000"] = newFakeReg()
	s.regs["S-1-5-21-1000"].strings["AutoConfigURL"] = "http://corp/proxy.pac"
	s.fetched["http://corp/proxy.pac"] = `function FindProxyForURL(u,h){ return "DIRECT"; }`
	file := filepath.Join(t.TempDir(), "winproxy", "records.json")
	srv := recordingServer(s, file)

	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	all, err := (&records{path: file}).read()
	if err != nil {
		t.Fatal(err)
	}
	rec := all["S-1-5-21-1000"]
	if len(all) != 1 || !rec.HadOriginal || rec.Original != "http://corp/proxy.pac" || rec.Applied != s.regs["S-1-5-21-1000"].strings["AutoConfigURL"] {
		t.Fatalf("records = %+v; want the user's original and the applied URL", all)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record file outlived the restore: %v", err)
	}
}

// A run that ended without restoring (a crash) leaves the PAC in place; the next run takes the
// original from the record, not the stale PAC URL, and restores the user to it.
func TestServerAppliesAgainOverAnUnrestoredPAC(t *testing.T) {
	s := newSeam()
	s.regs["S-1-5-21-1000"] = newFakeReg()
	s.regs["S-1-5-21-1000"].strings["AutoConfigURL"] = "http://corp/proxy.pac"
	s.fetched["http://corp/proxy.pac"] = `function FindProxyForURL(u,h){ return "DIRECT"; }`
	file := filepath.Join(t.TempDir(), "records.json")

	crashed := recordingServer(s, file)
	if err := crashed.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	crashed.http.Close()

	next := recordingServer(s, file)
	if err := next.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := next.originals["S-1-5-21-1000"].AutoConfigURL; got != "http://corp/proxy.pac" {
		t.Fatalf("the original the PAC delegates to is %q; want the user's own PAC", got)
	}
	if err := next.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.regs["S-1-5-21-1000"].strings["AutoConfigURL"]; got != "http://corp/proxy.pac" {
		t.Fatalf("AutoConfigURL after the restore = %q; want the original", got)
	}
}

// At uninstall the record restores every user whose AutoConfigURL is still the agent's PAC, leaves
// a user who changed it since, and keeps a user whose settings cannot be opened.
func TestRestoreRecorded(t *testing.T) {
	s := newSeam()
	s.users = []string{"S-1-5-21-1000", "S-1-5-21-1001", "S-1-5-21-1002", "S-1-5-21-1003"}
	s.regs["S-1-5-21-1000"] = newFakeReg()
	s.regs["S-1-5-21-1000"].strings["AutoConfigURL"] = "http://corp/proxy.pac"
	s.fetched["http://corp/proxy.pac"] = `function FindProxyForURL(u,h){ return "DIRECT"; }`
	file := filepath.Join(t.TempDir(), "records.json")
	srv := recordingServer(s, file)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.http.Close() // the service never restored them
	s.regs["S-1-5-21-1001"].strings["AutoConfigURL"] = "http://someone-else/proxy.pac"

	unloaded := errors.New("the hive is not loaded")
	open := func(sid string) (Registry, error) {
		if sid == "S-1-5-21-1003" {
			return nil, unloaded
		}
		return s.open(sid)
	}
	restored, err := RestoreRecorded(file, open)
	if restored != 2 || !errors.Is(err, unloaded) {
		t.Fatalf("restored %d, err %v; want 2 restored and the unloaded hive reported", restored, err)
	}
	if got := s.regs["S-1-5-21-1000"].strings["AutoConfigURL"]; got != "http://corp/proxy.pac" {
		t.Fatalf("user 1000: AutoConfigURL = %q; want the original PAC", got)
	}
	if got := s.regs["S-1-5-21-1001"].strings["AutoConfigURL"]; got != "http://someone-else/proxy.pac" {
		t.Fatalf("user 1001: AutoConfigURL = %q; want the change made since left alone", got)
	}
	if _, ok := s.regs["S-1-5-21-1002"].strings["AutoConfigURL"]; ok {
		t.Fatal("user 1002 had no AutoConfigURL and still has one")
	}
	all, err := (&records{path: file}).read()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !strings.Contains(all["S-1-5-21-1003"].Applied, "proxy.pac") {
		t.Fatalf("records left = %+v; want only the user who could not be restored", all)
	}

	// No record file: nothing to do.
	if n, err := RestoreRecorded(filepath.Join(t.TempDir(), "none.json"), open); n != 0 || err != nil {
		t.Fatalf("with no records: %d, %v", n, err)
	}
}
