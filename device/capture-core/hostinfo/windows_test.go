//go:build windows

package hostinfo

import (
	"errors"
	"testing"
)

// The real readers against the machine running the test. What the machine states differs from
// host to host, so this asserts only that every reader runs to an answer without failing and that
// what it returns has the right shape; the selection rules are pinned by the portable tests.
func TestSystemSourcesReadThisMachine(t *testing.T) {
	f := Collect(SystemSources())
	for _, n := range f.Notes {
		t.Logf("note: %s", n)
	}
	t.Logf("attestation=%+v uuid=%q machine=%q seed=%q", f.Attestation, f.SystemUUID, f.MachineID, f.HardwareSeed())
	if f.MachineID == "" {
		t.Error("MachineGuid was not read; every Windows install has one")
	}
	if f.HardwareSeed() == "" {
		t.Error("no hardware seed on a Windows machine")
	}
	if id := f.Attestation.EntraDeviceID; id != "" && !guidPattern.MatchString(id) {
		t.Errorf("Entra device id %q is not a GUID", id)
	}
}

func TestSystemUserSourcesNameARealUser(t *testing.T) {
	u, err := NewResolver(SystemUserSources(), nil).Current()
	if errors.Is(err, ErrNoConsoleUser) {
		t.Skip("no interactive user on this host")
	}
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	t.Logf("user=%+v", u)
	if u.SID == "" || u.Account == "" {
		t.Fatalf("user = %+v, want a SID and an account", u)
	}
}
