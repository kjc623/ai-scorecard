package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/credential"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// stubSink is the minimal core.Sink for a resolveIdentity test: nothing reads it back.
type stubSink struct{}

func (stubSink) Append(e protocol.Entry) (protocol.Entry, error) { return e, nil }
func (stubSink) Stats() protocol.SpoolStats                      { return protocol.SpoolStats{} }

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// TestResolveIdentityAdoptsLoadedCredential proves the startup identity step: when a sealed
// credential exists, resolveIdentity installs its server-minted identity (the placeholder flags are
// ignored), before any provider starts.
func TestResolveIdentityAdoptsLoadedCredential(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "spool.key")
	spoolDir := filepath.Join(dir, "spool")
	credFile := filepath.Join(dir, "credential.sealed")

	keys, err := capturespool.NewFileKeyProvider(keyPath, spoolDir)
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	creds, err := credential.Open(credFile, keys)
	if err != nil {
		t.Fatalf("credential open: %v", err)
	}
	if err := creds.Save(&credential.Credential{
		Mode:       protocol.AuthModeDPoP,
		DeviceID:   "issued-device",
		TenantID:   "issued-tenant",
		PrivateKey: testPrivateKeyPEM(t),
		JWK:        &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"},
	}); err != nil {
		t.Fatalf("credential save: %v", err)
	}

	pipe, err := core.NewPipeline(stubSink{}, time.Now, func() string { return "evt" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	s := &service{
		cfg: Config{
			DeviceEndpoint: "https://ingest.example.invalid",
			AuthMode:       "dpop",
			CredentialFile: credFile,
			SpoolKey:       keyPath,
			SpoolDir:       spoolDir,
			TenantID:       "flag-tenant",
			DeviceID:       "flag-device",
			UserRef:        "flag-user",
			BackoffBase:    time.Second,
			BackoffCap:     time.Second,
			DrainInterval:  time.Second,
		},
		pipe:  pipe,
		spool: &spoolHolder{cfg: Config{SpoolKey: keyPath, SpoolDir: spoolDir}},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := s.resolveIdentity(context.Background()); err != nil {
		t.Fatalf("resolveIdentity: %v", err)
	}
	id, ok := pipe.Identity()
	if !ok {
		t.Fatal("identity is not resolved after resolveIdentity")
	}
	if id.TenantID != "issued-tenant" || id.DeviceID != "issued-device" {
		t.Fatalf("identity = %+v, want the issued (issued-tenant, issued-device), not the flags", id)
	}
	if id.UserRef != "flag-user" {
		t.Fatalf("user_ref = %q, want the flag value (the server does not issue a user_ref)", id.UserRef)
	}
}

// TestResolveIdentityNoDrainUsesFlags proves the local/offline fallback: with no --device-endpoint,
// the flags are the identity.
func TestResolveIdentityNoDrainUsesFlags(t *testing.T) {
	pipe, err := core.NewPipeline(stubSink{}, time.Now, func() string { return "evt" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	s := &service{
		cfg:  Config{TenantID: "flag-tenant", DeviceID: "flag-device", UserRef: "flag-user"},
		pipe: pipe,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := s.resolveIdentity(context.Background()); err != nil {
		t.Fatalf("resolveIdentity: %v", err)
	}
	id, ok := pipe.Identity()
	if !ok {
		t.Fatal("identity is not resolved after resolveIdentity")
	}
	if id.TenantID != "flag-tenant" || id.DeviceID != "flag-device" {
		t.Fatalf("identity = %+v, want the flags", id)
	}
}

// TestResolveIdentityUnresolvedOffline proves fail-closed: a drain-configured device that cannot
// enrol leaves the identity unresolved (so the pipeline refuses to mint) rather than adopting the
// flags.
func TestResolveIdentityUnresolvedOffline(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "spool.key")
	spoolDir := filepath.Join(dir, "spool")
	credFile := filepath.Join(dir, "credential.sealed")

	pipe, err := core.NewPipeline(stubSink{}, time.Now, func() string { return "evt" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	s := &service{
		cfg: Config{
			DeviceEndpoint: "https://ingest.example.invalid",
			AuthMode:       "dpop",
			CredentialFile: credFile,
			SpoolKey:       keyPath,
			SpoolDir:       spoolDir,
			EnrolmentToken: "tok-unused",
			TenantID:       "flag-tenant",
			DeviceID:       "flag-device",
			UserRef:        "flag-user",
			BackoffBase:    time.Second,
			BackoffCap:     time.Second,
			DrainInterval:  time.Second,
		},
		pipe:  pipe,
		spool: &spoolHolder{cfg: Config{SpoolKey: keyPath, SpoolDir: spoolDir}},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// The endpoint is unreachable, so the bounded synchronous enrol fails and identity stays
	// unresolved — never the flags.
	if err := s.resolveIdentity(context.Background()); err == nil {
		t.Fatal("resolveIdentity succeeded against an unreachable endpoint")
	}
	if _, ok := pipe.Identity(); ok {
		t.Fatal("identity was resolved against an unreachable endpoint; it must stay unresolved")
	}
	if _, err := pipe.Process(context.Background(), core.Observation{
		Route: protocol.RouteProxyTLS, Kind: protocol.KindPrompt, ToolFingerprint: "tool",
		OccurredAt: time.Now(), SizeBytes: 10,
		Decision: &protocol.Decision{RuleID: "r", Action: protocol.ActionLogged},
	}); !errors.Is(err, core.ErrIdentityUnresolved) {
		t.Fatalf("Process err = %v, want ErrIdentityUnresolved", err)
	}
}

// TestResolveIdentitySynchronousEnrol proves the first-run path: with no sealed credential and a
// reachable edge, resolveIdentity performs a bounded synchronous enrolment and installs the issued
// identity before any provider starts.
func TestResolveIdentitySynchronousEnrol(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "spool.key")
	spoolDir := filepath.Join(dir, "spool")
	credFile := filepath.Join(dir, "credential.sealed")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fi, err := startFakeIngest(log, dir, "issued-device", "issued-tenant")
	if err != nil {
		t.Fatalf("startFakeIngest: %v", err)
	}
	defer fi.close()

	pipe, err := core.NewPipeline(stubSink{}, time.Now, func() string { return "evt" })
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	s := &service{
		cfg: Config{
			DeviceEndpoint: fi.baseURL(),
			AuthMode:       "dpop",
			CAFile:         fi.caFile,
			CredentialFile: credFile,
			SpoolKey:       keyPath,
			SpoolDir:       spoolDir,
			EnrolmentToken: "tok-123",
			TenantID:       "flag-tenant",
			DeviceID:       "flag-device",
			UserRef:        "flag-user",
			MDMID:          "mdm-9",
			BackoffBase:    time.Second,
			BackoffCap:     time.Second,
			DrainInterval:  time.Second,
		},
		pipe:  pipe,
		spool: &spoolHolder{cfg: Config{SpoolKey: keyPath, SpoolDir: spoolDir}},
		log:   log,
	}

	if err := s.resolveIdentity(context.Background()); err != nil {
		t.Fatalf("resolveIdentity: %v", err)
	}
	id, ok := pipe.Identity()
	if !ok {
		t.Fatal("identity not issued after synchronous enrolment")
	}
	if id.TenantID != "issued-tenant" || id.DeviceID != "issued-device" {
		t.Fatalf("identity = %+v, want the issued (issued-tenant, issued-device), not the flags", id)
	}
}
