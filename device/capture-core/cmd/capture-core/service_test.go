package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

const entraSID = "S-1-12-1-1234567890-1234567890-1234567890-1234567890"

// expectedRef is the user_ref the device derives for a UPN under the test tenant's key.
func expectedRef(t *testing.T, upn string) string {
	t.Helper()
	key, _ := base64.RawURLEncoding.DecodeString(userRefKey)
	ref, err := protocol.DeriveUserRef(key, protocol.UserRefUPN, upn)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// sharedTrust makes every service in a test share one fake trust store, so the test can see what
// was installed and removed.
func sharedTrust(t *testing.T) *fakeTrustStore {
	t.Helper()
	f := &fakeTrustStore{}
	prev := platform.trustStore
	platform.trustStore = func(func(string, ...any)) trustStore { return f }
	t.Cleanup(func() { platform.trustStore = prev })
	return f
}

// A first start of a tenant-packaged device: it enrols with the deployment key and the attestation
// the OS states, fetches and caches the tenant's bundle before any provider is built, attributes
// observations to the console user, protects its state directory, installs its own CA, and
// removes the root again when it stops. A restart with the cloud unreachable enforces the cached
// bundle.
func TestServiceEnrolsFetchesItsPolicyAndRuns(t *testing.T) {
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
	trust := sharedTrust(t)
	cfg := testConfig(t, cloud, pub)

	svc, err := newService(context.Background(), cfg, testLogger(t))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	if b := svc.currentBundle(); b == nil || b.Version != "5" {
		t.Fatalf("bundle before startup = %+v, want version 5 fetched from GET /v1/policy", b)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = svc.Stop(context.Background())
		}
	}()

	enrols := cloud.enrolments()
	if len(enrols) != 1 {
		t.Fatalf("enrolments = %d, want 1", len(enrols))
	}
	enrol := enrols[0]
	if enrol.DeploymentKey != "sacdk_test" || enrol.Device.ManagedState != "managed" ||
		enrol.Attestation == nil || enrol.Attestation.IntuneDeviceID != "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55" {
		t.Fatalf("enrolment = %+v", enrol)
	}
	if enrol.Device.HardwareIdentityHash != drain.HardwareIdentityHash(cloudTenant, "smbios:a2219e09-2c68-6d1d-a831-345a6060843c") {
		t.Fatal("the hardware identity is not seeded from the hardware")
	}
	id, ok := svc.pipe.Identity()
	if !ok || id.DeviceID != cloudDevice || id.TenantID != cloudTenant {
		t.Fatalf("identity = %+v, want the server-minted device", id)
	}
	if id.UserRef != expectedRef(t, "Ada.Lovelace@Contoso.com") || id.SubjectName != "Ada.Lovelace@Contoso.com" {
		t.Fatalf("identity person = %q %q, want the UPN-derived ref under the issued key", id.UserRef, id.SubjectName)
	}
	for _, f := range []string{state.SpoolKeyFile, state.ContentKeyFile, state.CredentialFile} {
		if err := state.CheckFile(svc.dir.Path(f)); err != nil {
			t.Errorf("%s is not protected: %v", f, err)
		}
	}
	if err := state.CheckFile(svc.dir.Root()); err != nil {
		t.Errorf("the state directory is not protected: %v", err)
	}
	if _, err := os.Stat(svc.dir.Path(state.PolicyDir, "bundle.json")); err != nil {
		t.Errorf("the verified bundle was not cached: %v", err)
	}
	trust.mu.Lock()
	installed := len(trust.installed) > 0
	trust.mu.Unlock()
	if !installed {
		t.Error("the per-device CA was not installed in the trust store")
	}
	// The first health tick runs as the service starts.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(svc.dir.Path(state.HealthFile)); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Errorf("no health snapshot was written: %v", err)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap := svc.health.Snapshot(); !snap.Enrolled || snap.ManagedState != "managed" || snap.UserRefSource != "upn" || snap.PolicyFetch == nil {
		t.Errorf("health snapshot = %+v", snap)
	}
	// Each row is named by its collector code: a provider's by its Name, the extension's with the
	// native host's hyphenated default normalised.
	svc.health.SetExtensionReport(protocol.NewHealthReport("", "capture-extension", "", time.Now()))
	var names []string
	for _, rep := range svc.health.healthRequest().Collectors {
		if !protocol.Collector(rep.Collector).Valid() {
			t.Errorf("health row names %q, which is not a collector code", rep.Collector)
		}
		names = append(names, rep.Collector)
	}
	if got, want := strings.Join(names, ","), "egress_proxy,loopback_broker,cli_shim,capture_extension,classifier_host"; got != want {
		t.Errorf("health rows = %s, want %s", got, want)
	}

	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true
	if trust.removes == 0 {
		t.Error("the trusted root was not removed at stop")
	}

	cloud.srv.Close()
	again, err := newService(context.Background(), cfg, testLogger(t))
	if err != nil {
		t.Fatalf("newService after restart: %v", err)
	}
	if b := again.currentBundle(); b == nil || b.Version != "5" {
		t.Fatalf("bundle after a restart with the cloud unreachable = %+v, want the cached version 5", b)
	}
	if len(cloud.enrolments()) != 1 {
		t.Fatal("the restart enrolled again instead of using the stored credential")
	}
}

