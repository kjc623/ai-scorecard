//go:build windows

package toolconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func daclOf(t *testing.T, path string) (*windows.SECURITY_DESCRIPTOR, *windows.ACL) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	return sd, dacl
}

func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// A new managed file is readable by Users and writable only by SYSTEM and Administrators, with
// nothing inherited.
func TestWriteManagedNewFileIsReadOnlyForUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ClaudeCode", "managed-settings.json")
	if err := writeManaged(path, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	sd, dacl := daclOf(t, path)
	sddl := sd.String()
	if !strings.Contains(sddl, "D:P") {
		t.Errorf("the DACL is not protected: %s", sddl)
	}
	if !strings.Contains(sddl, ";;;BU)") {
		t.Errorf("Users cannot read the file: %s", sddl)
	}
	if usersMayWrite(dacl) {
		t.Errorf("an account other than an administrator may write the file: %s", sddl)
	}
}

// An existing file's access control is kept when it lets no user write, and replaced when it does.
func TestWriteManagedKeepsOrReplacesTheFilesDACL(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept.json")
	open := filepath.Join(dir, "open.json")
	for _, p := range []string{kept, open} {
		if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setDACL(t, kept, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;AU)")
	setDACL(t, open, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;BU)")

	for _, p := range []string{kept, open} {
		if err := writeManaged(p, []byte(`{"env":{}}`)); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); string(got) != `{"env":{}}` {
			t.Fatalf("%s holds %q", p, got)
		}
	}
	if sd, _ := daclOf(t, kept); !strings.Contains(sd.String(), ";;;AU)") {
		t.Errorf("the file's own access control was not kept: %s", sd.String())
	}
	if sd, dacl := daclOf(t, open); usersMayWrite(dacl) {
		t.Errorf("a DACL that let Users write was kept: %s", sd.String())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".managed-*")); len(m) != 0 {
		t.Fatalf("temporary files left behind: %v", m)
	}
}
