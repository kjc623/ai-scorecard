package directoryadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/directory"
	"github.com/shadow-ai-capture/control-api/internal/graphsync"
	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/scim"
)

// The live test: the directory pull and teams end to end, as sac_control against the database
// SAC_TEST_PG_DSN names, through the real RLS sessions, grants, constraints and ops.v_team_member.
// ops.audit is append-only and references the seeded tenant, so it stays behind, closed.

func TestStatementsPrepare(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	all := append(append([]string{}, Statements...), graphsync.Statements...)
	for i, s := range all {
		name := fmt.Sprintf("sac_directoryadmin_probe_%d", i)
		if _, err := db.ExecContext(ctx, "PREPARE "+name+" AS "+s); err != nil {
			t.Errorf("statement %d does not prepare: %v\n%s", i, err, s)
			continue
		}
		_, _ = db.ExecContext(ctx, "DEALLOCATE "+name)
	}
}

type liveDirectory struct {
	users   []graphsync.User
	members map[string][]string
}

func (d *liveDirectory) Users(_ context.Context, _ string, fn func(graphsync.User) error) error {
	for _, u := range d.users {
		if err := fn(u); err != nil {
			return err
		}
	}
	return nil
}

func (d *liveDirectory) GroupMembers(_ context.Context, _, id string) ([]string, error) {
	return d.members[id], nil
}

func (d *liveDirectory) Group(_ context.Context, _, id string) (graphsync.Group, error) {
	return graphsync.Group{ID: id, DisplayName: "Sales (Entra)"}, nil
}

const (
	oidAda   = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	oidGrace = "26118915-6090-4610-87a4-49d0767e9fde"
	oidGroup = "11111111-2222-4333-8444-555555555555"
)

