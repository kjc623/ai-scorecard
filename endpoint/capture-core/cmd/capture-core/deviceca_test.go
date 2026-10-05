package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
)

// The CA is minted once and reused: a restart must hand proxy.tls and the trust store the root the
// device already trusts, not a new one every start.
func TestDeviceCAGeneratedOnceAndReused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), deviceCADir)
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

	cert, key, created, err := ensureDeviceCA(dir, "DESKTOP-01", now)
	if err != nil || !created {
		t.Fatalf("first start: created=%v err=%v", created, err)
	}
	if _, err := tlsproxy.NewCAFromPEM(cert, key, now); err != nil {
		t.Fatalf("the generated pair is not a usable interception CA: %v", err)
	}
	block, _ := pem.Decode(cert)
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.IsCA || parsed.NotAfter.Sub(now) < deviceCAValidity-2*time.Hour {
		t.Fatalf("root is CA=%v valid until %s; want a CA valid for %s", parsed.IsCA, parsed.NotAfter, deviceCAValidity)
	}
	if err := checkProtectedFile(filepath.Join(dir, deviceCAKeyFile)); err != nil {
		t.Fatalf("the key file is not protected: %v", err)
	}

	for i := 0; i < 2; i++ {
		cert2, key2, created2, err := ensureDeviceCA(dir, "DESKTOP-01", now.Add(time.Duration(i+1)*24*time.Hour))
		if err != nil || created2 {
			t.Fatalf("restart %d: created=%v err=%v; the kept CA must be reused", i, created2, err)
		}
		if !bytes.Equal(cert, cert2) || !bytes.Equal(key, key2) {
			t.Fatalf("restart %d handed out a different pair", i)
		}
	}
}

// Renewal happens at a start within the margin, never by letting the root expire under live
// connections.
func TestDeviceCARenewedNearExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), deviceCADir)
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	cert, _, _, err := ensureDeviceCA(dir, "DESKTOP-01", now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(deviceCAValidity - deviceCARenewBefore + time.Hour)
	cert2, _, created, err := ensureDeviceCA(dir, "DESKTOP-01", later)
	if err != nil || !created || bytes.Equal(cert, cert2) {
		t.Fatalf("near expiry: created=%v err=%v; want a new root", created, err)
	}
}

// A certificate whose key is missing or does not match is not this device's CA.
func TestDeviceCAReplacedWhenThePairIsBroken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), deviceCADir)
	now := time.Now()
	if _, _, _, err := ensureDeviceCA(dir, "DESKTOP-01", now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, deviceCAKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, created, err := ensureDeviceCA(dir, "DESKTOP-01", now); err != nil || !created {
		t.Fatalf("missing key: created=%v err=%v; want a new pair", created, err)
	}
}
