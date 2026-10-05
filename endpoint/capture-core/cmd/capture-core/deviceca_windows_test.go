//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The key file's DACL as Windows reports it: protected (nothing inherited from %ProgramData%, which
// lets every user read), and allowing only SYSTEM, Administrators and the agent's own account.
func TestDeviceCAKeyDACLAllowsOnlySystemAndAdministrators(t *testing.T) {
	dir := filepath.Join(t.TempDir(), deviceCADir)
	if _, _, _, err := ensureDeviceCA(dir, "DESKTOP-01", time.Now()); err != nil {
		t.Fatal(err)
	}
	sddl, err := fileSDDL(filepath.Join(dir, deviceCAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("key file SDDL: %s", sddl)
	if !strings.Contains(sddl, "D:P") {
		t.Errorf("the DACL is not protected; it would inherit from its directory: %s", sddl)
	}
	for _, want := range []string{"(A;;FA;;;SY)", "(A;;FA;;;BA)"} {
		if !strings.Contains(sddl, want) {
			t.Errorf("SDDL %s lacks %s", sddl, want)
		}
	}
	for _, other := range []string{";BU)", ";AU)", ";WD)", ";IU)", ";S-1-5-32-545)"} {
		if strings.Contains(sddl, other) {
			t.Errorf("SDDL %s allows %s", sddl, strings.Trim(other, ";)"))
		}
	}
}

// A key file anyone can read is not trusted, however it got there: it is replaced.
func TestDeviceCAReplacesAKeyUsersCanRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), deviceCADir)
	now := time.Now()
	cert, key, _, err := ensureDeviceCA(dir, "DESKTOP-01", now)
	if err != nil {
		t.Fatal(err)
	}
	self, err := selfSID()
	if err != nil {
		t.Fatal(err)
	}
	readable := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + self + ")(A;;FR;;;BU)"
	if err := writeFileWithSDDL(filepath.Join(dir, deviceCAKeyFile), key, readable); err != nil {
		t.Fatal(err)
	}
	cert2, _, created, err := ensureDeviceCA(dir, "DESKTOP-01", now)
	if err != nil || !created || string(cert2) == string(cert) {
		t.Fatalf("a Users-readable key: created=%v err=%v; want a new pair", created, err)
	}
	if err := checkProtectedFile(filepath.Join(dir, deviceCAKeyFile)); err != nil {
		t.Fatalf("the replacement key is not protected: %v", err)
	}
}

func TestCheckSDDL(t *testing.T) {
	self := "S-1-5-21-1-2-3-1001"
	for _, ok := range []string{
		"O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)",
		"O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-1-2-3-1001)",
		"O:S-1-5-21-1-2-3-1001D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-1-2-3-1001)",
		"O:SYG:SYD:P(D;;FA;;;WD)(A;;FA;;;SY)",
	} {
		if err := checkSDDL(ok, self); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"O:BAD:P(A;;FA;;;SY)(A;;FR;;;BU)",
		"O:S-1-5-21-9-9-9-1002D:P(A;;FA;;;SY)",
		"O:SYD:NO_ACCESS_CONTROL",
		"O:SY",
		"O:SYD:AI(A;OICIID;FA;;;SY)(A;OICIID;0x1200a9;;;BU)",
	} {
		if err := checkSDDL(bad, self); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
