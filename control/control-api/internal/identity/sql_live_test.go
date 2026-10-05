//go:build sac_sql_driver

package identity_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/sqlpg"
)

// The tagged live test: every identity statement prepared verbatim, then the SQL store driven through
// the real driver against a PostgreSQL with database/schema.sql applied — the definer functions,
// forced row-level security, the cross-tenant uniqueness of an Entra tenant id, single-use attempts
// and invites, and the audit rows activation commits. It SKIPS loudly when no server is reachable.
//
// Point it at a throwaway database, not the lab's: it seeds two tenants of its own and removes what
// it can, but ops.audit is append-only and references them, so both stay behind, closed.
// SAC_PG_DSN (then PG*, then localhost) seeds as an owner; SAC_PG_STORE_DSN, when set, is the
// connection the store uses, so it can run as a member of sac_control.

func liveDSN(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if name == "SAC_PG_STORE_DSN" {
		return liveDSN("SAC_PG_DSN")
	}
	if host := os.Getenv("PGHOST"); host != "" {
		user := os.Getenv("PGUSER")
		if user == "" {
			user = "postgres"
		}
		return fmt.Sprintf("postgres://%s:%s@%s:5432/shadow?sslmode=disable", user, os.Getenv("PGPASSWORD"), host)
	}
	return "postgres://postgres:sac-lab-only@127.0.0.1:5432/shadow?sslmode=disable"
}

