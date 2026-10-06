//go:build windows

package state

import (
	"bytes"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The key file's DACL as Windows reports it: protected (nothing inherited from %ProgramData%,
// which lets every user read), and allowing only SYSTEM, Administrators and the agent's account.
func TestKeyFileDACLAllowsOnlySystemAndAdministrators(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Key(SpoolKeyFile); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(d.Path(SpoolKeyFile), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	if !strings.Contains(sddl, "D:P") {
		t.Errorf("the DACL is not protected: %s", sddl)
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

// A key file Users can read is re-written with the protected DACL, keeping its key.
func TestReadOrCreateKeyReprotectsAReadableKey(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, KeySize)
	self, err := selfSID()
	if err != nil {
		t.Fatal(err)
	}
	readable, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + self.String() + ")(A;;FR;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	path := d.Path(ContentKeyFile)
	name, _ := windows.UTF16PtrFromString(path)
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: readable}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	var n uint32
	if err := windows.WriteFile(h, key, &n, nil); err != nil {
		t.Fatal(err)
	}
	windows.CloseHandle(h)
	if err := CheckFile(path); err == nil {
		t.Fatal("precondition: a Users-readable file passed the check")
	}

	got, err := d.Key(ContentKeyFile)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("Key: err=%v, same key=%v", err, bytes.Equal(got, key))
	}
	if err := CheckFile(path); err != nil {
		t.Fatalf("the key file was not re-protected: %v", err)
	}
}

func TestCheckSDDL(t *testing.T) {
	self := "S-1-5-21-1-2-3-1001"
	for _, ok := range []string{
		"O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)",
		"O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-1-2-3-1001)",
		"O:S-1-5-21-1-2-3-1001D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-1-2-3-1001)",
		"O:SYG:SYD:P(D;;FA;;;WD)(A;;FA;;;SY)",
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
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
