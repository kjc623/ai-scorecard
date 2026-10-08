//go:build windows

package hostinfo

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modiphlpapi.NewProc("GetExtendedTcpTable")

	modwintrust                              = windows.NewLazySystemDLL("wintrust.dll")
	procWTHelperProvDataFromStateData        = modwintrust.NewProc("WTHelperProvDataFromStateData")
	procWTHelperGetProvSignerFromChain       = modwintrust.NewProc("WTHelperGetProvSignerFromChain")
	procWTHelperGetProvCertFromChain         = modwintrust.NewProc("WTHelperGetProvCertFromChain")
	procCryptCATAdminAcquireContext2         = modwintrust.NewProc("CryptCATAdminAcquireContext2")
	procCryptCATAdminReleaseContext          = modwintrust.NewProc("CryptCATAdminReleaseContext")
	procCryptCATAdminCalcHashFromFileHandle2 = modwintrust.NewProc("CryptCATAdminCalcHashFromFileHandle2")
	procCryptCATAdminEnumCatalogFromHash     = modwintrust.NewProc("CryptCATAdminEnumCatalogFromHash")
	procCryptCATAdminReleaseCatalogContext   = modwintrust.NewProc("CryptCATAdminReleaseCatalogContext")
	procCryptCATCatalogInfoFromContext       = modwintrust.NewProc("CryptCATCatalogInfoFromContext")
)

// tcpTableOwnerPIDAll is TCP_TABLE_OWNER_PID_ALL: every connection and listener, with its owner.
const tcpTableOwnerPIDAll = 5

// OwnerOfLocalTCP returns the PID of the process that opened a loopback TCP connection, given the
// server side's view of it: local is the server's endpoint and remote the client's. Both ends are on
// this host, so the TCP table holds a row for each; the answer is the row whose local endpoint is the
// client's.
func OwnerOfLocalTCP(local, remote netip.AddrPort) (uint32, error) {
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	family, parse := uint32(windows.AF_INET6), parseTCP6Table
	if local.Addr().Is4() && remote.Addr().Is4() {
		family, parse = windows.AF_INET, parseTCP4Table
	}
	buf, err := tcpTable(family)
	if err != nil {
		return 0, err
	}
	rows, err := parse(buf)
	if err != nil {
		return 0, err
	}
	if pid, ok := clientRow(rows, local, remote); ok {
		return pid, nil
	}
	// A dual-stack socket carries an IPv4 connection in the IPv6 table, as a v4-mapped address.
	if family == windows.AF_INET {
		mapped := func(a netip.AddrPort) netip.AddrPort {
			return netip.AddrPortFrom(netip.AddrFrom16(a.Addr().As16()), a.Port())
		}
		if buf, err := tcpTable(windows.AF_INET6); err == nil {
			if rows, err := parseTCP6Table(buf); err == nil {
				if pid, ok := clientRow(rows, mapped(local), mapped(remote)); ok {
					return pid, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("hostinfo: no TCP connection from %s to %s: %w", remote, local, ErrNotFound)
}

// tcpTable reads GetExtendedTcpTable for one address family. The table can grow between the call
// that sizes the buffer and the call that fills it, so a short buffer is retried.
func tcpTable(family uint32) ([]byte, error) {
	size := uint32(16 << 10)
	for range 4 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, uintptr(family), tcpTableOwnerPIDAll, 0)
		switch errno := syscall.Errno(r); {
		case r == 0:
			return buf, nil
		case errors.Is(errno, windows.ERROR_INSUFFICIENT_BUFFER):
			size += size / 8
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable: %w", errno)
		}
	}
	return nil, errors.New("hostinfo: the TCP table kept growing")
}

var processes = newProcessCache(processCacheTTL, processCacheMax)

// ProcessInfo describes a running process. The answer is cached by (PID, creation time), so a
// reused PID is never answered for its previous process. An unreadable token leaves User nil and
// an unsigned image an empty Publisher; neither is an error.
func ProcessInfo(pid uint32) (Process, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return Process{}, fmt.Errorf("opening process %d: %w", pid, err)
	}
	// While this handle is open, Windows does not reuse the PID, so every lookup below by PID
	// answers for this process.
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return Process{}, fmt.Errorf("GetProcessTimes of process %d: %w", pid, err)
	}
	started := time.Unix(0, created.Nanoseconds())
	now := time.Now()
	if p, ok := processes.get(pid, started, now); ok {
		return p, nil
	}
	image, err := processImage(h)
	if err != nil {
		return Process{}, fmt.Errorf("image of process %d: %w", pid, err)
	}
	p := Process{PID: pid, Image: image, Started: started, Publisher: publisher(image)}
	if u, err := UserOfProcess(pid); err == nil {
		p.User = &u
	}
	processes.put(p, now)
	return p, nil
}

