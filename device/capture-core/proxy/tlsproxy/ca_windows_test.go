package tlsproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procNCryptExportKey = ncryptDLL.NewProc("NCryptExportKey")

const (
	nteBadKeyState  = 0x8009000B // NTE_BAD_KEY_STATE, what certutil reports for a key it may not export
	ntePerm         = 0x80090010 // NTE_PERM
	nteNotSupported = 0x80090029 // NTE_NOT_SUPPORTED
)

// The private-key blob types an export would use: BCRYPT_PRIVATE_KEY_BLOB, BCRYPT_ECCPRIVATE_BLOB
// and NCRYPT_PKCS8_PRIVATE_KEY_BLOB.
var privateBlobTypes = []string{"PRIVATEBLOB", "ECCPRIVATEBLOB", "PKCS8_PRIVATEKEY"}

// exportStatus is the status NCryptExportKey returns for the key's private half as blobType: the
// size query, or the export itself when the size query succeeds.
func exportStatus(h uintptr, blobType string) uint32 {
	bt, err := windows.UTF16PtrFromString(blobType)
	if err != nil {
		panic(err)
	}
	var size uint32
	r, _, _ := procNCryptExportKey.Call(h, 0, uintptr(unsafe.Pointer(bt)), 0, 0, 0, uintptr(unsafe.Pointer(&size)), 0)
	if r != 0 || size == 0 {
		return uint32(r)
	}
	buf := make([]byte, size)
	r, _, _ = procNCryptExportKey.Call(h, 0, uintptr(unsafe.Pointer(bt)), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&size)), 0)
	return uint32(r)
}

// assertNotExportable checks the key's export policy and that every private-key export fails.
func assertNotExportable(t *testing.T, ca *CA) {
	t.Helper()
	h, err := cngHandle(ca.key)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := exportPolicy(h)
	if err != nil {
		t.Fatal(err)
	}
	if policy&ncryptExportFlags != 0 {
		t.Errorf("export policy = 0x%x, want no export or archiving flag", policy)
	}
	for _, blob := range privateBlobTypes {
		switch st := exportStatus(h, blob); st {
		case nteNotSupported, ntePerm, nteBadKeyState:
		default:
			t.Errorf("NCryptExportKey(%s) = 0x%08X; want NTE_NOT_SUPPORTED, NTE_PERM or NTE_BAD_KEY_STATE", blob, st)
		}
	}
}

// The device root's key is a CNG machine key that refuses export, leaves signed through it verify,
// and a restart opens the same key by name. The test uses a key name of its own, so a device root
// on this machine is untouched.
func TestCNGDeviceRootKeyIsNotExportable(t *testing.T) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := deviceRootKeyName + "-test-" + hex.EncodeToString(b[:])
	keys, err := openCNGKeyStore(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deleteCNGKey(name); err != nil {
			t.Errorf("deleting the test key %s: %v", name, err)
		}
	})

	dir := filepath.Join(t.TempDir(), "device-ca")
	var log []string
	trust := newRecordingTrust(&log, filepath.Join(dir, deviceCAKeyFile))
	now := time.Now()
	ca, created, err := openDeviceCA(context.Background(), keys, dir, "DESKTOP-TEST", now, trust)
	if err != nil || !created {
		t.Fatalf("minting through the CNG store: created=%v err=%v", created, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, deviceCAKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key file was written beside the CNG key: %v", err)
	}
	assertNotExportable(t, ca)
	verifyLeaf(t, ca, now)

	reopened, err := openCNGKeyStore(name)
	if err != nil {
		t.Fatal(err)
	}
	again, created, err := openDeviceCA(context.Background(), reopened, dir, "DESKTOP-TEST", now.Add(time.Hour), trust)
	if err != nil || created || again.Info().Fingerprint != ca.Info().Fingerprint {
		t.Fatalf("restart: created=%v err=%v; the CNG key must be reopened by name", created, err)
	}
	assertNotExportable(t, again)
	verifyLeaf(t, again, now.Add(time.Hour))
}

// Deleting the device root's key removes it from the provider, and deleting a key that is not there
// succeeds, so an uninstall that runs twice, or on a device that never made a root, does not fail.
// The test uses a key name of its own.
func TestCNGKeyDeleteRemovesTheKey(t *testing.T) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := deviceRootKeyName + "-test-" + hex.EncodeToString(b[:])
	keys, err := openCNGKeyStore(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Generate(); err != nil {
		t.Fatal(err)
	}
	if err := deleteCNGKey(name); err != nil {
		t.Fatalf("deleting the key: %v", err)
	}
	reopened, err := openCNGKeyStore(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Key(); err == nil {
		t.Fatal("the key still opens after it was deleted")
	}
	if err := deleteCNGKey(name); err != nil {
		t.Fatalf("deleting a key that is not there: %v", err)
	}
}
