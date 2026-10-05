//go:build windows

package hostinfo

import (
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// The Windows readers use the standard library's syscall package and lazily loaded system DLLs,
// not golang.org/x/sys/windows, for the reason cmd/capture-core/service_windows.go gives: this
// module builds offline with no module beyond the repository's own.

const (
	keyWow6464Key = 0x0100

	certStoreProvSystemW               = 10
	certSystemStoreLocalMachine        = 0x00020000
	certStoreReadonlyFlag              = 0x00008000
	certStoreOpenExistingFlag          = 0x00004000
	firmwareTableProviderRSMB          = 0x52534D42 // 'RSMB'
	errorNoMoreItems                   = syscall.Errno(259)
	errorNoToken                       = syscall.Errno(1008)
	errorPrivilegeNotHeld              = syscall.Errno(1314)
	errorMoreData                      = syscall.Errno(234)
	invalidConsoleSession       uint32 = 0xFFFFFFFF
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	wtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procGetSystemFirmwareTable       = kernel32.NewProc("GetSystemFirmwareTable")
	procWTSGetActiveConsoleSessionID = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	procWTSQueryUserToken            = wtsapi32.NewProc("WTSQueryUserToken")
	procImpersonateLoggedOnUser      = advapi32.NewProc("ImpersonateLoggedOnUser")
	procRevertToSelf                 = advapi32.NewProc("RevertToSelf")
)

// SystemSources reads the real machine.
func SystemSources() Sources {
	reg := SystemRegistry()
	return Sources{
		Registry:     reg,
		MachineCerts: machineCerts,
		SMBIOS:       firmwareSMBIOS,
		MachineID:    func() (string, error) { return MachineGUID(reg) },
	}
}

// SystemUserSources names the real console user.
func SystemUserSources() UserSources {
	return UserSources{Console: consoleUser, UPN: directoryUPN, Process: processUser}
}

// SystemRegistry is HKLM, read through the 64-bit view.
func SystemRegistry() Registry { return winRegistry{} }

type winRegistry struct{}

func openKey(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ|keyWow6464Key, &h); err != nil {
		return 0, regErr(err)
	}
	return h, nil
}

func regErr(err error) error {
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
		return ErrNotFound
	}
	return err
}

func (winRegistry) SubKeys(path string) ([]string, error) {
	h, err := openKey(path)
	if err != nil {
		return nil, err
	}
	defer syscall.RegCloseKey(h)
	var count, maxLen uint32
	if err := syscall.RegQueryInfoKey(h, nil, nil, nil, &count, &maxLen, nil, nil, nil, nil, nil, nil); err != nil {
		return nil, err
	}
	out := make([]string, 0, count)
	buf := make([]uint16, maxLen+1)
	for i := uint32(0); ; i++ {
		n := uint32(len(buf))
		err := syscall.RegEnumKeyEx(h, i, &buf[0], &n, nil, nil, nil, nil)
		if errors.Is(err, errorNoMoreItems) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, syscall.UTF16ToString(buf[:n]))
	}
}

func (winRegistry) value(path, name string) (uint32, []byte, error) {
	h, err := openKey(path)
	if err != nil {
		return 0, nil, err
	}
	defer syscall.RegCloseKey(h)
	pn, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, nil, err
	}
	var typ, size uint32
	if err := syscall.RegQueryValueEx(h, pn, nil, &typ, nil, &size); err != nil {
		return 0, nil, regErr(err)
	}
	if size == 0 {
		return typ, nil, nil
	}
	buf := make([]byte, size)
	if err := syscall.RegQueryValueEx(h, pn, nil, &typ, &buf[0], &size); err != nil {
		return 0, nil, regErr(err)
	}
	return typ, buf[:size], nil
}

