//go:build windows

package toolconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// managedSDDL is the access control of a managed file that had none worth keeping: full control for
// SYSTEM and Administrators, read for Users, nothing inherited.
const managedSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)"

// writeMask is every right that changes a file, its attributes, its access control or its owner.
const writeMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
	windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	windows.GENERIC_WRITE | windows.GENERIC_ALL

// administrators are the accounts that may hold write rights on a managed file: SYSTEM,
// Administrators, TrustedInstaller and CREATOR OWNER (which grants nothing on a file).
var administrators = map[string]bool{
	"S-1-5-18":     true,
	"S-1-5-32-544": true,
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464": true,
	"S-1-3-0": true,
}

// writeManaged writes data to a temporary file beside path, gives it the access control the managed
// file keeps, and renames it over path. A new folder under Program Files inherits read-only access
// for users.
func writeManaged(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dacl, info, err := keptDACL(path)
	if err != nil {
		return err
	}
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := windows.SetNamedSecurityInfo(tmp, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|info, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("setting the access control of %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// keptDACL is the access control a rewritten file gets: the current file's, with its protection,
// unless there is no file or its access control lets an account other than an administrator write
// it; then managedSDDL.
func keptDACL(path string) (*windows.ACL, windows.SECURITY_INFORMATION, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	switch {
	case err == nil:
		dacl, _, derr := sd.DACL()
		if derr == nil && dacl != nil && !usersMayWrite(dacl) {
			info := windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
			if ctl, _, cerr := sd.Control(); cerr == nil && ctl&windows.SE_DACL_PROTECTED != 0 {
				info = windows.PROTECTED_DACL_SECURITY_INFORMATION
			}
			return dacl, info, nil
		}
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
	default:
		return nil, 0, fmt.Errorf("reading the access control of %s: %w", path, err)
	}
	sd, err = windows.SecurityDescriptorFromString(managedSDDL)
	if err != nil {
		return nil, 0, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, 0, err
	}
	return dacl, windows.PROTECTED_DACL_SECURITY_INFORMATION, nil
}

// usersMayWrite reports whether an allow entry that applies to the file itself grants a write right
// to an account other than an administrator.
func usersMayWrite(dacl *windows.ACL) bool {
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return true
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Mask&writeMask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !administrators[sid.String()] {
			return true
		}
	}
	return false
}
