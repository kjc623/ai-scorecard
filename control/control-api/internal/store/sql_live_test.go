package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// The live test: every statement prepared as written, then the store driven through its methods as
// sac_control under forced row-level security, against the database SAC_TEST_PG_DSN names.

func TestStatementsPrepare(t *testing.T) {
	db := pgtest.Open(t)
	for i, s := range store.Statements {
		name := fmt.Sprintf("sac_store_probe_%d", i)
		if _, err := db.Exec("PREPARE " + name + " AS " + s.SQL); err != nil {
			t.Errorf("statement %q does not prepare: %v", s.Name, err)
			continue
		}
		_, _ = db.Exec("DEALLOCATE " + name)
	}
}

func TestStoreAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	now := time.Now().UTC().Truncate(time.Microsecond)

	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	tn, err := st.Tenant(ctx, tenant)
	if err != nil || !tn.Active() || tn.ResidencyRegion != "eastus" || tn.DeviceIdentity != "clear" {
		t.Fatalf("Tenant = %+v, %v", tn, err)
	}
	if _, err := st.Tenant(ctx, pgtest.UUID(t)); !errors.Is(err, store.ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}

	// Devices: insert, find by hardware identity, refresh without losing what the update omits.
	deviceID := pgtest.UUID(t)
	d, err := st.UpsertDevice(ctx, store.Device{TenantID: tenant, DeviceID: deviceID, OS: "windows",
		OSVersion: "11", HardwareIdentityHash: "hw-live", Hostname: "LAPTOP-1", AgentVersion: "1.0.0", ResidencyRegion: "eastus"})
	if err != nil || d.ManagedState != "unknown" || d.EnrolledAt.IsZero() {
		t.Fatalf("UpsertDevice = %+v, %v", d, err)
	}
	if _, err := st.UpsertDevice(ctx, store.Device{TenantID: tenant, DeviceID: deviceID, OS: "windows", ManagedState: "managed"}); err != nil {
		t.Fatal(err)
	}
	found, err := st.FindDeviceByHardwareIdentity(ctx, tenant, "hw-live")
	if err != nil || found.DeviceID != deviceID || found.Hostname != "LAPTOP-1" || found.OSVersion != "11" || found.ManagedState != "managed" {
		t.Fatalf("FindDeviceByHardwareIdentity = %+v, %v", found, err)
	}
	if _, err := st.Device(ctx, tenant, pgtest.UUID(t)); !errors.Is(err, store.ErrDeviceUnknown) {
		t.Fatalf("unknown device: %v", err)
	}

	// Credentials: a rotation revokes the previous one in the same transaction.
	first, second := pgtest.UUID(t), pgtest.UUID(t)
	for _, id := range []string{first, second} {
		if err := st.IssueCredential(ctx, store.Credential{TenantID: tenant, CredentialID: id, DeviceID: deviceID,
			PublicKeyThumbprint: "thumb-" + id[:8], IssuedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	c1, err := st.DeviceCredential(ctx, tenant, first)
	if err != nil || c1.RevokedAt == nil {
		t.Fatalf("the rotated-away credential = %+v, %v", c1, err)
	}
	c2, err := st.DeviceCredential(ctx, tenant, second)
	if err != nil || c2.Active(now) != nil || c2.DeviceID != deviceID {
		t.Fatalf("the live credential = %+v, %v", c2, err)
	}

	// Health: a known collector is recorded; an unknown one writes nothing.
	if err := st.RecordHealth(ctx, tenant, deviceID, now, []store.CollectorState{{Collector: "egress_proxy", State: "healthy"}},
		store.DeviceHealth{AgentVersion: "1.0.1", CollectionMode: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHealth(ctx, tenant, deviceID, now.Add(time.Second), []store.CollectorState{{Collector: "nope", State: "healthy"}},
		store.DeviceHealth{}); !errors.Is(err, store.ErrUnknownCollector) {
		t.Fatalf("unknown collector: %v", err)
	}

	// Deployment keys: create, resolve, count, revoke once.
	keyID := pgtest.UUID(t)
	keyHash := fmt.Sprintf("sha256:%064x", now.UnixNano())
	audit := func(action string) store.AuditEntry {
		return store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com", Action: action,
			ObjectType: "deployment_key", ObjectID: keyID, OccurredAt: now}
	}
	if _, err := st.CreateDeploymentKey(ctx, store.DeploymentKey{KeyID: keyID, TenantID: tenant, KeyHash: keyHash,
		Label: "live", CreatedBy: "admin@example.com", CreatedAt: now}, audit("deployment_key.create")); err != nil {
		t.Fatal(err)
	}
	k, err := st.DeploymentKeyByHash(ctx, tenant, keyHash)
	if err != nil || k.KeyID != keyID {
		t.Fatalf("DeploymentKeyByHash = %+v, %v", k, err)
	}
	if err := st.RecordDeploymentEnrolment(ctx, tenant, keyID, now, audit("device.enrol")); err != nil {
		t.Fatal(err)
	}
	revoked, err := st.RevokeDeploymentKey(ctx, tenant, keyID, now, audit("deployment_key.revoke"))
	if err != nil || revoked.RevokedAt == nil || revoked.EnrolmentCount != 1 {
		t.Fatalf("RevokeDeploymentKey = %+v, %v", revoked, err)
	}
	again, err := st.RevokeDeploymentKey(ctx, tenant, keyID, now.Add(time.Hour), audit("deployment_key.revoke"))
	if err != nil || !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("second revoke = %+v, %v", again, err)
	}
	if _, err := st.RevokeDeploymentKey(ctx, tenant, pgtest.UUID(t), now, audit("x")); !errors.Is(err, store.ErrDeploymentKeyUnknown) {
		t.Fatalf("unknown key: %v", err)
	}

	// Intune binding is unique per tenant.
	other := pgtest.UUID(t)
	if _, err := st.UpsertDevice(ctx, store.Device{TenantID: tenant, DeviceID: other, OS: "macos"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeviceIntuneID(ctx, tenant, deviceID, "intune-live"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeviceIntuneID(ctx, tenant, other, "intune-live"); !errors.Is(err, store.ErrIntuneDeviceConflict) {
		t.Fatalf("second device claiming the Intune id: %v", err)
	}
	if d, err := st.FindDeviceByIntuneID(ctx, tenant, "intune-live"); err != nil || d.DeviceID != deviceID {
		t.Fatalf("FindDeviceByIntuneID = %+v, %v", d, err)
	}

	// Verification needs an active Entra connection.
	if err := st.SetDeviceVerification(ctx, tenant, store.VerificationIntune, audit("tenant.device_verification.set")); !errors.Is(err, store.ErrNoEntraConnection) {
		t.Fatalf("intune without a connection: %v", err)
	}
	if err := st.SetDeviceVerification(ctx, tenant, store.VerificationNone, audit("tenant.device_verification.set")); err != nil {
		t.Fatal(err)
	}
	sum, err := st.DeploymentSummary(ctx, tenant)
	if err != nil || sum.DeviceVerification != "none" || len(sum.Keys) != 1 || sum.DevicesEnrolled != 2 {
		t.Fatalf("DeploymentSummary = %+v, %v", sum, err)
	}

	// Policy: inputs read, and a bundle minted once under the lock.
	in, err := st.PolicyInputs(ctx, tenant)
	if err != nil || in.Tenant.CeilingMode != "m1" {
		t.Fatalf("PolicyInputs = %+v, %v", in, err)
	}
	if _, err := st.LatestPolicyBundle(ctx, tenant); !errors.Is(err, store.ErrNoPolicyBundle) {
		t.Fatalf("no bundle yet: %v", err)
	}
	envelope := []byte(`{"key_id":"policy-key-1","algorithm":"ed25519","payload":{},"signature":"AA=="}`)
	minted, err := st.MintPolicyBundle(ctx, tenant, func(latest *store.PolicyBundle) (store.MintDecision, error) {
		return store.MintDecision{Bundle: &store.PolicyBundle{
			Version: now.Unix(), ScopeMatrix: []byte(`{"tenant_default":"m1"}`), DestinationAllowlist: []byte(`[]`),
			RetentionClass: "standard", SignatureKID: "policy-key-1",
			SignedDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(envelope)), SignedEnvelope: envelope, EffectiveFrom: now, CreatedBy: "control-api",
		}}, nil
	})
	if err != nil {
		t.Fatalf("MintPolicyBundle: %v", err)
	}
	latest, err := st.LatestPolicyBundle(ctx, tenant)
	if err != nil || latest.Version != minted.Version || string(latest.SignedEnvelope) != string(envelope) {
		t.Fatalf("LatestPolicyBundle = %+v, %v", latest, err)
	}
}
