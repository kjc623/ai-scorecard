package onboard_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/onboard"
	"github.com/shadow-ai-capture/control-api/internal/pgtest"
)

// The live test for the vendor operator's commands: the statements prepared verbatim, then `tenant
// create` and `tenant invite` run as sac_control — the role the tenant-admin job connects as —
// under forced row-level security, against the database SAC_TEST_PG_DSN names. The tenants it
// creates stay behind, closed, because their audit rows are append-only.

func TestAdminStatementsPrepare(t *testing.T) {
	db := pgtest.Open(t)
	for i, s := range onboard.AdminStatements {
		name := fmt.Sprintf("sac_onboard_probe_%d", i)
		if _, err := db.Exec("PREPARE " + name + " AS " + s.SQL); err != nil {
			t.Errorf("statement %q does not prepare: %v", s.Name, err)
			continue
		}
		_, _ = db.Exec("DEALLOCATE " + name)
	}
}

func closeTenant(t *testing.T, db *sql.DB, tenant string) {
	t.Cleanup(func() {
		tx, err := db.Begin()
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		_, _ = tx.Exec(`SELECT set_config('app.tenant_id', $1, true)`, tenant)
		for _, q := range []string{
			`DELETE FROM ops.onboarding_invite WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.tenant_email_domain WHERE tenant_id = $1::uuid`,
			`UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
			        status_reason = 'onboard live test', status_changed_by = 'test', status_changed_at = now()
			  WHERE tenant_id = $1::uuid`,
		} {
			if _, err := tx.Exec(q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
				return
			}
		}
		_ = tx.Commit()
	})
}

func TestTenantCreateAndInviteAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	db := pgtest.OpenAs(t, "sac_control")
	ctx := context.Background()
	domain := fmt.Sprintf("onboard-live-%s.example", pgtest.UUID(t)[:8])

	first, err := onboard.CreateTenant(ctx, db, onboard.NewTenant{Name: "Live One", Region: "eu-west", CeilingMode: "m1", Actor: "op"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	closeTenant(t, owner, first)
	second, err := onboard.CreateTenant(ctx, db, onboard.NewTenant{Name: "Live Two", Region: "eu-west", CeilingMode: "m3", Actor: "op"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	closeTenant(t, owner, second)

	inv, err := onboard.CreateInvite(ctx, db, onboard.NewInvite{TenantID: first, Domains: []string{strings.ToUpper(domain)}, PublicURL: "https://app.example.test", Actor: "op"})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	token := strings.TrimPrefix(inv.URL, "https://app.example.test/onboard/")
	if tenant, err := identity.ParseInviteToken(token); err != nil || tenant != first {
		t.Fatalf("invite URL %s names %q, %v", inv.URL, tenant, err)
	}
	// The same domain again for the same tenant is fine; for another tenant it is refused.
	if _, err := onboard.CreateInvite(ctx, db, onboard.NewInvite{TenantID: first, Domains: []string{domain}, PublicURL: "https://app.example.test", Actor: "op"}); err != nil {
		t.Fatalf("second invite: %v", err)
	}
	if _, err := onboard.CreateInvite(ctx, db, onboard.NewInvite{TenantID: second, Domains: []string{domain}, PublicURL: "https://app.example.test", Actor: "op"}); err == nil ||
		!strings.Contains(err.Error(), "another tenant") {
		t.Fatalf("a domain was given to a second tenant: %v", err)
	}
	st := identity.NewSQL(db)
	if got, err := st.TenantForEmailDomain(ctx, domain); err != nil || got != first {
		t.Fatalf("domain -> %q, %v", got, err)
	}
	if got, err := st.Invite(ctx, first, identity.HashToken(token)); err != nil || got.ID != inv.InviteID || got.CreatedBy != "vendor:op" {
		t.Fatalf("stored invite = %+v, %v", got, err)
	}
	var actions []string
	rows, err := owner.Query(`SELECT action || '/' || actor_id FROM ops.audit WHERE tenant_id = $1::uuid ORDER BY audit_seq`, first)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	want := "tenant.create/vendor:op,tenant_email_domain.add/vendor:op,onboarding_invite.create/vendor:op,onboarding_invite.create/vendor:op"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit = %v", actions)
	}
	if _, err := onboard.CreateTenant(ctx, db, onboard.NewTenant{Name: "x", Region: "eu-west", CeilingMode: "m4", Actor: "op"}); err == nil {
		t.Fatal("a tenant with an unknown ceiling was created")
	}
}