func openLive(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sqlpg.OpenDB(liveDSN(name))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		t.Skipf("SKIPPING (not a failure): no PostgreSQL reachable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestIdentityStatementsPrepare(t *testing.T) {
	db := openLive(t, "SAC_PG_DSN")
	for i, s := range identity.Statements {
		name := fmt.Sprintf("sac_identity_probe_%d", i)
		if _, err := db.Exec("PREPARE " + name + " AS " + s.SQL); err != nil {
			t.Errorf("statement %q does not prepare: %v", s.Name, err)
			continue
		}
		_, _ = db.Exec("DEALLOCATE " + name)
	}
}

// ownerTx runs seed statements with the tenant set, so an owner under forced RLS can seed too.
func ownerTx(t *testing.T, db *sql.DB, tenant string, stmts ...func(*sql.Tx) error) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := s(tx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func exec(q string, args ...any) func(*sql.Tx) error {
	return func(tx *sql.Tx) error { _, err := tx.Exec(q, args...); return err }
}

func seedTenant(t *testing.T, db *sql.DB, suffix int64, label string) string {
	tenant := fmt.Sprintf("00000000-0000-4000-80%02d-%012d", 7+len(label)%2, suffix)
	ownerTx(t, db, tenant, exec(`INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
	  VALUES ($1::uuid, $2, 'active', 'eu-west', 'vendor', 'm1')`, tenant, "identity-live-"+label))
	t.Cleanup(func() {
		tx, err := db.Begin()
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		_, _ = tx.Exec(`SELECT set_config('app.tenant_id', $1, true)`, tenant)
		for _, q := range []string{
			`DELETE FROM ops.auth_signin WHERE invite_id IN (SELECT invite_id FROM ops.onboarding_invite WHERE tenant_id = $1::uuid)
			    OR connection_id IN (SELECT connection_id FROM ops.identity_connection WHERE tenant_id = $1::uuid)`,
			`DELETE FROM ops.role_grant WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.auth_session WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.identity_connection WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.onboarding_invite WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.tenant_email_domain WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.user_ref_alias WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.scim_user WHERE tenant_id = $1::uuid`,
			`UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
			        status_reason = 'identity live test', status_changed_by = 'test', status_changed_at = now()
			  WHERE tenant_id = $1::uuid`,
		} {
			if _, err := tx.Exec(q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
				return
			}
		}
		_ = tx.Commit()
	})
	return tenant
}

func TestIdentitySQLStoreAgainstPostgres(t *testing.T) {
	owner := openLive(t, "SAC_PG_DSN")
	st := identity.NewSQL(openLive(t, "SAC_PG_STORE_DSN"))
	ctx := context.Background()
	n := time.Now().UnixNano() % 1_000_000_000_000
	tenant := seedTenant(t, owner, n, "a")
	other := seedTenant(t, owner, n, "bb")
	tid := fmt.Sprintf("00000000-0000-4000-8009-%012d", n)
	domain := fmt.Sprintf("live-%d.example", n)
	now := time.Now().UTC()

	// Email domain discovery through the definer function.
	if _, err := st.TenantForEmailDomain(ctx, domain); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown domain: %v", err)
	}
	token, _ := identity.MintInviteToken(tenant)
	var inviteID string
	ownerTx(t, owner, tenant,
		exec(`INSERT INTO ops.tenant_email_domain (domain, tenant_id, created_by) VALUES ($1, $2::uuid, 'test')`, domain, tenant),
		func(tx *sql.Tx) error {
			return tx.QueryRow(`INSERT INTO ops.onboarding_invite (tenant_id, token_hash, created_by, expires_at)
			  VALUES ($1::uuid, $2, 'vendor:test', now() + interval '1 hour') RETURNING invite_id::text`,
				tenant, identity.HashToken(token)).Scan(&inviteID)
		})
	if got, err := st.TenantForEmailDomain(ctx, strings.ToUpper(domain)); err != nil || got != tenant {
		t.Fatalf("domain -> %q, %v", got, err)
	}
	if inv, err := st.Invite(ctx, tenant, identity.HashToken(token)); err != nil || inv.ID != inviteID || inv.UsedAt != nil {
		t.Fatalf("Invite = %+v, %v", inv, err)
	}
	if _, err := st.Invite(ctx, other, identity.HashToken(token)); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("an invite was visible from another tenant: %v", err)
	}
	if sum, err := st.TenantSummary(ctx, tenant); err != nil || sum.Name != "identity-live-a" || len(sum.Domains) != 1 {
		t.Fatalf("TenantSummary = %+v, %v", sum, err)
	}

	// A pending Entra connection: reused for the same tid, refused for another tenant, invisible to
	// the active-only definer lookup until activation.
	audit := identity.AuditEntry{ActorType: "system", ActorID: "onboarding-invite:" + inviteID, Action: "identity_connection.create", ObjectType: "identity_connection"}
	conn, err := st.CreatePendingConnection(ctx, identity.NewConnection{TenantID: tenant, Provider: identity.ProviderEntra, EntraTenantID: tid}, audit)
	if err != nil || conn.Status != identity.StatusPending || conn.EntraTenantID != tid {
		t.Fatalf("CreatePendingConnection = %+v, %v", conn, err)
	}
	if again, err := st.CreatePendingConnection(ctx, identity.NewConnection{TenantID: tenant, Provider: identity.ProviderEntra, EntraTenantID: tid}, audit); err != nil || again.ID != conn.ID {
		t.Fatalf("re-consent = %+v, %v", again, err)
	}
	if _, err := st.CreatePendingConnection(ctx, identity.NewConnection{TenantID: other, Provider: identity.ProviderEntra, EntraTenantID: tid}, audit); !errors.Is(err, identity.ErrLinkedElsewhere) {
		t.Fatalf("another tenant claimed the tid: %v", err)
	}
	if _, err := st.ConnectionForEntraTenant(ctx, tid); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("a pending connection resolved by tid: %v", err)
	}
	if c, err := st.ConnectionByID(ctx, conn.ID); err != nil || c.Status != identity.StatusPending || c.RolesClaim != "roles" {
		t.Fatalf("ConnectionByID = %+v, %v", c, err)
	}

	// Attempts: single use, swept when expired.
	hash := make([]byte, 32)
	_, _ = rand.Read(hash)
	a := identity.Attempt{Hash: hash, ConnectionID: conn.ID, State: "s", Nonce: "n", VerifierEnc: []byte{1}, RedirectURI: "https://app/callback",
		InviteID: inviteID, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	if err := st.PutAttempt(ctx, a); err != nil {
		t.Fatalf("PutAttempt: %v", err)
	}
	if got, err := st.TakeAttempt(ctx, hash); err != nil || got.ConnectionID != conn.ID || got.InviteID != inviteID || got.Nonce != "n" {
		t.Fatalf("TakeAttempt = %+v, %v", got, err)
	}
	if _, err := st.TakeAttempt(ctx, hash); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("an attempt was taken twice: %v", err)
	}
	if err := st.SweepAttempts(ctx, now); err != nil {
		t.Fatalf("SweepAttempts: %v", err)
	}

	// Activation: once, then the invite is spent.
	act := identity.Activation{TenantID: tenant, ConnectionID: conn.ID, InviteID: inviteID, Subject: "oid-live", Actor: "first@" + domain, At: now}
	if err := st.Activate(ctx, act); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := st.Activate(ctx, act); !errors.Is(err, identity.ErrInviteUsed) {
		t.Fatalf("second Activate: %v", err)
	}
	if c, err := st.ConnectionForEntraTenant(ctx, strings.ToUpper(tid)); err != nil || c.ID != conn.ID || c.ActivatedBy != act.Actor {
		t.Fatalf("active connection by tid = %+v, %v", c, err)
	}
	if roles, err := st.RoleGrants(ctx, tenant, conn.ID, "oid-live"); err != nil || len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("RoleGrants = %v, %v", roles, err)
	}
	var actions []string
	rows, err := owner.Query(`SELECT action FROM ops.audit WHERE tenant_id = $1::uuid ORDER BY audit_seq`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	if strings.Join(actions, ",") != "identity_connection.create,onboarding_invite.use,identity_connection.activate,role.grant" {
		t.Fatalf("audit = %v", actions)
	}

	// An OIDC pending connection takes a re-entered client.
	issuer := "https://" + domain + "/oauth2"
	oc, err := st.CreatePendingConnection(ctx, identity.NewConnection{TenantID: other, Provider: identity.ProviderOIDC, Issuer: issuer, ClientID: "c1", ClientSecretEnc: []byte{7}}, audit)
	if err != nil {
		t.Fatalf("oidc pending: %v", err)
	}
	if oc2, err := st.CreatePendingConnection(ctx, identity.NewConnection{TenantID: other, Provider: identity.ProviderOIDC, Issuer: issuer, ClientID: "c2", ClientSecretEnc: []byte{8}}, audit); err != nil || oc2.ID != oc.ID || oc2.ClientID != "c2" {
		t.Fatalf("re-entered client = %+v, %v", oc2, err)
	}
	if conns, err := st.TenantConnections(ctx, other); err != nil || len(conns) != 1 || conns[0].ClientID != "c2" {
		t.Fatalf("TenantConnections = %+v, %v", conns, err)
	}

	// SCIM linkage through an alias, and the tenant's user_ref key.
	if key, err := st.UserRefKey(ctx, tenant); err != nil || key != nil {
		t.Fatalf("UserRefKey before minting = %v, %v", key, err)
	}
	canonical, alias := "u_"+strings.Repeat("ab", 16), "u_"+strings.Repeat("cd", 16)
	nameHash := make([]byte, 32)
	_, _ = rand.Read(nameHash)
	ownerTx(t, owner, tenant,
		exec(`UPDATE ops.tenant SET user_ref_key_enc = '\x0102'::bytea WHERE tenant_id = $1::uuid`, tenant),
		exec(`INSERT INTO ops.scim_user (tenant_id, user_name_hash, resource_enc, user_ref, active) VALUES ($1::uuid, $2, '\x00'::bytea, $3, false)`, tenant, nameHash, canonical),
		exec(`INSERT INTO ops.user_ref_alias (tenant_id, alias_ref, user_ref) VALUES ($1::uuid, $2, $3)`, tenant, alias, canonical))
	if key, err := st.UserRefKey(ctx, tenant); err != nil || len(key) != 2 {
		t.Fatalf("UserRefKey = %v, %v", key, err)
	}
	if ref, active, err := st.ScimUser(ctx, tenant, []string{"u_" + strings.Repeat("ef", 16), alias}); err != nil || ref != canonical || active {
		t.Fatalf("ScimUser = %q %v %v", ref, active, err)
	}
	if _, _, err := st.ScimUser(ctx, other, []string{alias}); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("a SCIM user was visible from another tenant: %v", err)
	}
}