// processImage is the process's executable as a Win32 path (QueryFullProcessImageNameW).
func processImage(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// publisher is the common name of the image's verified signer: its embedded Authenticode signature
// or, for an image Windows signs through a catalog (most of System32), the catalog's signer.
// Revocation is not checked and nothing is fetched from the network.
func publisher(image string) string {
	path, err := windows.UTF16PtrFromString(image)
	if err != nil {
		return ""
	}
	file := windows.WinTrustFileInfo{Size: uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})), FilePath: path}
	cn, err := verifiedSigner(windows.WTD_CHOICE_FILE, unsafe.Pointer(&file))
	switch {
	case err == nil:
		return cn
	case errors.Is(err, syscall.Errno(windows.TRUST_E_NOSIGNATURE)):
		return catalogSigner(image)
	default:
		return ""
	}
}

// verifiedSigner runs WinVerifyTrust over one subject (a file or a catalog member) and, when the
// signature verifies, names the signer from the verification's provider data.
func verifiedSigner(choice uint32, subject unsafe.Pointer) (string, error) {
	data := windows.WinTrustData{
		Size:                            uint32(unsafe.Sizeof(windows.WinTrustData{})),
		UIChoice:                        windows.WTD_UI_NONE,
		RevocationChecks:                windows.WTD_REVOKE_NONE,
		UnionChoice:                     choice,
		FileOrCatalogOrBlobOrSgnrOrCert: subject,
		StateAction:                     windows.WTD_STATEACTION_VERIFY,
		ProvFlags:                       windows.WTD_REVOCATION_CHECK_NONE | windows.WTD_CACHE_ONLY_URL_RETRIEVAL,
	}
	err := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, &data)
	// The state a verification opens is closed whatever the verdict.
	defer func() {
		data.StateAction = windows.WTD_STATEACTION_CLOSE
		_ = windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, &data)
	}()
	if err != nil {
		return "", err
	}
	return signerName(data.StateData)
}

// cryptProviderCert is the head of CRYPT_PROVIDER_CERT: its size, then the certificate.
type cryptProviderCert struct {
	size uint32
	cert *windows.CertContext
}

// signerName reads the first signer's certificate out of a verification's provider data
// (WTHelperProvDataFromStateData, then the signer and its chain's first certificate) and returns
// its subject's common name.
func signerName(state windows.Handle) (string, error) {
	prov, _, _ := procWTHelperProvDataFromStateData.Call(uintptr(state))
	if prov == 0 {
		return "", errors.New("hostinfo: the verification has no provider data")
	}
	sgnr, _, _ := procWTHelperGetProvSignerFromChain.Call(prov, 0, 0, 0)
	if sgnr == 0 {
		return "", errors.New("hostinfo: the signature has no signer")
	}
	pc, _, _ := procWTHelperGetProvCertFromChain.Call(sgnr, 0)
	if pc == 0 {
		return "", errors.New("hostinfo: the signer has no certificate")
	}
	// pc points into memory wintrust owns, not the Go heap; reading the address through the
	// variable's storage keeps it a pointer without a uintptr conversion.
	cert := (*(**cryptProviderCert)(unsafe.Pointer(&pc))).cert
	if cert == nil {
		return "", errors.New("hostinfo: the signer has no certificate")
	}
	return commonName(cert), nil
}

// oidCommonName is szOID_COMMON_NAME as the ANSI string CertGetNameString takes.
var oidCommonName = []byte("2.5.4.3\x00")

func commonName(cert *windows.CertContext) string {
	oid := unsafe.Pointer(&oidCommonName[0])
	n := windows.CertGetNameString(cert, windows.CERT_NAME_ATTR_TYPE, 0, oid, nil, 0)
	if n <= 1 {
		return ""
	}
	buf := make([]uint16, n)
	windows.CertGetNameString(cert, windows.CERT_NAME_ATTR_TYPE, 0, oid, &buf[0], n)
	return windows.UTF16ToString(buf)
}

