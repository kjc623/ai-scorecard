//go:build sac_sql_driver

package scim

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/sqlpg"
)

// The tagged live test: the SCIM provider end to end, over HTTP, against a PostgreSQL with
// database/schema.sql applied, through the real RLS sessions, definer function, constraints and audit
// trigger. It SKIPS loudly when no server is reachable.
//
// Point it at a throwaway database, not the lab's: it seeds its own tenant and removes what it can,
// but ops.audit is append-only and references the tenant, so the tenant row stays behind, closed,
// named scim-live-<n>. The DSN follows the directory live test: SAC_PG_DSN, then the PG* variables,
// then a localhost default. SAC_PG_STORE_DSN, when set, is the connection the provider itself uses,
// so it can run as sac_control under forced RLS and its grants while the seeding runs as an owner.

func liveDSN() string {
	if v := os.Getenv("SAC_PG_DSN"); v != "" {
		return v
	}
	if host := os.Getenv("PGHOST"); host != "" {
		user := os.Getenv("PGUSER")
		if user == "" {
			user = "postgres"
		}
		db := os.Getenv("PGDATABASE")
		if db == "" {
			db = "shadow"
		}
		return fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", user, os.Getenv("PGPASSWORD"), host, db)
	}
	return "postgres://postgres:sac-lab-only@127.0.0.1:5432/shadow?sslmode=disable"
}

func openLive(t *testing.T) *sql.DB {
	t.Helper()
	return openDSN(t, liveDSN())
}

