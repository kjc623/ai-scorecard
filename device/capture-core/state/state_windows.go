//go:build windows

package state

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// %ProgramData% lets every user read what is created beneath it, so the state directory and its
// key files carry their own protected DACL instead of an inherited one: full control for SYSTEM,
// Administrators and the account the agent runs as (SYSTEM, for the service), nothing for anyone
// else. Files are created with that DACL, never created and then narrowed.

const (
	dirSDDL  = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)"
	fileSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)"
)

// selfSID is the SID of the account this process runs as.
func selfSID() (*windows.SID, error) {
	tok := windows.GetCurrentProcessToken()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return tu.User.Sid.Copy()
}

func descriptor(template string) (*windows.SECURITY_DESCRIPTOR, error) {
	self, err := selfSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf(template, self.String()))
}

// protectDir replaces the directory's DACL with the protected one; Windows propagates its
// inheritable entries to the files already inside. A directory owned by another account is given
// to this one.
func protectDir(path string) error {
	sd, err := descriptor(dirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("setting the directory's access control: %w", err)
	}
	if err := checkFile(path); err == nil {
		return nil
	}
	self, err := selfSID()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, self, nil, nil, nil); err != nil {
		return fmt.Errorf("taking ownership of the directory: %w", err)
	}
	return checkFile(path)
}

// createProtected creates path exclusively, under the protected DACL. An existing file is an
// error that satisfies errors.Is(err, fs.ErrExist).
func createProtected(path string) (*os.File, error) {
	sd, err := descriptor(fileSDDL)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func checkFile(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the security of %s: %w", path, err)
	}
	self, err := selfSID()
	if err != nil {
		return err
	}
	return checkSDDL(sd.String(), self.String())
}

var (
	aceTrustee = regexp.MustCompile(`\(([^;)]*);[^;)]*;[^;)]*;[^;)]*;[^;)]*;([^)]*)\)`)
	sidPrefix  = regexp.MustCompile(`^S-1(-[0-9]+)+`)
)

// resolveTrustee returns the SID string a security descriptor account names. Windows renders a
// well-known account as a two-letter alias ("LA" is the local Administrator), so an alias is
// resolved through a one-entry descriptor.
func resolveTrustee(name string) (string, bool) {
	if sidPrefix.MatchString(name) {
		return strings.ToUpper(name), true
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + name)
	if err != nil {
		return "", false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return "", false
	}
	return strings.ToUpper(owner.String()), true
}

// checkSDDL accepts a security descriptor only when its owner and every account an allow entry
// names are SYSTEM, Administrators or self.
func checkSDDL(sddl, self string) error {
	allowed := func(sid string) bool {
		switch strings.ToUpper(sid) {
		case "SY", "BA", "S-1-5-18", "S-1-5-32-544", strings.ToUpper(self):
			return true
		}
		if resolved, ok := resolveTrustee(sid); ok {
			switch resolved {
			case "S-1-5-18", "S-1-5-32-544", strings.ToUpper(self):
				return true
			}
		}
		return false
	}
	// The owner is a SID string ("O:S-1-5-21-...") or a two-letter alias ("O:BA"), followed
	// directly by the next part ("G:" or "D:").
	owner := ""
	if i := strings.Index(sddl, "O:"); i >= 0 {
		rest := sddl[i+2:]
		if owner = sidPrefix.FindString(rest); owner == "" && len(rest) >= 2 {
			owner = rest[:2]
		}
	}
	if !allowed(owner) {
		return fmt.Errorf("%w (owner %q)", ErrNotProtected, owner)
	}
	d := strings.Index(sddl, "D:")
	if d < 0 || strings.HasPrefix(sddl[d:], "D:NO_ACCESS_CONTROL") {
		return fmt.Errorf("%w (no DACL)", ErrNotProtected)
	}
	for _, m := range aceTrustee.FindAllStringSubmatch(sddl[d:], -1) {
		if strings.HasPrefix(m[1], "A") && !allowed(m[2]) {
			return fmt.Errorf("%w (allows %s)", ErrNotProtected, m[2])
		}
	}
	return nil
}
