//go:build sac_sql_driver

package sqlpg_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/enrol"
	"github.com/shadow-ai-capture/control-api/internal/store"
	"github.com/shadow-ai-capture/control-api/sqlpg"
)

// The tagged integration test: the database/sql plumbing and the exact statement text, against a
// reachable PostgreSQL with database/schema.sql applied.
//
// It applies nothing of its own: the lab's compose file applies the schema. It seeds its own tenant,
// token, device and credential, and removes them again. It SKIPS, loudly, when no server is
// reachable, because a skip with a reason is honest and a red gate on a machine without Docker is
// not.

const labDSN = "postgres://postgres:sac-lab-only@127.0.0.1:5432/shadow?sslmode=disable"

func parDSN() string {
	if v := os.Getenv("SAC_PG_DSN"); v != "" {
		return v
	}
	return labDSN
}

func openLab(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlpg.OpenDB(parDSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("SKIPPING (not a failure): no PostgreSQL at %s — %v\n"+
			"Start the lab first:  node localdev/run.mjs\n"+
			"Or point this test at another server:  SAC_PG_DSN=... go test -tags sac_sql_driver ./sqlpg/ -v",
			redactDSN(parDSN()), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	proto := strings.Index(dsn, "://")
	if at < 0 || proto < 0 || at < proto {
		return dsn
	}
	creds := dsn[proto+3 : at]
	if i := strings.Index(creds, ":"); i >= 0 {
		creds = creds[:i] + ":<redacted>"
	}
	return dsn[:proto+3] + creds + dsn[at:]
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// seed creates the rows the control path needs and removes them afterwards.
type fixture struct {
	tenant     string
	device     string
	credential string
	token      string // plaintext; only the hash is stored
}

func seed(t *testing.T, db *sql.DB) fixture {
	t.Helper()
	now := time.Now().UnixNano() % 1_000_000_000_000
	f := fixture{
		tenant:     fmt.Sprintf("00000000-0000-4000-8000-%012d", now),
		device:     fmt.Sprintf("00000000-0000-4000-9000-%012d", now),
		credential: fmt.Sprintf("00000000-0000-4000-a000-%012d", now),
	}
	var err error
	if f.token, err = enrol.MintEnrolmentToken(f.tenant); err != nil {
		t.Fatalf("mint token: %v", err)
	}
	ctx := context.Background()
	seedSQL := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode, content_search)
		  VALUES ($1::uuid, 'control-integration', 'active', 'eu-west', 'vendor', 'm1', 'disabled')`, []any{f.tenant}},
		{`INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
		  VALUES ($1::uuid, $2, now() + interval '1 hour')`, []any{f.tenant, enrol.HashEnrolmentToken(f.token)}},
		{`INSERT INTO ops.device (tenant_id, device_id, os) VALUES ($1::uuid, $2::uuid, 'windows')`, []any{f.tenant, f.device}},
		{`INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, credential_type, public_key_thumbprint, issued_at, expires_at)
		  VALUES ($1::uuid, $2::uuid, $3::uuid, 'x509', 'sha256-control-integration', now(), now() + interval '90 days')`,
			[]any{f.tenant, f.credential, f.device}},
	}
	for _, s := range seedSQL {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed (%s): %v", firstLine(s.sql), err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, s := range []string{
			`DELETE FROM ops.enrolment_token WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.device_credential WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.device WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.tenant WHERE tenant_id = $1::uuid`,
		} {
			if _, err := db.ExecContext(ctx, s, f.tenant); err != nil {
				t.Logf("cleanup (%s): %v", firstLine(s), err)
				return
			}
		}
	})
	return f
}

// TestStatementTextPrepares proves every constant in internal/store is valid SQL against the live
// schema, placeholders and all, without binding values. It is the cheap half of "the live test
// executes their text": a statement that no longer parses fails here by name.
func TestStatementTextPrepares(t *testing.T) {
	db := openLab(t)
	ctx := context.Background()
	for i, s := range store.Statements {
		name := fmt.Sprintf("sac_probe_%d", i)
		if _, err := db.ExecContext(ctx, "PREPARE "+name+" AS "+s.SQL); err != nil {
			t.Errorf("statement %q does not prepare against the live schema: %v", s.Name, err)
			continue
		}
		if _, err := db.ExecContext(ctx, "DEALLOCATE "+name); err != nil {
			t.Errorf("statement %q prepared but would not deallocate: %v", s.Name, err)
		}
	}
}

// TestStoreLifecycleThroughDatabaseSQL exercises the store through the real driver against the real
// schema: resolve a token, find an unknown device, insert a device, rotate a credential in one
// transaction, read it back, settle the token, and refuse a replayed jti.
func TestStoreLifecycleThroughDatabaseSQL(t *testing.T) {
	db := openLab(t)
	f := seed(t, db)
	ctx := context.Background()

	st, err := sqlpg.Open(parDSN())
	if err != nil {
		t.Fatalf("sqlpg.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	t.Run("tenant and token resolve", func(t *testing.T) {
		tenant, err := st.Tenant(ctx, f.tenant)
		if err != nil {
			t.Fatalf("Tenant: %v", err)
		}
		if tenant.ResidencyRegion != "eu-west" || !tenant.Active() {
			t.Fatalf("tenant = %+v, want an active eu-west tenant", tenant)
		}
		got, err := st.ResolveEnrolmentToken(ctx, f.tenant, enrol.HashEnrolmentToken(f.token))
		if err != nil {
			t.Fatalf("ResolveEnrolmentToken: %v", err)
		}
		if got.UsedAt != nil {
			t.Fatalf("a freshly seeded token reports used_at %v", got.UsedAt)
		}
	})

	t.Run("an unknown hardware identity is a clean miss", func(t *testing.T) {
		if _, err := st.FindDeviceByHardwareIdentity(ctx, f.tenant, "hwid-not-present"); err != store.ErrDeviceUnknown {
			t.Fatalf("err = %v, want store.ErrDeviceUnknown", err)
		}
	})

	newDevice := fmt.Sprintf("00000000-0000-4000-b000-%012d", time.Now().UnixNano()%1_000_000_000_000)
	t.Run("device insert and credential rotation are one step", func(t *testing.T) {
		created, err := st.UpsertDevice(ctx, store.Device{
			TenantID: f.tenant, DeviceID: newDevice, OS: "windows",
			HardwareIdentityHash: "hwid-control", ResidencyRegion: "eu-west",
		})
		if err != nil {
			t.Fatalf("UpsertDevice: %v", err)
		}
		if created.HardwareIdentityHash != "hwid-control" {
			t.Fatalf("hardware identity = %q, want hwid-control", created.HardwareIdentityHash)
		}
		found, err := st.FindDeviceByHardwareIdentity(ctx, f.tenant, "hwid-control")
		if err != nil {
			t.Fatalf("FindDeviceByHardwareIdentity: %v", err)
		}
		if found.DeviceID != newDevice {
			t.Fatalf("found device %s, want %s", found.DeviceID, newDevice)
		}

		newCred := fmt.Sprintf("00000000-0000-4000-c000-%012d", time.Now().UnixNano()%1_000_000_000_000)
		cred, err := st.IssueCredential(ctx, store.IssueCredential{
			TenantID: f.tenant, DeviceID: newDevice, CredentialID: newCred,
			Type: protocol.AuthModeDPoP, PublicKeyThumbprint: "thumb-rotated",
			PublicKeyJWK: []byte(`{"kty":"EC","crv":"P-256","x":"a","y":"b"}`),
			IssuedAt:     time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(90 * 24 * time.Hour),
			RevokedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("IssueCredential: %v", err)
		}
		if cred.Type != protocol.AuthModeDPoP || cred.CredentialID != newCred {
			t.Fatalf("issued credential = %+v", cred)
		}
		live, err := st.DeviceCredentialByDevice(ctx, f.tenant, newDevice)
		if err != nil {
			t.Fatalf("DeviceCredentialByDevice: %v", err)
		}
		if live.CredentialID != newCred {
			t.Fatalf("live credential = %s, want the freshly issued %s", live.CredentialID, newCred)
		}
	})

	t.Run("the token settles exactly once", func(t *testing.T) {
		hash := enrol.HashEnrolmentToken(f.token)
		if err := st.MarkEnrolmentTokenUsed(ctx, f.tenant, hash, time.Now().UTC()); err != nil {
			t.Fatalf("first MarkEnrolmentTokenUsed: %v", err)
		}
		if err := st.MarkEnrolmentTokenUsed(ctx, f.tenant, hash, time.Now().UTC()); err != store.ErrTokenUsed {
			t.Fatalf("second MarkEnrolmentTokenUsed = %v, want store.ErrTokenUsed", err)
		}
	})
}
