//go:build windows

package hostinfo

import (
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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

func openKey(path string) (registry.Key, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.READ|registry.WOW64_64KEY)
	if err != nil {
		return 0, regErr(err)
	}
	return k, nil
}

func regErr(err error) error {
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return ErrNotFound
	}
	return err
}

func (winRegistry) SubKeys(path string) ([]string, error) {
	k, err := openKey(path)
	if err != nil {
		return nil, err
	}
	defer k.Close()
	return k.ReadSubKeyNames(-1)
}

func (winRegistry) String(path, name string) (string, error) {
	k, err := openKey(path)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return "", regErr(err)
	}
	return v, nil
}

func (winRegistry) Integer(path, name string) (uint64, error) {
	k, err := openKey(path)
	if err != nil {
		return 0, err
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name)
	if err != nil {
		return 0, regErr(err)
	}
	return v, nil
}

// machineCerts lists LocalMachine\My read-only. Only the public certificates are read.
func machineCerts() ([]*x509.Certificate, error) {
	name, err := windows.UTF16PtrFromString("MY")
	if err != nil {
		return nil, err
	}
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM_W, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_READONLY_FLAG|windows.CERT_STORE_OPEN_EXISTING_FLAG,
		uintptr(unsafe.Pointer(name)))
	if err != nil {
		return nil, fmt.Errorf("opening LocalMachine\\My: %w", err)
	}
	defer windows.CertCloseStore(store, 0)
	var out []*x509.Certificate
	var ctx *windows.CertContext
	for {
		// Each call frees the context it is handed; the loop ends when the store is exhausted.
		ctx, err = windows.CertEnumCertificatesInStore(store, ctx)
		if err != nil || ctx == nil {
			return out, nil
		}
		der := append([]byte(nil), unsafe.Slice(ctx.EncodedCert, ctx.Length)...)
		if c, err := x509.ParseCertificate(der); err == nil {
			out = append(out, c)
		}
	}
}

var procGetSystemFirmwareTable = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemFirmwareTable")

// firmwareTableProviderRSMB is the 'RSMB' provider signature of GetSystemFirmwareTable.
const firmwareTableProviderRSMB = 0x52534D42

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
func consoleToken() (windows.Token, error) {
	sess := windows.WTSGetActiveConsoleSessionId()
	if sess == 0xFFFFFFFF {
		return 0, ErrNoConsoleUser
	}
	var tok windows.Token
	if err := windows.WTSQueryUserToken(sess, &tok); err != nil {
		switch {
		case errors.Is(err, windows.ERROR_NO_TOKEN):
			return 0, ErrNoConsoleUser
		case errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD), errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return 0, ErrNotPermitted
		default:
			return 0, fmt.Errorf("WTSQueryUserToken: %w", err)
		}
	}
	return tok, nil
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
	return userFromToken(windows.GetCurrentProcessToken())
}

// UserOfProcess names the user a process runs as, from its token: the identity of a local peer
// such as the browser's native-messaging relay.
func UserOfProcess(pid uint32) (User, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return User{}, fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return User{}, fmt.Errorf("opening the token of process %d: %w", pid, err)
	}
	defer tok.Close()
	u, err := userFromToken(tok)
	if err != nil {
		return User{}, err
	}
	u.Source = "process"
	if u.ObjectID == "" {
		u.ObjectID, _ = ObjectIDFromSID(u.SID)
	}
	return u, nil
}

// userFromToken names a token's user with what is local and cheap: the SID, DOMAIN\user, and the
// UPN Windows cached at sign-in.
func userFromToken(tok windows.Token) (User, error) {
	tu, err := tok.GetTokenUser()
	if err != nil {
		return User{}, err
	}
	sid := tu.User.Sid.String()
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
func impersonating(tok windows.Token, fn func() (string, error)) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		var imp windows.Token
		if err := windows.DuplicateTokenEx(tok, windows.TOKEN_IMPERSONATE|windows.TOKEN_QUERY, nil,
			windows.SecurityImpersonation, windows.TokenImpersonation, &imp); err != nil {
			ch <- result{"", fmt.Errorf("duplicating the user token: %w", err)}
			return
		}
		defer imp.Close()
		if err := windows.SetThreadToken(nil, imp); err != nil {
			ch <- result{"", fmt.Errorf("impersonating the user: %w", err)}
			return
		}
		s, ferr := fn()
		if err := windows.RevertToSelf(); err != nil {
			ch <- result{"", fmt.Errorf("RevertToSelf: %w", err)}
			return
		}
		ch <- result{s, ferr}
	}()
	res := <-ch
	return res.s, res.err
}

// upnOfCaller is the calling thread's user principal name: GetUserNameEx, then TranslateName from
// the SAM-compatible account name. Both need a domain controller for a domain account and answer
// nothing for a local one.
func upnOfCaller(account string) (string, error) {
	upn, err := nameCall(func(buf *uint16, n *uint32) error {
		return windows.GetUserNameEx(windows.NameUserPrincipal, buf, n)
	})
	if err == nil && upn != "" {
		return upn, nil
	}
	if account == "" {
		return "", err
	}
	acct, perr := windows.UTF16PtrFromString(account)
	if perr != nil {
		return "", perr
	}
	return nameCall(func(buf *uint16, n *uint32) error {
		return windows.TranslateName(acct, windows.NameSamCompatible, windows.NameUserPrincipal, buf, n)
	})
}

func nameCall(call func(*uint16, *uint32) error) (string, error) {
	n := uint32(256)
	for i := 0; i < 3; i++ {
		buf := make([]uint16, n)
		size := n
		err := call(&buf[0], &size)
		if err == nil {
			return windows.UTF16ToString(buf), nil
		}
		if !errors.Is(err, windows.ERROR_MORE_DATA) || size <= n {
			return "", err
		}
		n = size
	}
	return "", errors.New("hostinfo: name buffer kept growing")
}
