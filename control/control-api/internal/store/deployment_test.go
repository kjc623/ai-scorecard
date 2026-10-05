package store

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

const (
	tenant1 = "5a3c0de0-7e57-4a11-9000-0000000d3a01"
	device1 = "22222222-2222-4222-8222-222222222222"
	device2 = "33333333-3333-4333-8333-333333333333"
)

// TestTenantStatementsFilterOnTheTenant: every statement over a tenant table names the tenant in
// its WHERE (or VALUES) as $1, so it is scoped twice -- by RLS and by its own predicate -- and a
// session left with the wrong tenant still reads nothing it should not.
func TestTenantStatementsFilterOnTheTenant(t *testing.T) {
	global := map[string]bool{"interception_hosts": true, "servable_classifier_release": true, "lock_tenant_policy": true}
	scoped := regexp.MustCompile(`tenant_id = \$1::uuid|VALUES \(\$1::uuid|\(\$1::uuid, \$2::uuid`)
	for _, s := range DeploymentStatements {
		if global[s.Name] {
			if regexp.MustCompile(`(FROM|INTO|UPDATE|JOIN)\s+ops\.`).MatchString(s.SQL) {
				t.Errorf("%s reads an ops table but is listed as global", s.Name)
			}
			continue
		}
		if !scoped.MatchString(s.SQL) {
			t.Errorf("%s does not scope itself to the tenant in $1:\n%s", s.Name, s.SQL)
		}
	}
}

func TestDeploymentKeyLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Second), now.Add(time.Hour)
	for _, c := range []struct {
		k    DeploymentKey
		want error
	}{
		{DeploymentKey{}, nil},
		{DeploymentKey{ExpiresAt: &future}, nil},
		{DeploymentKey{ExpiresAt: &past}, ErrDeploymentKeyExpired},
		{DeploymentKey{ExpiresAt: &now}, ErrDeploymentKeyExpired},
		{DeploymentKey{RevokedAt: &past, ExpiresAt: &past}, ErrDeploymentKeyRevoked},
	} {
		if got := c.k.Usable(now); !errors.Is(got, c.want) {
			t.Errorf("Usable(%+v) = %v, want %v", c.k, got, c.want)
		}
	}
}

func TestMemoryRevocationFirstStands(t *testing.T) {
	m := NewMemory()
	m.AddTenant(Tenant{TenantID: tenant1, Status: "active", IngestEnabled: true})
	ctx := context.Background()
	t0 := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	k, err := m.CreateDeploymentKey(ctx, DeploymentKey{KeyID: device1, TenantID: tenant1, KeyHash: "sha256:00", CreatedAt: t0},
		AuditEntry{TenantID: tenant1, Action: "deployment_key.create"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RevokeDeploymentKey(ctx, tenant1, k.KeyID, t0.Add(time.Hour), AuditEntry{Action: "revoke-1"}); err != nil {
		t.Fatal(err)
	}
	again, err := m.RevokeDeploymentKey(ctx, tenant1, k.KeyID, t0.Add(2*time.Hour), AuditEntry{Action: "revoke-2"})
	if err != nil || !again.RevokedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("second revoke = %+v, %v", again, err)
	}
	if n := len(m.Audits()); n != 2 {
		t.Fatalf("audits = %d, want create and one revoke", n)
	}
	if _, err := m.RevokeDeploymentKey(ctx, tenant1, device2, t0, AuditEntry{}); !errors.Is(err, ErrDeploymentKeyUnknown) {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestMemoryIntuneIDIsUniquePerTenant(t *testing.T) {
	m := NewMemory()
	m.AddTenant(Tenant{TenantID: tenant1, Status: "active", IngestEnabled: true})
	m.AddDevice(Device{TenantID: tenant1, DeviceID: device1, OS: "windows"})
	m.AddDevice(Device{TenantID: tenant1, DeviceID: device2, OS: "windows"})
	ctx := context.Background()
	if err := m.SetDeviceIntuneID(ctx, tenant1, device1, "intune-a"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetDeviceIntuneID(ctx, tenant1, device2, "intune-a"); !errors.Is(err, ErrIntuneDeviceConflict) {
		t.Fatalf("second device claiming the id: %v", err)
	}
	// An upsert of the device (as every enrolment does) keeps the binding, as the SQL upsert never
	// touches the column.
	d, _ := m.Device(ctx, tenant1, device1)
	d.IntuneDeviceID = ""
	if _, err := m.UpsertDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, err := m.FindDeviceByIntuneID(ctx, tenant1, "intune-a"); err != nil || got.DeviceID != device1 {
		t.Fatalf("binding after upsert = %+v, %v", got, err)
	}
}

func TestMemoryVerificationNeedsAnActiveEntraConnection(t *testing.T) {
	m := NewMemory()
	m.AddTenant(Tenant{TenantID: tenant1, Status: "active", IngestEnabled: true})
	ctx := context.Background()
	m.AddIdentityConnection(tenant1, IdentityConnection{Provider: "entra", Status: "pending", EntraTenantID: "x"})
	if err := m.SetDeviceVerification(ctx, tenant1, VerificationIntune, AuditEntry{}); !errors.Is(err, ErrNoEntraConnection) {
		t.Fatalf("pending connection: %v", err)
	}
	m.AddIdentityConnection(tenant1, IdentityConnection{Provider: "entra", Status: "active", EntraTenantID: "tid"})
	if err := m.SetDeviceVerification(ctx, tenant1, VerificationIntune, AuditEntry{}); err != nil {
		t.Fatal(err)
	}
	v, _ := m.DeviceVerification(ctx, tenant1)
	if v.Mode != VerificationIntune || v.EntraTenantID != "tid" {
		t.Fatalf("verification = %+v", v)
	}
	if _, err := m.DeviceVerification(ctx, device1); !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func TestMemoryMintRefusesAVersionThatDoesNotIncrease(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	mint := func(v int64) error {
		_, err := m.MintPolicyBundle(ctx, tenant1, func(*PolicyBundle) (MintDecision, error) {
			return MintDecision{Bundle: &PolicyBundle{Version: v, SignedEnvelope: []byte("{}")}}, nil
		})
		return err
	}
	if err := mint(5); err != nil {
		t.Fatal(err)
	}
	if err := mint(5); err == nil {
		t.Fatal("a repeated version was accepted; the primary key would refuse it")
	}
	if _, err := m.MintPolicyBundle(ctx, "99999999-9999-4999-8999-999999999999", func(*PolicyBundle) (MintDecision, error) {
		return MintDecision{}, nil
	}); !errors.Is(err, ErrNoPolicyBundle) {
		t.Fatalf("keep with nothing stored: %v", err)
	}
}
