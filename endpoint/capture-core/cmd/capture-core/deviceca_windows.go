//go:build windows

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
	"unsafe"
)

// The device CA key's access control on Windows. %ProgramData% lets every user read what is created
// beneath it, so the key is created with its own protected DACL instead of an inherited one: full
// control for SYSTEM, Administrators and the account the agent runs as (SYSTEM, for the service),
// and nothing for anyone else. It is created that way, never created and then narrowed, so there is
// no moment at which it is readable.

var (
	kernel32ACL                                        = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree                                      = kernel32ACL.NewProc("LocalFree")
	procConvertStringSecurityDescriptorToSecurityDescW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procConvertSecurityDescriptorToStringSecurityDescW = advapi32.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	procGetNamedSecurityInfoW                          = advapi32.NewProc("GetNamedSecurityInfoW")
)

const (
	sddlRevision1         = 1
	seFileObject          = 1
	ownerSecurityInfo     = 0x00000001
	daclSecurityInfo      = 0x00000004
	sidLocalSystem        = "S-1-5-18"
	sidAdministrators     = "S-1-5-32-544"
	fileAttributeNormal   = 0x80
	createNew             = 1
	genericWrite          = 0x40000000
	protectedDACLTemplate = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)"
)

// selfSID is the SID of the account this process runs as.
func selfSID() (string, error) {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String()
}

// writeProtectedFile writes data to path with the protected DACL, through a new temporary file that
// is renamed into place: a same-volume rename keeps the file's own (non-inherited) DACL.
func writeProtectedFile(path string, data []byte) error {
	self, err := selfSID()
	if err != nil {
		return err
	}
	return writeFileWithSDDL(path, data, fmt.Sprintf(protectedDACLTemplate, self))
}

// writeFileWithSDDL creates path's content under exactly the security descriptor sddlText.
func writeFileWithSDDL(path string, data []byte, sddlText string) error {
	sddl, err := syscall.UTF16PtrFromString(sddlText)
	if err != nil {
		return err
	}
	var sd uintptr
	if r, _, err := procConvertStringSecurityDescriptorToSecurityDescW.Call(uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return fmt.Errorf("building the key's security descriptor: %w", err)
	}
	defer procLocalFree.Call(sd)

	tmp, err := tempSibling(path)
	if err != nil {
		return err
	}
	name, err := syscall.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	sa := &syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := syscall.CreateFile(name, genericWrite, 0, sa, createNew, fileAttributeNormal, 0)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	f := os.NewFile(uintptr(h), tmp)
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fileSDDL returns a file's owner and DACL in SDDL form.
func fileSDDL(path string) (string, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var owner, dacl, sd uintptr
	if r, _, _ := procGetNamedSecurityInfoW.Call(uintptr(unsafe.Pointer(name)), seFileObject, ownerSecurityInfo|daclSecurityInfo,
		uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd))); r != 0 {
		return "", fmt.Errorf("reading the security of %s: %w", path, syscall.Errno(r))
	}
	defer procLocalFree.Call(sd)
	var str *uint16
	if r, _, err := procConvertSecurityDescriptorToStringSecurityDescW.Call(sd, sddlRevision1, ownerSecurityInfo|daclSecurityInfo,
		uintptr(unsafe.Pointer(&str)), 0); r == 0 {
		return "", fmt.Errorf("rendering the security of %s: %w", path, err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(str)))
	return utf16PtrToString(str), nil
}

var (
	aceTrustee = regexp.MustCompile(`\(([^;)]*);[^;)]*;[^;)]*;[^;)]*;[^;)]*;([^)]*)\)`)
	sidPrefix  = regexp.MustCompile(`^S-1(-[0-9]+)+`)
)

// checkProtectedFile accepts the key file only when its owner and every account its DACL allows
// anything are SYSTEM, Administrators or this process's own account. Anything else was not written
// by writeProtectedFile, and a key another account can read, or could have planted, is not trusted.
func checkProtectedFile(path string) error {
	sddl, err := fileSDDL(path)
	if err != nil {
		return err
	}
	self, err := selfSID()
	if err != nil {
		return err
	}
	return checkSDDL(sddl, self)
}

// checkSDDL is the rule checkProtectedFile applies, on the SDDL text, so a test can state cases.
func checkSDDL(sddl, self string) error {
	allowed := func(sid string) bool {
		switch strings.ToUpper(sid) {
		case "SY", "BA", sidLocalSystem, sidAdministrators, strings.ToUpper(self):
			return true
		}
		return false
	}
	// The owner is a SID string ("O:S-1-5-21-…") or a two-letter alias ("O:BA"), followed directly
	// by the next part ("G:" or "D:").
	owner := ""
	if i := strings.Index(sddl, "O:"); i >= 0 {
		rest := sddl[i+2:]
		if owner = sidPrefix.FindString(rest); owner == "" && len(rest) >= 2 {
			owner = rest[:2]
		}
	}
	if !allowed(owner) {
		return fmt.Errorf("%w (owner %q)", errNotProtected, owner)
	}
	d := strings.Index(sddl, "D:")
	if d < 0 || strings.HasPrefix(sddl[d:], "D:NO_ACCESS_CONTROL") {
		return fmt.Errorf("%w (no DACL)", errNotProtected)
	}
	for _, m := range aceTrustee.FindAllStringSubmatch(sddl[d:], -1) {
		if strings.HasPrefix(m[1], "A") && !allowed(m[2]) {
			return fmt.Errorf("%w (allows %s)", errNotProtected, m[2])
		}
	}
	return nil
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var u []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return syscall.UTF16ToString(u)
}