// catalogInfo is CATALOG_INFO.
type catalogInfo struct {
	size        uint32
	catalogFile [windows.MAX_PATH]uint16
}

// wintrustCatalogInfo is WINTRUST_CATALOG_INFO.
type wintrustCatalogInfo struct {
	size                   uint32
	catalogVersion         uint32
	catalogFilePath        *uint16
	memberTag              *uint16
	memberFilePath         *uint16
	memberFile             windows.Handle
	calculatedFileHash     *byte
	calculatedFileHashSize uint32
	catalogContext         uintptr
	catAdmin               windows.Handle
}

// driverActionVerify is DRIVER_ACTION_VERIFY, the catalog subsystem that holds the operating
// system's components.
var driverActionVerify = windows.GUID{
	Data1: 0xf750e6c3, Data2: 0x38ee, Data3: 0x11d1,
	Data4: [8]byte{0x85, 0xe5, 0x00, 0xc0, 0x4f, 0xc2, 0x95, 0xee},
}

// catalogSigner finds the system catalog that lists the image's hash and verifies the image as a
// member of it. Catalogs are indexed by SHA-256 and, for older ones, SHA-1.
func catalogSigner(image string) string {
	f, err := os.Open(image)
	if err != nil {
		return ""
	}
	defer f.Close()
	for _, alg := range []string{"SHA256", "SHA1"} {
		if cn, ok := catalogSignerWith(alg, image, windows.Handle(f.Fd())); ok {
			return cn
		}
	}
	return ""
}

func catalogSignerWith(alg, image string, file windows.Handle) (string, bool) {
	algName, err := windows.UTF16PtrFromString(alg)
	if err != nil {
		return "", false
	}
	path, err := windows.UTF16PtrFromString(image)
	if err != nil {
		return "", false
	}
	var admin windows.Handle
	if r, _, _ := procCryptCATAdminAcquireContext2.Call(uintptr(unsafe.Pointer(&admin)), uintptr(unsafe.Pointer(&driverActionVerify)),
		uintptr(unsafe.Pointer(algName)), 0, 0); r == 0 {
		return "", false
	}
	defer procCryptCATAdminReleaseContext.Call(uintptr(admin), 0)

	// The first call, with no buffer, sizes the hash.
	var size uint32
	_, _, _ = procCryptCATAdminCalcHashFromFileHandle2.Call(uintptr(admin), uintptr(file), uintptr(unsafe.Pointer(&size)), 0, 0)
	if size == 0 {
		return "", false
	}
	hash := make([]byte, size)
	if r, _, _ := procCryptCATAdminCalcHashFromFileHandle2.Call(uintptr(admin), uintptr(file),
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&hash[0])), 0); r == 0 || size == 0 || int(size) > len(hash) {
		return "", false
	}
	hash = hash[:size]
	cat, _, _ := procCryptCATAdminEnumCatalogFromHash.Call(uintptr(admin), uintptr(unsafe.Pointer(&hash[0])),
		uintptr(size), 0, 0)
	if cat == 0 {
		return "", false
	}
	defer procCryptCATAdminReleaseCatalogContext.Call(uintptr(admin), cat, 0)
	info := catalogInfo{size: uint32(unsafe.Sizeof(catalogInfo{}))}
	if r, _, _ := procCryptCATCatalogInfoFromContext.Call(cat, uintptr(unsafe.Pointer(&info)), 0); r == 0 {
		return "", false
	}
	// A catalog names its members by the upper-case hex of their hash.
	tag, err := windows.UTF16PtrFromString(strings.ToUpper(hex.EncodeToString(hash)))
	if err != nil {
		return "", false
	}
	member := wintrustCatalogInfo{
		size:                   uint32(unsafe.Sizeof(wintrustCatalogInfo{})),
		catalogFilePath:        &info.catalogFile[0],
		memberTag:              tag,
		memberFilePath:         path,
		memberFile:             file,
		calculatedFileHash:     &hash[0],
		calculatedFileHashSize: size,
		catAdmin:               admin,
	}
	cn, err := verifiedSigner(windows.WTD_CHOICE_CATALOG, unsafe.Pointer(&member))
	return cn, err == nil
}
