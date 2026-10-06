package store

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

// Every statement over a tenant table names the tenant as $1 in its own predicate, so it is scoped
// twice: by row-level security and by its WHERE or VALUES.
func TestTenantStatementsFilterOnTheTenant(t *testing.T) {
	global := map[string]bool{
		"set_tenant": true, "collector_vocabulary": true, "interception_hosts": true,
		"lock_tenant_policy": true, "retention_defaults": true,
	}
	scoped := regexp.MustCompile(`tenant_id = \$1::uuid|VALUES \(\$1::uuid|\(\$1::uuid, \$2::uuid`)
	ops := regexp.MustCompile(`(FROM|INTO|UPDATE|JOIN)\s+ops\.`)
	for _, s := range Statements {
		if global[s.Name] {
			if ops.MatchString(s.SQL) {
				t.Errorf("%s reads an ops table but is listed as global", s.Name)
			}
			continue
		}
		if !scoped.MatchString(s.SQL) {
			t.Errorf("%s does not scope itself to the tenant in $1:\n%s", s.Name, s.SQL)
		}
	}
}

func TestDeploymentKeyUsable(t *testing.T) {
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

func TestCredentialActive(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Hour)
	for _, c := range []struct {
		c    Credential
		want error
	}{
		{Credential{ExpiresAt: now.Add(time.Hour)}, nil},
		{Credential{ExpiresAt: now}, ErrCredentialExpired},
		{Credential{ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}, ErrCredentialRevoked},
	} {
		if got := c.c.Active(now); !errors.Is(got, c.want) {
			t.Errorf("Active(%+v) = %v, want %v", c.c, got, c.want)
		}
	}
}

func TestUUIDs(t *testing.T) {
	id, err := NewUUID()
	if err != nil || !IsUUID(id) || id[14] != '4' {
		t.Fatalf("NewUUID = %q, %v", id, err)
	}
	for _, bad := range []string{"", "not-a-uuid", "5a3c0de0-7e57-4a11-9000-0000000d3a0", "5a3c0de0x7e57-4a11-9000-0000000d3a01"} {
		if IsUUID(bad) {
			t.Errorf("IsUUID(%q) = true", bad)
		}
	}
}