func (r winRegistry) String(path, name string) (string, error) {
	typ, b, err := r.value(path, name)
	if err != nil {
		return "", err
	}
	if typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ {
		return "", fmt.Errorf("hostinfo: %s\\%s is registry type %d, not a string", path, name, typ)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return syscall.UTF16ToString(u), nil
}

func (r winRegistry) Integer(path, name string) (uint64, error) {
	typ, b, err := r.value(path, name)
	if err != nil {
		return 0, err
	}
	switch {
	case typ == syscall.REG_DWORD && len(b) >= 4:
		return uint64(binary.LittleEndian.Uint32(b)), nil
	case typ == syscall.REG_QWORD && len(b) >= 8:
		return binary.LittleEndian.Uint64(b), nil
	default:
		return 0, fmt.Errorf("hostinfo: %s\\%s is registry type %d, not an integer", path, name, typ)
	}
}

// machineCerts lists LocalMachine\My read-only. Only the public certificates are read.
func machineCerts() ([]*x509.Certificate, error) {
	name, err := syscall.UTF16PtrFromString("MY")
	if err != nil {
		return nil, err
	}
	store, err := syscall.CertOpenStore(certStoreProvSystemW, 0, 0,
		certSystemStoreLocalMachine|certStoreReadonlyFlag|certStoreOpenExistingFlag, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return nil, fmt.Errorf("opening LocalMachine\\My: %w", err)
	}
	defer syscall.CertCloseStore(store, 0)
	var out []*x509.Certificate
	var ctx *syscall.CertContext
	for {
		// Each call frees the context it is handed; the loop ends when the store is exhausted.
		ctx, _ = syscall.CertEnumCertificatesInStore(store, ctx)
		if ctx == nil {
			return out, nil
		}
		der := append([]byte(nil), unsafe.Slice(ctx.EncodedCert, ctx.Length)...)
		if c, err := x509.ParseCertificate(der); err == nil {
			out = append(out, c)
		}
	}
}

// firmwareSMBIOS returns the raw SMBIOS table (GetSystemFirmwareTable 'RSMB'), which any user may
// read: a RawSMBIOSData header of 8 bytes, then the structure table.
func firmwareSMBIOS() (RawSMBIOS, error) {
	n, _, err := procGetSystemFirmwareTable.Call(firmwareTableProviderRSMB, 0, 0, 0)
	if n == 0 {
		return RawSMBIOS{}, fmt.Errorf("GetSystemFirmwareTable: %w", err)
	}
	buf := make([]byte, n)
	m, _, err := procGetSystemFirmwareTable.Call(firmwareTableProviderRSMB, 0, uintptr(unsafe.Pointer(&buf[0])), n)
	if m == 0 || m > n {
		return RawSMBIOS{}, fmt.Errorf("GetSystemFirmwareTable: %w", err)
	}
	buf = buf[:m]
	if len(buf) < 8 {
		return RawSMBIOS{}, errors.New("GetSystemFirmwareTable returned no SMBIOS header")
	}
	table := buf[8:]
	if l := int(binary.LittleEndian.Uint32(buf[4:8])); l < len(table) {
		table = table[:l]
	}
	return RawSMBIOS{Major: buf[1], Minor: buf[2], Table: table}, nil
}

// consoleToken is the primary token of the user signed in at the physical console. Only a
// service (SE_TCB_NAME) may ask; anyone else is told ErrNotPermitted.
func consoleToken() (syscall.Token, error) {
	sess, _, _ := procWTSGetActiveConsoleSessionID.Call()
	if uint32(sess) == invalidConsoleSession {
		return 0, ErrNoConsoleUser
	}
	var tok syscall.Handle
	if r, _, err := procWTSQueryUserToken.Call(sess, uintptr(unsafe.Pointer(&tok))); r == 0 {
		switch {
		case errors.Is(err, errorNoToken):
			return 0, ErrNoConsoleUser
		case errors.Is(err, errorPrivilegeNotHeld), errors.Is(err, syscall.ERROR_ACCESS_DENIED):
			return 0, ErrNotPermitted
		default:
			return 0, fmt.Errorf("WTSQueryUserToken: %w", err)
		}
	}
	return syscall.Token(tok), nil
}

func consoleUser() (User, error) {
	tok, err := consoleToken()
	if err != nil {
		return User{}, err
	}
	defer tok.Close()
	return userFromToken(tok)
}

func processUser() (User, error) {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return User{}, err
	}
	defer tok.Close()
	return userFromToken(tok)
}

