package tlsproxy

import (
	"crypto"
	"fmt"
	"unsafe"

	"github.com/google/certtostore"
	"golang.org/x/sys/windows"
)

// deviceRootKeyName is the CNG machine key that holds the device root's private key.
const deviceRootKeyName = "ShadowAICapture-DeviceRoot"

// The NCRYPT_EXPORT_POLICY_PROPERTY flags. A device root's key carries none of them.
const (
	ncryptAllowExportFlag             = 0x1 // NCRYPT_ALLOW_EXPORT_FLAG
	ncryptAllowPlaintextExportFlag    = 0x2 // NCRYPT_ALLOW_PLAINTEXT_EXPORT_FLAG
	ncryptAllowArchivingFlag          = 0x4 // NCRYPT_ALLOW_ARCHIVING_FLAG
	ncryptAllowPlaintextArchivingFlag = 0x8 // NCRYPT_ALLOW_PLAINTEXT_ARCHIVING_FLAG
	ncryptExportFlags                 = ncryptAllowExportFlag | ncryptAllowPlaintextExportFlag | ncryptAllowArchivingFlag | ncryptAllowPlaintextArchivingFlag
	ncryptExportPolicyProperty        = "Export Policy" // NCRYPT_EXPORT_POLICY_PROPERTY
)

var (
	ncryptDLL                     = windows.NewLazySystemDLL("ncrypt.dll")
	procNCryptGetProperty         = ncryptDLL.NewProc("NCryptGetProperty")
	procNCryptOpenStorageProvider = ncryptDLL.NewProc("NCryptOpenStorageProvider")
	procNCryptOpenKey             = ncryptDLL.NewProc("NCryptOpenKey")
	procNCryptDeleteKey           = ncryptDLL.NewProc("NCryptDeleteKey")
	procNCryptFreeObject          = ncryptDLL.NewProc("NCryptFreeObject")
)

const (
	ncryptMachineKeyFlag = 0x20       // NCRYPT_MACHINE_KEY_FLAG
	nteBadKeyset         = 0x80090016 // NTE_BAD_KEYSET: NCryptOpenKey found no key of that name
)

// platformKeyStore keeps the device root's key in CNG.
func platformKeyStore(string) (caKeyStore, error) {
	keys, err := openCNGKeyStore(deviceRootKeyName)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// cngKeyStore keeps the key as a P-256 machine key in the Microsoft Software Key Storage Provider.
// The key is created without an export policy, which the provider treats as not exportable, and a
// key whose policy allows export is refused: it is not this device's secret.
type cngKeyStore struct {
	store *certtostore.WinCertStore
}

// openCNGKeyStore opens the provider for the named machine key. The provider stays open for the
// life of the process, because the signer's key handle belongs to it.
func openCNGKeyStore(name string) (*cngKeyStore, error) {
	store, err := certtostore.OpenWinCertStoreWithOptions(certtostore.WinCertStoreOptions{
		Provider:  certtostore.ProviderMSSoftware,
		Container: name,
	})
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: opening the CNG key storage provider: %w", err)
	}
	return &cngKeyStore{store: store}, nil
}

func (s *cngKeyStore) Key() (crypto.Signer, error) {
	key, err := s.store.Key()
	if err != nil {
		return nil, err
	}
	if err := checkNotExportable(key); err != nil {
		return nil, err
	}
	return key, nil
}

// Generate overwrites the named key with a new one.
func (s *cngKeyStore) Generate() (crypto.Signer, error) {
	key, err := s.store.Generate(certtostore.GenerateOpts{Algorithm: certtostore.EC, Size: 256})
	if err != nil {
		return nil, err
	}
	if err := checkNotExportable(key); err != nil {
		return nil, err
	}
	return key, nil
}

// deletePlatformKey deletes the device root's CNG machine key.
func deletePlatformKey(string) error { return deleteCNGKey(deviceRootKeyName) }

// deleteCNGKey deletes the named machine key from the Microsoft Software Key Storage Provider. A
// key that does not exist is already deleted.
func deleteCNGKey(name string) error {
	provName, err := windows.UTF16PtrFromString(certtostore.ProviderMSSoftware)
	if err != nil {
		return err
	}
	keyName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var prov, key uintptr
	if r, _, _ := procNCryptOpenStorageProvider.Call(uintptr(unsafe.Pointer(&prov)), uintptr(unsafe.Pointer(provName)), 0); r != 0 {
		return fmt.Errorf("tlsproxy: opening the CNG key storage provider: 0x%08X", uint32(r))
	}
	defer procNCryptFreeObject.Call(prov)
	r, _, _ := procNCryptOpenKey.Call(prov, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(keyName)), 0, ncryptMachineKeyFlag)
	if uint32(r) == nteBadKeyset {
		return nil
	}
	if r != 0 {
		return fmt.Errorf("tlsproxy: opening the CNG key %s: 0x%08X", name, uint32(r))
	}
	// NCryptDeleteKey frees the handle when it succeeds, and only then.
	if r, _, _ := procNCryptDeleteKey.Call(key, 0); r != 0 {
		procNCryptFreeObject.Call(key)
		return fmt.Errorf("tlsproxy: deleting the CNG key %s: 0x%08X", name, uint32(r))
	}
	return nil
}

// cngHandle is a certtostore key's NCRYPT_KEY_HANDLE.
func cngHandle(key crypto.Signer) (uintptr, error) {
	h, ok := key.(interface{ TransientTpmHandle() uintptr })
	if !ok {
		return 0, fmt.Errorf("tlsproxy: %T is not a CNG key", key)
	}
	return h.TransientTpmHandle(), nil
}

func checkNotExportable(key crypto.Signer) error {
	h, err := cngHandle(key)
	if err != nil {
		return err
	}
	policy, err := exportPolicy(h)
	if err != nil {
		return err
	}
	if policy&ncryptExportFlags != 0 {
		return fmt.Errorf("tlsproxy: the CNG key's export policy is 0x%x; the device root's key must not be exportable", policy)
	}
	return nil
}

// exportPolicy reads the key's NCRYPT_EXPORT_POLICY_PROPERTY.
func exportPolicy(h uintptr) (uint32, error) {
	name, err := windows.UTF16PtrFromString(ncryptExportPolicyProperty)
	if err != nil {
		return 0, err
	}
	var policy, n uint32
	r, _, _ := procNCryptGetProperty.Call(h, uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(&policy)), unsafe.Sizeof(policy), uintptr(unsafe.Pointer(&n)), 0)
	if r != 0 {
		return 0, fmt.Errorf("tlsproxy: reading the CNG key's export policy: 0x%08X", uint32(r))
	}
	return policy, nil
}
