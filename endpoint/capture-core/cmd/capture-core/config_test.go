package main

import (
	"path/filepath"
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
