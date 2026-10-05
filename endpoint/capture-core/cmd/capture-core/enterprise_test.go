package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	cloudTenant = "11111111-1111-4111-8111-111111111111"
	cloudDevice = "3f0c1a2b-0000-4000-8000-000000000001"
)

// fakeCloud is the device-facing half of control-api for an enterprise device: enrolment by
// deployment key, the DPoP token, GET /v1/policy and the heartbeat, over real TLS.
type fakeCloud struct {
	t      *testing.T
	srv    *httptest.Server
	caFile string
	bundle []byte // the signed envelope served; nil answers 404 no_policy_bundle

	mu         sync.Mutex
	enrol      protocol.EnrolmentRequest
	enrols     int
	policyINMs []string
}

func startFakeCloud(t *testing.T, bundle []byte) *fakeCloud {
	t.Helper()
	c := &fakeCloud{t: t, bundle: bundle}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enrol", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		_ = json.Unmarshal(body, &c.enrol)
		c.enrols++
		c.mu.Unlock()
		writeIngestJSON(w, http.StatusOK, protocol.EnrolmentResponse{
			SchemaVersion: protocol.EnrolmentSchemaVersion, DeviceID: cloudDevice, TenantID: cloudTenant, Region: "eu",
			Credential: protocol.IssuedCredential{Mode: protocol.AuthModeDPoP, JWK: &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"}},
			UserRefKey: vectorKey, ServerTime: time.Now().UTC(),
		})
	})
	mux.HandleFunc("/v1/token", func(w http.ResponseWriter, r *http.Request) {
		writeIngestJSON(w, http.StatusOK, protocol.TokenResponse{AccessToken: "at", TokenType: protocol.TokenTypeDPoP, ExpiresIn: 900})
	})
	mux.HandleFunc("/v1/policy", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.policyINMs = append(c.policyINMs, r.Header.Get(protocol.HeaderIfNoneMatch))
		c.mu.Unlock()
		if c.bundle == nil {
			writeIngestJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "no_policy_bundle"}})
			return
		}
		w.Header().Set(protocol.HeaderETag, protocol.PolicyETag("5"))
		if protocol.ETagMatches(r.Header.Get(protocol.HeaderIfNoneMatch), protocol.PolicyETag("5")) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeIngestJSON(w, http.StatusOK, protocol.PolicyResponse{SchemaVersion: protocol.PolicySchemaVersion, BundleVersion: "5",
			SignedBundle: c.bundle, ServerTime: time.Now().UTC()})
	})
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		writeIngestJSON(w, http.StatusOK, protocol.HealthResponse{AckedAt: now, ServerTime: now, NextReportAfterS: 900})
	})
	c.srv = httptest.NewTLSServer(mux)
	t.Cleanup(c.srv.Close)
	c.caFile = filepath.Join(t.TempDir(), "edge-ca.pem")
	if err := os.WriteFile(c.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

// enterpriseConfig is what the generic capture-core.env and a tenant's ShadowAICapture.tenant.env
// give a device between them: no device id, no bundle, no CA pair, no user ref, no token.
func enterpriseConfig(t *testing.T, cloud *fakeCloud, pub ed25519.PublicKey) Config {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.SpoolDir = filepath.Join(dir, "spool")
	cfg.SpoolKey = filepath.Join(dir, "spool.key")
	cfg.CredentialFile = filepath.Join(dir, "state", "credential.sealed")
	cfg.PolicyKey, cfg.PolicyKeyID = hex.EncodeToString(pub), "policy-key-1"
	cfg.TenantID = cloudTenant
	cfg.DeviceEndpoint = cloud.srv.URL
	cfg.DeploymentKey = "sacdk_" + cloudTenant + "_secret"
	cfg.AuthMode = "dpop"
	cfg.CAFile = cloud.caFile
	cfg.EnableTLS, cfg.EnableLoopback = false, false
	cfg.DrainInterval, cfg.HealthInterval = time.Hour, time.Hour
	if err := cfg.validate(runMode{}); err != nil {
		t.Fatalf("an enterprise configuration does not validate: %v", err)
	}
	return cfg
}

func TestEnterpriseDeviceEnrolsByDeploymentKeyAndFetchesItsPolicy(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, signedTestBundle(t, priv, "5", protocol.ModeM1))
	withHostFacts(t, hostinfo.Facts{
		Attestation: protocol.DeviceAttestation{IntuneDeviceID: "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55", SerialNumber: "PF2X9K7Q"},
		SystemUUID:  "a2219e09-2c68-6d1d-a831-345a6060843c",
	})
	console := &fakeConsole{}
	console.set(hostinfo.User{SID: entraSID, Account: `AzureAD\AdaLovelace`, UPN: "Ada.Lovelace@Contoso.com"}, nil)
	withConsole(t, console)
	cfg := enterpriseConfig(t, cloud, pub)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc, err := newService(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	// The first bundle is in force before any provider is built or started.
	if b := svc.currentBundle(); b == nil || b.Version != "5" {
		t.Fatalf("bundle before startup = %+v, want version 5 fetched from GET /v1/policy", b)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = svc.Stop(context.Background()) }()

	cloud.mu.Lock()
	enrol, enrols := cloud.enrol, cloud.enrols
	cloud.mu.Unlock()
	if enrols != 1 {
		t.Fatalf("enrolments = %d, want 1", enrols)
	}
	if enrol.DeploymentKey != cfg.DeploymentKey || enrol.EnrolmentToken != "" {
		t.Fatalf("bootstrap credential = token %q key %q", enrol.EnrolmentToken, enrol.DeploymentKey)
	}
	if enrol.Attestation == nil || enrol.Attestation.IntuneDeviceID != "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55" || enrol.Device.ManagedState != "managed" {
		t.Fatalf("attestation %+v managed_state %q", enrol.Attestation, enrol.Device.ManagedState)
	}
	if enrol.Device.HardwareIdentityHash != drain.HardwareIdentityHash(cloudTenant, "", "smbios:a2219e09-2c68-6d1d-a831-345a6060843c") {
		t.Fatal("the hardware identity is not seeded from the hardware")
	}

	id, ok := svc.pipe.Identity()
	if !ok || id.DeviceID != cloudDevice || id.TenantID != cloudTenant {
		t.Fatalf("identity = %+v, want the server-minted device with no device id configured", id)
	}
	if id.UserRef != vectorUPNRef || id.SubjectName != "Ada.Lovelace@Contoso.com" {
		t.Fatalf("identity person = %q %q, want the UPN-derived ref under the issued key", id.UserRef, id.SubjectName)
	}
	if _, err := os.Stat(filepath.Join(cfg.stateDir(), policyCacheDir, "bundle.json")); err != nil {
		t.Fatalf("the verified bundle was not cached: %v", err)
	}
	if snap := svc.health.Snapshot(); snap.ManagedState != "managed" || snap.UserRefSource != "upn" || snap.PolicyFetch == nil || snap.PolicyFetch.LastAnswer != "served" {
		t.Fatalf("health snapshot managed=%q source=%q fetch=%+v", snap.ManagedState, snap.UserRefSource, snap.PolicyFetch)
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A restart with the cloud unreachable enforces the cached bundle before the network answers.
	cloud.srv.Close()
	again, err := newService(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("newService after restart: %v", err)
	}
	if b := again.currentBundle(); b == nil || b.Version != "5" {
		t.Fatalf("bundle after an offline restart = %+v, want the cached version 5", b)
	}
}

// A tenant with no bundle leaves a new device at M0, enrolled and reporting.
func TestEnterpriseDeviceWithNoTenantBundleRunsAtM0(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, nil)
	cfg := enterpriseConfig(t, cloud, pub)
	svc, err := newService(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	if svc.currentBundle() != nil {
		t.Fatal("a bundle is in force although the tenant has none")
	}
	if svc.policyResult().Outcome != "fell_to_m0" {
		t.Fatalf("policy result = %+v, want fell_to_m0", svc.policyResult())
	}
	if !svc.drainer.Status().Enrolled {
		t.Fatal("the device did not enrol")
	}
}