func openDSN(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sqlpg.OpenDB(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("SKIPPING (not a failure): no PostgreSQL reachable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSCIMSQLStatementsPrepare(t *testing.T) {
	db := openLive(t)
	ctx := context.Background()
	for i, s := range Statements {
		name := fmt.Sprintf("sac_scim_probe_%d", i)
		if _, err := db.ExecContext(ctx, "PREPARE "+name+" AS "+s.SQL); err != nil {
			t.Errorf("statement %q does not prepare: %v", s.Name, err)
			continue
		}
		_, _ = db.ExecContext(ctx, "DEALLOCATE "+name)
	}
}

func seedLiveTenant(t *testing.T, db *sql.DB, identity string) string {
	t.Helper()
	ctx := context.Background()
	tenant := fmt.Sprintf("00000000-0000-4000-8002-%012d", time.Now().UnixNano()%1_000_000_000_000)
	if _, err := db.ExecContext(ctx, `INSERT INTO ops.tenant
	  (tenant_id, name, status, residency_region, key_custody, ceiling_mode, content_search, device_identity)
	  VALUES ($1::uuid, $2, 'active', 'eu-west', 'vendor', 'm1', 'disabled', $3)`, tenant, "scim-live-"+tenant[24:], identity); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM ops.scim_group_member WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.scim_group WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.scim_user WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.user_ref_alias WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.user_dim WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.scim_token WHERE tenant_id = $1::uuid`,
			`UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
			        status_reason = 'scim live test', status_changed_by = 'test', status_changed_at = now()
			  WHERE tenant_id = $1::uuid`,
		} {
			if _, err := db.ExecContext(ctx, q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	return tenant
}

func TestSCIMAgainstPostgres(t *testing.T) {
	db := openLive(t)
	ctx := context.Background()
	tenant := seedLiveTenant(t, db, "clear")
	other := seedLiveTenant(t, db, "clear")

	storeDB := db
	if v := os.Getenv("SAC_PG_STORE_DSN"); v != "" {
		storeDB = openDSN(t, v)
	}
	cipher, _ := directory.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	keys, _ := directory.NewUserRefKeys(directory.NewSQL(storeDB), cipher)
	st := NewSQL(storeDB)
	svc, err := NewService(st, keys, cipher, Config{BaseURL: "https://live.example.test/scim/v2"})
	if err != nil {
		t.Fatal(err)
	}
	svc.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens := NewTokens(st)
	tokenID, tok, err := tokens.Create(ctx, tenant, "live", "admin@live.test")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	_, otherTok, err := tokens.Create(ctx, other, "live", "admin@live.test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(svc, "/scim/v2", slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()
	h := &harness{t: t, srv: srv}

	h.mustDo(200, "GET", filterPath("Users", `userName eq "nobody"`), tok, nil)
	id := str(h.mustDo(201, "POST", "/scim/v2/Users", tok, entraCreateUser)["id"])
	h.mustDo(409, "POST", "/scim/v2/Users", tok, entraCreateUser)
	if l := h.mustDo(200, "GET", filterPath("Users", `externalId eq "`+vectorOIDID+`"`), tok, nil); total(l) != 1 {
		t.Fatalf("externalId lookup = %v", l)
	}
	h.mustDo(200, "PATCH", "/scim/v2/Users/"+id, tok, `{"Operations":[
	  {"op":"Replace","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"Research"},
	  {"op":"Replace","path":"userName","value":"ada.king@contoso.com"}]}`)

	var canonical string
	var active bool
	if err := db.QueryRowContext(ctx, `SELECT user_ref, active FROM ops.scim_user WHERE tenant_id = $1::uuid AND scim_id = $2::uuid`, tenant, id).Scan(&canonical, &active); err != nil {
		t.Fatal(err)
	}
	var aliases int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ops.user_ref_alias WHERE tenant_id = $1::uuid AND user_ref = $2`, tenant, canonical).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 3 {
		t.Fatalf("aliases = %d, want the original upn-ref, the renamed upn-ref and the oid-ref", aliases)
	}
	var dept, status string
	if err := db.QueryRowContext(ctx, `SELECT department, status FROM ops.user_dim WHERE tenant_id = $1::uuid AND user_ref = $2`, tenant, canonical).Scan(&dept, &status); err != nil {
		t.Fatal(err)
	}
	if dept != "Research" || status != "active" {
		t.Fatalf("user_dim = %s / %s", dept, status)
	}

	// Groups and membership.
	gid := str(h.mustDo(201, "POST", "/scim/v2/Groups", tok, map[string]any{"displayName": "Analysts", "members": []any{map[string]any{"value": id}}})["id"])
	if code, out := h.do("PATCH", "/scim/v2/Groups/"+gid, tok, `{"Operations":[{"op":"Remove","path":"members","value":[{"value":"`+id+`"}]}]}`); code != 204 {
		t.Fatalf("group patch = %d %v", code, out)
	}
	if code, _ := h.do("DELETE", "/scim/v2/Groups/"+gid, tok, nil); code != 204 {
		t.Fatal("group delete")
	}

	// Isolation through the real RLS session: the other tenant's token sees nothing of this one.
	h.mustDo(404, "GET", "/scim/v2/Users/"+id, otherTok, nil)
	forged := TokenPrefix + other + "." + strings.SplitN(strings.TrimPrefix(tok, TokenPrefix), ".", 2)[1]
	h.mustDo(401, "GET", "/scim/v2/Users", forged, nil)

	// Retire.
	if code, _ := h.do("DELETE", "/scim/v2/Users/"+id, tok, nil); code != 204 {
		t.Fatal("user delete")
	}
	if err := db.QueryRowContext(ctx, `SELECT active FROM ops.scim_user WHERE tenant_id = $1::uuid AND scim_id = $2::uuid`, tenant, id).Scan(&active); err != nil || active {
		t.Fatalf("retired user active = %v, %v", active, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM ops.user_dim WHERE tenant_id = $1::uuid AND user_ref = $2`, tenant, canonical).Scan(&status); err != nil || status != "inactive" {
		t.Fatalf("retired user_dim status = %s, %v", status, err)
	}

	var audits int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ops.audit WHERE tenant_id = $1::uuid AND actor_id = $2`, tenant, "scim:"+tokenID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits < 6 {
		t.Fatalf("audit rows by the token = %d", audits)
	}

	if list, err := tokens.List(ctx, tenant); err != nil || len(list) != 1 || list[0].TokenID != tokenID || list[0].CreatedBy != "admin@live.test" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := tokens.Revoke(ctx, tenant, tokenID, "admin@live.test"); err != nil {
		t.Fatal(err)
	}
	h.mustDo(401, "GET", "/scim/v2/Users", tok, nil)
}
