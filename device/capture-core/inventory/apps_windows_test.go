//go:build windows

package inventory

import (
	"strings"
	"testing"
)

// The machine's own registry reads, read only: the machine's uninstall entries (Windows itself
// registers some), the loaded people's hives, and both package repositories, which may be empty.
func TestSystemRegistryReads(t *testing.T) {
	reg := SystemRegistry()
	entries, err := reg.Uninstall("")
	if err != nil {
		t.Fatalf("machine uninstall entries: %v", err)
	}
	named := 0
	for _, e := range entries {
		if strings.TrimSpace(e.DisplayName) != "" {
			named++
		}
	}
	if named == 0 {
		t.Fatalf("no named machine uninstall entry among %d", len(entries))
	}
	users, err := reg.Users()
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	for _, u := range users {
		if !userSID(u.SID) {
			t.Errorf("%s is not a person's hive", u.SID)
		}
		// Another person's hive may be closed to an unelevated test run; the service runs as
		// LocalSystem.
		if _, err := reg.Uninstall(u.SID); err != nil {
			t.Logf("%s uninstall entries: %v", u.SID, err)
		}
		pkgs, err := reg.Packages(u.SID)
		if err != nil {
			t.Logf("%s packages: %v", u.SID, err)
		}
		for _, p := range pkgs {
			if _, _, ok := parsePackageFullName(p); !ok {
				t.Errorf("%s package %q is not a package full name", u.SID, p)
			}
		}
	}
	if _, err := reg.Packages(""); err != nil {
		t.Fatalf("machine packages: %v", err)
	}
}
