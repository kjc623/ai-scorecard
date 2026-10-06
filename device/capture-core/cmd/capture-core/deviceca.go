package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/capture-core/state"
)

// The per-device interception CA. A CA that was the same on every device would make one device's
// key every device's interception capability, so each device mints its own on first start, keeps
// the key in the protected state directory, and installs the public half in the trust store.
const (
	deviceCACertFile = "ca.pem"
	deviceCAKeyFile  = "ca.key"

	// deviceCAValidity is the root's life. It is installed once and reused across restarts.
	deviceCAValidity = 2 * 365 * 24 * time.Hour
	// deviceCARenewBefore is how close to expiry a start replaces the root. Renewal happens only
	// at start; an expired root would break every intercepted connection.
	deviceCARenewBefore = 60 * 24 * time.Hour
)

// ensureDeviceCA returns the CA pair kept in dir, minting a new one when there is none, when it is
// unusable, or when it is near expiry. created reports a new pair.
func ensureDeviceCA(dir, label string, now time.Time) (certPEM, keyPEM []byte, created bool, err error) {
	if certPEM, keyPEM, why := loadDeviceCA(dir, now); why == "" {
		return certPEM, keyPEM, false, nil
	}
	ca, err := tlsproxy.NewCAValidFor(label, now, deviceCAValidity)
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
	// The key first: a crash between the two writes leaves a key with no certificate, which the
	// next start replaces, never a certificate whose key is missing.
	if err := state.WriteFile(filepath.Join(dir, deviceCAKeyFile), keyPEM); err != nil {
		return nil, nil, false, fmt.Errorf("device CA key: %w", err)
	}
	if err := state.WriteFile(filepath.Join(dir, deviceCACertFile), certPEM); err != nil {
		return nil, nil, false, fmt.Errorf("device CA certificate: %w", err)
	}
	return certPEM, keyPEM, true, nil
}

// loadDeviceCA returns the kept pair, or the reason it cannot be reused. A key file another
// account can read, or could have planted, is not this device's secret, so it is replaced.
func loadDeviceCA(dir string, now time.Time) (certPEM, keyPEM []byte, why string) {
	keyPath := filepath.Join(dir, deviceCAKeyFile)
	certPEM, err := os.ReadFile(filepath.Join(dir, deviceCACertFile))
	if err != nil {
		return nil, nil, "no certificate"
	}
	if err := state.CheckFile(keyPath); err != nil {
		return nil, nil, "key file: " + err.Error()
	}
	if keyPEM, err = os.ReadFile(keyPath); err != nil {
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
