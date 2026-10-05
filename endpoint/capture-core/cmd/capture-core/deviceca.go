package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
)

// The per-device interception CA, when the agent keeps its own (Config.generatesDeviceCA). The lab
// profile ships a CA pair minted offline by cmd/sac-bundle; a tenant package carries none, because
// a CA that is the same on every device would make one device's key every device's interception
// capability (docs/01 §3.3, A3). So the device mints its CA on first start, keeps the key where only
// SYSTEM and Administrators can read it, and installs the public half through trust/ exactly as a
// pinned pair is installed.
const (
	deviceCADir      = "device-ca"
	deviceCACertFile = "ca.pem"
	deviceCAKeyFile  = "ca.key"

	// deviceCAValidity is the root's life. The root is installed once and reused across restarts,
	// so it outlives the three months of a CA minted per run.
	deviceCAValidity = 2 * 365 * 24 * time.Hour
	// deviceCARenewBefore is how close to expiry a start replaces the root. Renewal happens only at
	// start, and a machine restarts well within this margin; an expired root would break every
	// intercepted connection rather than merely stop capture.
	deviceCARenewBefore = 60 * 24 * time.Hour
)

// ensureDeviceCA returns the CA pair kept in dir, minting a new one when there is none, when it is
// unusable, or when it is near expiry. created reports a new pair, so the caller can say the
// trusted root changed.
func ensureDeviceCA(dir, label string, now time.Time) (certPEM, keyPEM []byte, created bool, err error) {
	certPEM, keyPEM, why := loadDeviceCA(dir, now)
	if why == "" {
		return certPEM, keyPEM, false, nil
	}
	ca, err := tlsproxy.NewCAValidFor(label, nil, now, deviceCAValidity)
	if err != nil {
		return nil, nil, false, err
	}
	certPEM = ca.PEM()
	if keyPEM, err = ca.KeyPEM(); err != nil {
		return nil, nil, false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, false, fmt.Errorf("device CA directory: %w", err)
	}
	// The key first: a crash between the two writes leaves a key with no certificate, which the next
	// start replaces, never a certificate whose key is missing.
	if err := writeProtectedFile(filepath.Join(dir, deviceCAKeyFile), keyPEM); err != nil {
		return nil, nil, false, fmt.Errorf("device CA key: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, deviceCACertFile), certPEM, 0o644); err != nil {
		return nil, nil, false, fmt.Errorf("device CA certificate: %w", err)
	}
	return certPEM, keyPEM, true, nil
}

// loadDeviceCA returns the kept pair, or the reason it cannot be reused. A key file anyone else can
// read, or that someone else planted, is not this device's secret, so it is replaced rather than
// trusted.
func loadDeviceCA(dir string, now time.Time) (certPEM, keyPEM []byte, why string) {
	keyPath := filepath.Join(dir, deviceCAKeyFile)
	certPEM, err := os.ReadFile(filepath.Join(dir, deviceCACertFile))
	if err != nil {
		return nil, nil, "no certificate"
	}
	if err := checkProtectedFile(keyPath); err != nil {
		return nil, nil, "key file: " + err.Error()
	}
	keyPEM, err = os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, "key unreadable"
	}
	ca, err := tlsproxy.NewCAFromPEM(certPEM, keyPEM, now)
	if err != nil {
		return nil, nil, err.Error()
	}
	if now.Add(deviceCARenewBefore).After(ca.Info().NotAfter) {
		return nil, nil, "near expiry"
	}
	return certPEM, keyPEM, ""
}

// writeFileAtomic replaces path with data through a temporary file in the same directory.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// tempSibling names a not-yet-existing file beside path.
func tempSibling(path string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+hex.EncodeToString(b[:])), nil
}

var errNotProtected = errors.New("readable or writable by an account other than SYSTEM, Administrators or this service")