// A tenant with no bundle leaves a new device enrolled, reporting and at M0.
func TestServiceWithNoTenantBundleRunsAtM0(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, nil)
	svc, err := newService(context.Background(), testConfig(t, cloud, pub), testLogger(t))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	if svc.currentBundle() != nil || svc.policyResult().Outcome != "fell_to_m0" {
		t.Fatalf("bundle %+v result %+v, want none and fell_to_m0", svc.currentBundle(), svc.policyResult())
	}
	if !svc.drainer.Status().Enrolled {
		t.Fatal("the device did not enrol")
	}
}

// The console user is re-read and the identity re-stamped when the person changes; with nobody at
// the console, proxy observations are unattributed rather than attributed to the service account.
func TestConsoleUserChangeRestampsTheIdentity(t *testing.T) {
	cloud := startFakeCloud(t, nil)
	console := &fakeConsole{}
	console.set(hostinfo.User{SID: entraSID, Account: `AzureAD\AdaLovelace`, UPN: "ada@contoso.com"}, nil)
	withConsole(t, console)
	svc, err := newService(context.Background(), testConfig(t, cloud, nil), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := (identityResolver{svc}).Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id, _ := svc.pipe.Identity(); id.UserRef != expectedRef(t, "ada@contoso.com") {
		t.Fatalf("user_ref = %q, want Ada's", id.UserRef)
	}
	console.set(hostinfo.User{SID: "S-1-5-21-1-2-3-1002", Account: `CONTOSO\grace`, UPN: "grace@contoso.com"}, nil)
	svc.refreshPerson()
	if id, _ := svc.pipe.Identity(); id.UserRef != expectedRef(t, "grace@contoso.com") || id.SubjectName != "grace@contoso.com" {
		t.Fatalf("identity after the switch = %+v, want Grace", id)
	}
	console.set(hostinfo.User{}, hostinfo.ErrNoConsoleUser)
	svc.refreshPerson()
	if id, _ := svc.pipe.Identity(); id.UserRef != unattributedUserRef || id.SubjectName != "" {
		t.Fatalf("identity with nobody at the console = %+v, want unattributed", id)
	}
}

// A tenant whose setting is hashed receives no clear account name, from the next observation on.
func TestHashedTenantDropsTheClearName(t *testing.T) {
	cloud := startFakeCloud(t, nil)
	console := &fakeConsole{}
	console.set(hostinfo.User{SID: entraSID, Account: `AzureAD\AdaLovelace`, UPN: "ada@contoso.com"}, nil)
	withConsole(t, console)
	svc, err := newService(context.Background(), testConfig(t, cloud, nil), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := (identityResolver{svc}).Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	svc.adoptDeviceIdentity(protocol.DeviceIdentityHashed)
	id, _ := svc.pipe.Identity()
	if id.SubjectName != "" || id.UserRef != expectedRef(t, "ada@contoso.com") {
		t.Fatalf("identity under hashed = %+v, want the ref and no name", id)
	}
	if svc.clearHostname() != "" || (svc.resolvedHostname() != "" && svc.hostnameHash() == "") {
		t.Fatal("a hashed tenant would be sent the clear hostname")
	}
}

// Observations reach the edge with the issued identity.
func TestSpooledObservationsAreDeliveredWithTheIssuedIdentity(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, signedTestBundle(t, priv, "5", protocol.ModeM0))
	svc, err := newService(context.Background(), testConfig(t, cloud, pub), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Stop(context.Background()) }()
	session := newNativeSession(svc, svc.peerPerson(hostinfo.User{}))
	answer := session.Handle(context.Background(), observationFrame(t, "obs-1", nil))
	if typ := frameType(t, answer); typ != protocol.TypeAck {
		t.Fatalf("observation answered %s: %s", typ, answer)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(cloud.receivedEvents()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	events := cloud.receivedEvents()
	if len(events) != 1 {
		t.Fatalf("the edge received %d events, want 1", len(events))
	}
	var env struct {
		TenantID string `json:"tenant_id"`
		DeviceID string `json:"device_id"`
		UserRef  string `json:"user_ref"`
		Mode     string `json:"collection_mode"`
	}
	if err := json.Unmarshal(events[0], &env); err != nil {
		t.Fatal(err)
	}
	if env.TenantID != cloudTenant || env.DeviceID != cloudDevice || env.Mode != "m0" || env.UserRef != unattributedUserRef {
		t.Fatalf("delivered envelope = %+v", env)
	}
}

// lockedBuffer is a log destination the service's goroutines can share with the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A discovery recorded on an enrolled service is spooled, logged as spooled, and reaches the edge's
// /v1/events with the issued identity.
func TestRecordedDiscoveryReachesTheEdge(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cloud := startFakeCloud(t, signedTestBundle(t, priv, "5", protocol.ModeM0))
	logs := &lockedBuffer{}
	svc, err := newService(context.Background(), testConfig(t, cloud, pub), newLogger("info", logs))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Stop(context.Background()) }()

	sum := sha256.Sum256([]byte(cloudTenant + "|" + cloudDevice + "|app:cursor|app_installed"))
	err = svc.pipe.Record(context.Background(), core.Fact{
		Kind:            protocol.KindDiscovery,
		Route:           protocol.RouteInvScan,
		ToolFingerprint: "app:cursor",
		Person:          &core.Person{UserRef: unattributedUserRef},
		OccurredAt:      time.Now(),
		DedupKey:        "sha256:" + hex.EncodeToString(sum[:]),
		FactFields: core.FactFields{
			DiscoveryType:  protocol.DiscoveryTypeAppInstalled,
			DetectionBasis: protocol.DetectionBasisInstalledScan,
			AppVersion:     "0.48.1",
		},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(cloud.receivedEvents()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	events := cloud.receivedEvents()
	if len(events) != 1 {
		t.Fatalf("the edge received %d events, want 1", len(events))
	}
	var env struct {
		TenantID      string `json:"tenant_id"`
		DeviceID      string `json:"device_id"`
		UserRef       string `json:"user_ref"`
		Kind          string `json:"kind"`
		Source        string `json:"source"`
		Direction     string `json:"direction"`
		DiscoveryType string `json:"discovery_type"`
		AppVersion    string `json:"app_version"`
	}
	if err := json.Unmarshal(events[0], &env); err != nil {
		t.Fatal(err)
	}
	if env.TenantID != cloudTenant || env.DeviceID != cloudDevice || env.UserRef != unattributedUserRef ||
		env.Kind != "discovery" || env.Source != "inv.scan" || env.Direction != "none" ||
		env.DiscoveryType != "app_installed" || env.AppVersion != "0.48.1" {
		t.Fatalf("delivered envelope = %s", events[0])
	}
	if log := logs.String(); !strings.Contains(log, `"msg":"envelope spooled"`) || !strings.Contains(log, `"discovery_type":"app_installed"`) {
		t.Fatalf("the service did not log the spooled record:\n%s", log)
	}
}