// userFromToken names a token's user with what is local and cheap: the SID, DOMAIN\user, and the
// UPN Windows cached at sign-in.
func userFromToken(tok syscall.Token) (User, error) {
	tu, err := tok.GetTokenUser()
	if err != nil {
		return User{}, err
	}
	sid, err := tu.User.Sid.String()
	if err != nil {
		return User{}, err
	}
	u := User{SID: sid}
	if account, domain, _, err := tu.User.Sid.LookupAccount(""); err == nil {
		u.Account = account
		if domain != "" {
			u.Account = domain + `\` + account
		}
	}
	u.UPN = UPNFromIdentityStore(winRegistry{}, sid)
	return u, nil
}

// directoryUPN asks the directory for the user's principal name. For the console user it runs
// while impersonating them, because GetUserNameEx answers for the calling thread's user; for this
// process's own user it needs no impersonation.
func directoryUPN(u User) (string, error) {
	if u.Source == "process" {
		return upnOfCaller(u.Account)
	}
	tok, err := consoleToken()
	if err != nil {
		return "", err
	}
	defer tok.Close()
	// The console may have changed hands since u was read; answer only for the same account.
	if cur, err := userFromToken(tok); err != nil || cur.SID != u.SID {
		return "", errors.New("hostinfo: the console user changed during the lookup")
	}
	return impersonating(tok, func() (string, error) { return upnOfCaller(u.Account) })
}

// impersonating runs fn on a dedicated OS thread that carries tok's identity. The thread is never
// unlocked: a goroutine that exits while locked takes its thread with it, so a thread whose
// RevertToSelf failed can never run other Go code as the user.
func impersonating(tok syscall.Token, fn func() (string, error)) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		if r, _, err := procImpersonateLoggedOnUser.Call(uintptr(tok)); r == 0 {
			ch <- result{"", fmt.Errorf("ImpersonateLoggedOnUser: %w", err)}
			return
		}
		s, ferr := fn()
		if r, _, err := procRevertToSelf.Call(); r == 0 {
			ch <- result{"", fmt.Errorf("RevertToSelf: %w", err)}
			return
		}
		ch <- result{s, ferr}
	}()
	res := <-ch
	return res.s, res.err
}

// upnOfCaller is the calling thread's user principal name: GetUserNameEx, then TranslateName from
// the SAM-compatible account name. Both need a directory (a domain controller) for a domain
// account and answer nothing for a local one.
func upnOfCaller(account string) (string, error) {
	upn, err := nameCall(func(buf *uint16, n *uint32) error { return syscall.GetUserNameEx(syscall.NameUserPrincipal, buf, n) })
	if err == nil && upn != "" {
		return upn, nil
	}
	if account == "" {
		return "", err
	}
	acct, perr := syscall.UTF16PtrFromString(account)
	if perr != nil {
		return "", perr
	}
	return nameCall(func(buf *uint16, n *uint32) error {
		return syscall.TranslateName(acct, syscall.NameSamCompatible, syscall.NameUserPrincipal, buf, n)
	})
}

func nameCall(call func(*uint16, *uint32) error) (string, error) {
	n := uint32(256)
	for i := 0; i < 3; i++ {
		buf := make([]uint16, n)
		size := n
		err := call(&buf[0], &size)
		if err == nil {
			if size > uint32(len(buf)) {
				size = uint32(len(buf))
			}
			return string(utf16.Decode(trimNUL(buf[:size]))), nil
		}
		if !errors.Is(err, errorMoreData) || size <= n {
			return "", err
		}
		n = size
	}
	return "", errors.New("hostinfo: name buffer kept growing")
}

func trimNUL(u []uint16) []uint16 {
	for i, c := range u {
		if c == 0 {
			return u[:i]
		}
	}
	return u
}
