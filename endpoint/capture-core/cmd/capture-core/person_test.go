package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	capturespool "github.com/shadow-ai-capture/device/capture-spool"
	"github.com/shadow-ai-capture/device/protocol"
)

// The protocol package's DeriveUserRef vectors (computed outside Go), so the device is checked
// against the derivation the directory side uses, not against itself.
const (
	vectorKey    = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	vectorUPNRef = "u_ed0bf663359a09dab5105ed60a56e835" // upn  Ada.Lovelace@Contoso.com
	vectorOIDRef = "u_a504db1d5d49e461d150819d7d9286e8" // oid  6F9619FF-8B86-D011-B42D-00C04FC964FF
	vectorAcctRf = "u_07eff34d88f27c2bd74a0274623831b7" // acct DESKTOP-01\Kyle
	// entraSID is the Entra account whose object id is the oid vector, written by hand from the
	// GUID (hostinfo's TestObjectIDFromSID shows the arithmetic).
	entraSID = "S-1-12-1-1872108031-3490810758-3221237172-4284795215"
)

// enrolledService is a drain-configured service whose sealed credential carries the tenant's
// user_ref key, so resolveIdentity takes the production path: load, adopt, derive.
func enrolledService(t *testing.T, cfg Config, console *fakeConsole) *service {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "spool.key")
	spoolDir := filepath.Join(dir, "spool")
	credFile := filepath.Join(dir, "credential.sealed")
	keys, err := capturespool.NewFileKeyProvider(keyPath, spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := credential.Open(credFile, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Save(&credential.Credential{
		Mode:       protocol.AuthModeDPoP,
		DeviceID:   "3f0c1a2b-0000-4000-8000-000000000001",
		TenantID:   "11111111-1111-4111-8111-111111111111",
		UserRefKey: vectorKey,
		PrivateKey: testPrivateKeyPEM(t),
		JWK:        &protocol.JWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y"},
	}); err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(stubSink{}, time.Now, func() string { return "evt" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.DeviceEndpoint, cfg.AuthMode, cfg.CredentialFile = "https://ingest.example.invalid", "dpop", credFile
	cfg.SpoolKey, cfg.SpoolDir = keyPath, spoolDir
	cfg.BackoffBase, cfg.BackoffCap, cfg.DrainInterval = time.Second, time.Second, time.Second
	return &service{
		cfg:    cfg,
		pipe:   pipe,
		spool:  &spoolHolder{cfg: Config{SpoolKey: keyPath, SpoolDir: spoolDir}},
		people: newPeople(console.sources()),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func currentIdentity(t *testing.T, s *service) core.Identity {
	t.Helper()
	id, ok := s.pipe.Identity()
	if !ok {
		t.Fatal("no identity is installed")
	}
	return id
}

// The user_ref follows the person at the console: UPN first, then the Entra object id, then
// DOMAIN\user, and the unattributed reference when nobody is there. A change of user re-stamps the
// identity without a restart, and the device id stays the issued one throughout.
func TestConsoleUserChangeRestampsTheIdentity(t *testing.T) {
	console := &fakeConsole{}
	console.set(hostinfo.User{SID: entraSID, Account: `AzureAD\AdaLovelace`, UPN: "Ada.Lovelace@Contoso.com"}, nil)
	s := enrolledService(t, Config{}, console)
	if err := s.resolveIdentity(context.Background()); err != nil {
		t.Fatalf("resolveIdentity: %v", err)
	}
	id := currentIdentity(t, s)
	if id.UserRef != vectorUPNRef || id.SubjectName != "Ada.Lovelace@Contoso.com" {
		t.Fatalf("first user = %+v, want the UPN-derived ref and the UPN as the clear name", id)
	}
	if id.DeviceID != "3f0c1a2b-0000-4000-8000-000000000001" {
		t.Fatalf("device id = %q, want the issued one", id.DeviceID)
	}

	for _, step := range []struct {
		name    string
		user    hostinfo.User
		err     error
		ref     string
		subject string
	}{
		{"an Entra account whose UPN did not resolve", hostinfo.User{SID: entraSID, Account: `AzureAD\GraceHopper`}, nil, vectorOIDRef, `AzureAD\GraceHopper`},
		{"a local account", hostinfo.User{SID: "S-1-5-21-1-2-3-1001", Account: `DESKTOP-01\Kyle`}, nil, vectorAcctRf, `DESKTOP-01\Kyle`},
		{"nobody at the console", hostinfo.User{}, hostinfo.ErrNoConsoleUser, unattributedUserRef, ""},
	} {
		console.set(step.user, step.err)
		s.refreshPerson()
		id := currentIdentity(t, s)
		if id.UserRef != step.ref || id.SubjectName != step.subject {
			t.Errorf("%s: identity = %+v, want user_ref %s and name %q", step.name, id, step.ref, step.subject)
		}
		if id.DeviceID != "3f0c1a2b-0000-4000-8000-000000000001" {
			t.Errorf("%s: device id changed to %q", step.name, id.DeviceID)
		}
	}
}

// A tenant that sets device_identity to 'hashed' stops receiving the clear name from the next
// observation on; the pseudonymous user_ref is unaffected.
func TestHashedTenantDropsTheClearName(t *testing.T) {
	console := &fakeConsole{}
	console.set(hostinfo.User{SID: entraSID, UPN: "Ada.Lovelace@Contoso.com"}, nil)
	s := enrolledService(t, Config{}, console)
	if err := s.resolveIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.adoptDeviceIdentity(protocol.DeviceIdentityHashed)
	if id := currentIdentity(t, s); id.SubjectName != "" || id.UserRef != vectorUPNRef {
		t.Fatalf("after 'hashed' identity = %+v, want no name and the same ref", id)
	}
}

// The lab profile's SAC_USER_REF still wins over the console user; the clear name is the console
// user's, not the service account's.
func TestConfiguredUserRefWins(t *testing.T) {
	console := &fakeConsole{}
	console.set(hostinfo.User{SID: entraSID, Account: `AzureAD\AdaLovelace`, UPN: "Ada.Lovelace@Contoso.com"}, nil)
	s := enrolledService(t, Config{UserRef: "lab-user"}, console)
	if err := s.resolveIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if id := currentIdentity(t, s); id.UserRef != "lab-user" || id.SubjectName != "Ada.Lovelace@Contoso.com" {
		t.Fatalf("identity = %+v, want the configured ref and the console user's name", id)
	}
	if _, _, source := s.personIdentity(); source != "configured" {
		t.Fatalf("user_ref source = %q", source)
	}
}

// A server that issued no user_ref key (an older control-api) leaves the person unattributed rather
// than sending anything derived from a name.
func TestNoUserRefKeyIsUnattributed(t *testing.T) {
	console := &fakeConsole{}
	console.set(hostinfo.User{SID: entraSID, UPN: "Ada.Lovelace@Contoso.com"}, nil)
	s := &service{cfg: Config{}, people: newPeople(console.sources())}
	s.people.refresh()
	ref, subject, source := s.personIdentity()
	if ref != unattributedUserRef || subject != "Ada.Lovelace@Contoso.com" || source == "upn" {
		t.Fatalf("personIdentity = %q %q %q, want unattributed with the clear name", ref, subject, source)
	}
}