func TestDirectoryPullAndTeamsAgainstPostgres(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	tenant := pgtest.UUID(t)
	entra := pgtest.UUID(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO ops.tenant
	  (tenant_id, name, status, residency_region, ceiling_mode, content_search, device_identity)
	  VALUES ($1::uuid, $2, 'active', 'eu-west', 'm1', 'disabled', 'clear')`, tenant, "dir-live-"+tenant[:8]); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM ops.team WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.directory_sync WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.scim_group_member WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.scim_group WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.scim_user WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.user_ref_alias WHERE tenant_id = $1::uuid`,
			`DELETE FROM ops.user_dim WHERE tenant_id = $1::uuid`,
			`UPDATE ops.identity_connection SET status = 'disabled' WHERE tenant_id = $1::uuid`,
			`UPDATE ops.tenant SET status = 'closed', ingest_enabled = false, read_enabled = false,
			        status_reason = 'directory live test', status_changed_by = 'test', status_changed_at = now()
			  WHERE tenant_id = $1::uuid`,
		} {
			if _, err := db.ExecContext(context.Background(), q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})

	control := pgtest.OpenAs(t, "sac_control")
	st := NewSQL(control)
	at := time.Now().UTC().Truncate(time.Second)
	admin := Audit{Actor: "admin@live.test", Action: "directory_sync.enable", ObjectType: "directory_sync", ObjectID: tenant, At: at}

	// Turning the pull on needs an active Entra connection.
	if err := st.SetSync(ctx, tenant, true, admin); !errors.Is(err, ErrNoEntra) {
		t.Fatalf("SetSync without Entra = %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ops.identity_connection (tenant_id, provider, entra_tenant_id, status, activated_at, activated_by)
	  VALUES ($1::uuid, 'entra', $2, 'active', now(), 'test')`, tenant, entra); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if err := st.SetSync(ctx, tenant, true, admin); err != nil {
		t.Fatalf("SetSync: %v", err)
	}

	cipher, _ := directory.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	keys, _ := directory.NewUserRefKeys(directory.NewKeyStore(control), cipher)
	svc, err := scim.NewService(scim.NewSQL(control), keys, cipher, scim.Config{})
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc.Logger = quiet
	dir := &liveDirectory{
		users: []graphsync.User{
			{ID: oidAda, UserPrincipalName: "ada@contoso.com", DisplayName: "Ada Lovelace", Department: "Engineering",
				OnPremisesDistinguishedName: "CN=Ada,OU=Research,OU=Labs,DC=contoso,DC=com"},
			{ID: oidGrace, UserPrincipalName: "grace@contoso.com", DisplayName: "Grace Hopper", Department: "Sales"},
		},
		members: map[string][]string{oidGroup: {oidGrace}},
	}
	syncStore := graphsync.NewSQL(control)
	runner, err := graphsync.NewRunner(&graphsync.Syncer{Directory: dir, Provisioner: svc, Logger: quiet}, syncStore, quiet)
	if err != nil {
		t.Fatal(err)
	}
	target, ok, err := syncStore.Target(ctx, tenant)
	if err != nil || !ok || target.EntraTenantID != entra {
		t.Fatalf("Target = %+v, %v, %v", target, ok, err)
	}
	runner.RunTenant(ctx, target)

	status, err := st.Status(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if status.Sync == nil || status.Sync.LastStatus != "ok" || status.People != 2 || *status.Sync.UsersSynced != 2 {
		t.Fatalf("status = %+v, sync %+v", status, status.Sync)
	}
	var orgUnit, dept string
	if err := db.QueryRowContext(ctx, `SELECT org_unit, department FROM ops.user_dim WHERE tenant_id = $1::uuid AND display_name = 'Ada Lovelace'`, tenant).Scan(&orgUnit, &dept); err != nil {
		t.Fatal(err)
	}
	if orgUnit != "OU=Research,OU=Labs,DC=contoso,DC=com" || dept != "Engineering" {
		t.Fatalf("user_dim = %s / %s", orgUnit, dept)
	}

	// Teams from each source.
	newTeam := func(name, source, group, match string, members []string) string {
		t.Helper()
		id := pgtest.UUID(t)
		err := st.CreateTeam(ctx, tenant, Team{ID: id, Name: name, Source: source, GroupID: group, MatchValue: match, CreatedBy: "admin@live.test", CreatedAt: at},
			members, Audit{Actor: "admin@live.test", Action: "team.create", ObjectType: "team", ObjectID: id, At: at})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return id
	}
	var adaRef, graceRef string
	if err := db.QueryRowContext(ctx, `SELECT user_ref FROM ops.user_dim WHERE tenant_id = $1::uuid AND display_name = 'Ada Lovelace'`, tenant).Scan(&adaRef); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT user_ref FROM ops.user_dim WHERE tenant_id = $1::uuid AND display_name = 'Grace Hopper'`, tenant).Scan(&graceRef); err != nil {
		t.Fatal(err)
	}
	labs := newTeam("Labs", SourceOrgUnit, "", "ou=labs,dc=contoso,dc=com", nil)
	sales := newTeam("Sales", SourceDepartment, "", "SALES", nil)
	console := newTeam("Pilot", SourceConsole, "", "", []string{adaRef})
	if err := st.CreateTeam(ctx, tenant, Team{ID: pgtest.UUID(t), Name: "pilot", Source: SourceConsole, CreatedBy: "x", CreatedAt: at}, nil,
		Audit{Actor: "x", Action: "team.create", ObjectType: "team", At: at}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a duplicate name = %v", err)
	}

	// An imported Entra group, filled by the next pass.
	created, e := svc.CreateGroup(ctx, scim.Principal{TenantID: tenant, Actor: graphsync.Actor}, map[string]any{"displayName": "Sales", "externalId": oidGroup})
	if e != nil {
		t.Fatal(e)
	}
	group := newTeam("Sales group", SourceGroup, created["id"].(string), "", nil)
	runner.RunTenant(ctx, target)

	teams, err := st.Teams(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, tm := range teams {
		counts[tm.ID] = tm.Members
	}
	if counts[labs] != 1 || counts[sales] != 1 || counts[console] != 1 || counts[group] != 1 || len(teams) != 4 {
		t.Fatalf("teams = %+v", teams)
	}
	members, err := st.TeamMembers(ctx, tenant, group, 10)
	if err != nil || len(members) != 1 || members[0].UserRef != graceRef || members[0].Name != "Grace Hopper" {
		t.Fatalf("group members = %+v, %v", members, err)
	}
	for _, tm := range teams {
		if tm.ID == group && tm.GroupName != "Sales (Entra)" {
			t.Fatalf("the pass did not take the group's name from Entra: %+v", tm)
		}
	}

	// Members are chosen only for a console team.
	if err := st.ChangeMembers(ctx, tenant, sales, []string{adaRef}, nil, Audit{Actor: "x", Action: "team.members.change", ObjectType: "team", At: at}); !errors.Is(err, ErrNotConsole) {
		t.Fatalf("ChangeMembers on a department team = %v", err)
	}
	if err := st.ChangeMembers(ctx, tenant, console, []string{graceRef}, []string{adaRef}, Audit{Actor: "x", Action: "team.members.change", ObjectType: "team", At: at}); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.TeamMembers(ctx, tenant, console, 10); len(m) != 1 || m[0].UserRef != graceRef {
		t.Fatalf("console members = %+v", m)
	}

	values, err := st.DirectoryValues(ctx, tenant, SourceOrgUnit, 10)
	if err != nil || len(values) != 1 || values[0].People != 1 {
		t.Fatalf("org units = %+v, %v", values, err)
	}
	groups, err := st.Groups(ctx, tenant, 10)
	if err != nil || len(groups) != 1 || groups[0].Members != 1 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	if id, found, err := st.GroupByExternalID(ctx, tenant, oidGroup); err != nil || !found || id != created["id"] {
		t.Fatalf("GroupByExternalID = %s %v %v", id, found, err)
	}

	if err := st.DeleteTeam(ctx, tenant, console, Audit{Actor: "x", Action: "team.delete", ObjectType: "team", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTeam(ctx, tenant, console, Audit{Actor: "x", Action: "team.delete", ObjectType: "team", At: at}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if err := st.SetSync(ctx, tenant, false, Audit{Actor: "x", Action: "directory_sync.disable", ObjectType: "directory_sync", At: at}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := syncStore.Target(ctx, tenant); ok {
		t.Fatal("the pull is still on")
	}
	var audits int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ops.audit WHERE tenant_id = $1::uuid AND actor_id = $2`, tenant, graphsync.Actor).Scan(&audits); err != nil || audits < 3 {
		t.Fatalf("audit rows by the pull = %d, %v", audits, err)
	}
	if _, err := st.Status(ctx, pgtest.UUID(t)); !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("an unknown tenant = %v", err)
	}
}
