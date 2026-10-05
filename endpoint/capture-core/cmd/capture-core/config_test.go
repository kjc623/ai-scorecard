package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validDrainConfig returns a Config whose drain fields all validate, so each test can mutate one
// axis and assert that it is the only thing rejected.
func validDrainConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		DeviceEndpoint: "https://ingest.example.com",
		AuthMode:       "x509",
		CredentialFile: filepath.Join(dir, "credential.sealed"),
		SpoolDir:       filepath.Join(dir, "spool"),
		BackoffBase:    time.Second,
		BackoffCap:     time.Minute,
	}
}

func TestValidateDrainAcceptsBareEndpoint(t *testing.T) {
	for _, ep := range []string{
		"https://ingest.example.com",
		"https://ingest.example.com/",
		"https://ingest.example.com:8443",
	} {
		cfg := validDrainConfig(t)
		cfg.DeviceEndpoint = ep
		if err := cfg.validateDrain(); err != nil {
			t.Fatalf("endpoint %q rejected: %v", ep, err)
		}
	}
}

func TestValidateDrainRejectsPathPrefix(t *testing.T) {
	for _, ep := range []string{
		"https://ingest.example.com/v1/events",
		"https://ingest.example.com/prefix",
		"https://ingest.example.com//",
	} {
		cfg := validDrainConfig(t)
		cfg.DeviceEndpoint = ep
		if err := cfg.validateDrain(); err == nil {
			t.Fatalf("endpoint %q with a path prefix was accepted", ep)
		}
	}
}

func TestValidateDrainRejectsCredentialInsideSpool(t *testing.T) {
	dir := t.TempDir()
	cfg := validDrainConfig(t)
	cfg.SpoolDir = dir
	cfg.CredentialFile = filepath.Join(dir, "credential.sealed")
	if err := cfg.validateDrain(); err == nil {
		t.Fatal("a credential file inside the spool dir was accepted")
	}
}

func TestValidateDrainBackoffBounds(t *testing.T) {
	cfg := validDrainConfig(t)
	cfg.BackoffBase = 0
	if err := cfg.validateDrain(); err == nil {
		t.Fatal("a zero --backoff-base was accepted")
	}

	cfg = validDrainConfig(t)
	cfg.BackoffCap = 0
	if err := cfg.validateDrain(); err == nil {
		t.Fatal("a zero --backoff-cap was accepted")
	}

	cfg = validDrainConfig(t)
	cfg.BackoffBase = time.Minute
	cfg.BackoffCap = time.Second
	if err := cfg.validateDrain(); err == nil {
		t.Fatal("--backoff-base exceeding --backoff-cap was accepted")
	}
}

func TestValidateDrainAuthMode(t *testing.T) {
	for _, mode := range []string{"x509", "dpop"} {
		cfg := validDrainConfig(t)
		cfg.AuthMode = mode
		if err := cfg.validateDrain(); err != nil {
			t.Fatalf("auth mode %q rejected: %v", mode, err)
		}
	}
	for _, mode := range []string{"", "dev", "bearer", "X509"} {
		cfg := validDrainConfig(t)
		cfg.AuthMode = mode
		if err := cfg.validateDrain(); err == nil {
			t.Fatalf("auth mode %q was accepted", mode)
		}
	}
}

func TestValidateBootstrapCredentials(t *testing.T) {
	cfg := validDrainConfig(t)
	cfg.EnrolmentToken, cfg.DeploymentKey = "sac1.tenant.secret", "sacdk_secret"
	if err := cfg.validateDrain(); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("a token and a deployment key together = %v, want a refusal", err)
	}
	cfg = validDrainConfig(t)
	cfg.DeploymentKey = "sacdk_secret"
	if err := cfg.validateDrain(); err != nil {
		t.Fatalf("a deployment key alone was refused: %v", err)
	}
	cfg = Config{DeploymentKey: "sacdk_secret"}
	if err := cfg.validateDrain(); err == nil {
		t.Fatal("a deployment key with no device endpoint was accepted")
	}
}

// With no --bundle the bundle is fetched, which needs an endpoint; the old rule that the two flags
// go together still holds for a device that has nowhere to fetch from.
func TestValidatePolicyKeyWithoutBundle(t *testing.T) {
	base := Config{SpoolDir: "s", SpoolKey: "k", Retention: "1h", AttachmentCap: 1, TenantID: "t", DeviceID: "d"}
	cfg := base
	cfg.PolicyKey = strings.Repeat("ab", 32)
	if err := cfg.validate(runMode{}); err == nil {
		t.Fatal("a policy key with no bundle and no endpoint was accepted")
	}
	cfg = base
	cfg.BundlePath = "bundle.json"
	if err := cfg.validate(runMode{}); err == nil {
		t.Fatal("a bundle with no policy key was accepted")
	}
	cfg = validDrainConfig(t)
	cfg.SpoolKey, cfg.Retention, cfg.AttachmentCap = "k", "1h", 1
	cfg.PolicyKey = strings.Repeat("ab", 32)
	if err := cfg.validate(runMode{}); err != nil {
		t.Fatalf("a policy key with an endpoint and no bundle was refused: %v", err)
	}
	if !cfg.fetchesPolicy() {
		t.Fatal("the bundle is not fetched with a key, an endpoint and no --bundle")
	}
}

func TestGeneratesDeviceCAOnlyWhenNoPairIsConfigured(t *testing.T) {
	cfg := validDrainConfig(t)
	if cfg.generatesDeviceCA() {
		t.Fatal("a CA is generated although nothing trusts it")
	}
	cfg.TrustInstall = true
	if !cfg.generatesDeviceCA() {
		t.Fatal("trust install with no CA pair does not generate the device CA")
	}
	cfg.CACertFile, cfg.CAKeyFile = "ca.pem", "ca.key"
	if cfg.generatesDeviceCA() {
		t.Fatal("a configured CA pair (the lab profile) was replaced by a generated one")
	}
	local := Config{CLIShim: true}
	if local.generatesDeviceCA() {
		t.Fatal("a local run with no state directory generates a CA")
	}
	local.StateDir = t.TempDir()
	if !local.generatesDeviceCA() {
		t.Fatal("an explicit --state-dir does not hold a generated CA")
	}
}
