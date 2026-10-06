package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedMigrations(t *testing.T) {
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].Version != 1 || ms[0].Name != "schema" || !strings.Contains(ms[0].SQL, "CREATE SCHEMA ops;") {
		t.Fatalf("version 1 is not schema.sql: %+v", ms[0].Version)
	}
	for i, m := range ms {
		if m.Version != i+1 {
			t.Fatalf("migration %d has version %d", i, m.Version)
		}
	}
}

func TestLoadMigrations(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	tests := []struct {
		name    string
		files   fstest.MapFS
		want    []string // "version:name"
		wantErr string
	}{
		{
			name:  "schema only",
			files: fstest.MapFS{"schema.sql": file("CREATE SCHEMA ops;"), "migrations/README.md": file("x")},
			want:  []string{"1:schema"},
		},
		{
			name: "ordered and contiguous",
			files: fstest.MapFS{
				"schema.sql":                    file("x"),
				"migrations/0003-second.sql":    file("y"),
				"migrations/0002-add-index.sql": file("z"),
				"migrations/README.md":          file("ignored"),
			},
			want: []string{"1:schema", "2:add-index", "3:second"},
		},
		{name: "no schema", files: fstest.MapFS{}, wantErr: "schema.sql"},
		{
			name:    "version 1 is schema.sql",
			files:   fstest.MapFS{"schema.sql": file("x"), "migrations/0001-again.sql": file("y")},
			wantErr: "expected version 0002",
		},
		{
			name:    "gap",
			files:   fstest.MapFS{"schema.sql": file("x"), "migrations/0002-a.sql": file("y"), "migrations/0004-b.sql": file("z")},
			wantErr: "expected version 0003",
		},
		{
			name:    "duplicate version",
			files:   fstest.MapFS{"schema.sql": file("x"), "migrations/0002-a.sql": file("y"), "migrations/0002-b.sql": file("z")},
			wantErr: "expected version 0003",
		},
		{
			name:    "underscore",
			files:   fstest.MapFS{"schema.sql": file("x"), "migrations/0002_a.sql": file("y")},
			wantErr: "NNNN-name.sql",
		},
		{
			name:    "upper case",
			files:   fstest.MapFS{"schema.sql": file("x"), "migrations/0002-Add.sql": file("y")},
			wantErr: "NNNN-name.sql",
		},
		{
			name:    "empty file",
			files:   fstest.MapFS{"schema.sql": file("x"), "migrations/0002-a.sql": file(" \n")},
			wantErr: "empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := loadMigrations(tt.files)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range ms {
				got = append(got, fmt.Sprintf("%d:%s", m.Version, m.Name))
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

const oid = "6f1c2b4e-8d3a-4c5b-9e7f-0a1b2c3d4e5f"

func TestParseLogins(t *testing.T) {
	good := `[{"login":"ingest-api","role":"sac_ingest","objectId":"` + oid + `"},
	          {"login":"jobs","role":"sac_ops","objectId":"` + strings.ToUpper(oid) + `"}]`
	logins, err := ParseLogins(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(logins) != 2 || logins[0].Login != "ingest-api" || logins[1].Role != "sac_ops" {
		t.Fatalf("parsed %+v", logins)
	}

	for _, empty := range []string{"", "  \n"} {
		if l, err := ParseLogins(empty); err != nil || l != nil {
			t.Fatalf("ParseLogins(%q) = %v, %v; want nil, nil", empty, l, err)
		}
	}

	bad := map[string]string{
		"not json":         `ingest-api=sac_ingest`,
		"object not array": `{"login":"ingest-api","role":"sac_ingest","objectId":"` + oid + `"}`,
		"unknown field":    `[{"login":"ingest-api","role":"sac_ingest","objectId":"` + oid + `","admin":true}]`,
		"trailing data":    `[] []`,
		"unknown role":     `[{"login":"ingest-api","role":"sac_resolver","objectId":"` + oid + `"}]`,
		"superuser role":   `[{"login":"ingest-api","role":"azure_pg_admin","objectId":"` + oid + `"}]`,
		"missing role":     `[{"login":"ingest-api","objectId":"` + oid + `"}]`,
		"bad object id":    `[{"login":"ingest-api","role":"sac_ingest","objectId":"not-a-guid"}]`,
		"missing objectId": `[{"login":"ingest-api","role":"sac_ingest"}]`,
		"empty login":      `[{"login":"","role":"sac_ingest","objectId":"` + oid + `"}]`,
		"quote in login":   `[{"login":"a\"b","role":"sac_ingest","objectId":"` + oid + `"}]`,
		"space in login":   `[{"login":"a b","role":"sac_ingest","objectId":"` + oid + `"}]`,
		"too long":         `[{"login":"` + strings.Repeat("a", 64) + `","role":"sac_ingest","objectId":"` + oid + `"}]`,
		"reserved prefix":  `[{"login":"pg_monitor","role":"sac_ingest","objectId":"` + oid + `"}]`,
		"login is a role":  `[{"login":"sac_ops","role":"sac_ops","objectId":"` + oid + `"}]`,
		"duplicate": `[{"login":"jobs","role":"sac_ops","objectId":"` + oid + `"},
		               {"login":"jobs","role":"sac_query","objectId":"` + oid + `"}]`,
	}
	for name, in := range bad {
		if _, err := ParseLogins(in); err == nil {
			t.Errorf("%s: ParseLogins accepted %s", name, in)
		}
	}
}

func TestStatementsQuoteIdentifiers(t *testing.T) {
	l := Login{Login: "ingest-api", Role: "sac_ingest", ObjectID: oid}
	if got, want := grantStatement(l), `GRANT "sac_ingest" TO "ingest-api"`; got != want {
		t.Fatalf("grantStatement = %s, want %s", got, want)
	}
	if got, want := revokeStatement("sac_query", "content.vault"), `REVOKE "sac_query" FROM "content.vault"`; got != want {
		t.Fatalf("revokeStatement = %s, want %s", got, want)
	}
}

// fakeSession answers the migrator's own statements from fields and records every statement.
type fakeSession struct {
	ledger      string            // what sqlApplied returns
	populated   bool              // what sqlPopulated returns
	logins      map[string]bool   // existing login roles
	memberOf    map[string]string // login -> comma-separated runtime roles
	principalFn bool              // pgaadauth_create_principal_with_oid exists
	failOn      string            // a statement containing this fails
	log         []string
}

func (f *fakeSession) record(prefix, q string, args []any) error {
	entry := prefix + q
	if len(args) > 0 {
		entry += fmt.Sprint(args)
	}
	f.log = append(f.log, entry)
	if f.failOn != "" && strings.Contains(q, f.failOn) {
		return errors.New("statement failed")
	}
	return nil
}

func (f *fakeSession) exec(_ context.Context, q string, args ...any) error {
	return f.record("exec ", q, args)
}

func (f *fakeSession) queryRow(_ context.Context, q string, args ...any) scanner {
	f.log = append(f.log, "query "+q)
	switch q {
	case sqlApplied:
		return row{f.ledger}
	case sqlPopulated:
		return row{f.populated}
	case sqlRoleExists:
		return row{f.logins[args[0].(string)]}
	case sqlPrincipalFunc:
		return row{f.principalFn}
	case sqlMemberOf:
		return row{f.memberOf[args[0].(string)]}
	}
	return row{fmt.Errorf("unexpected query %q", q)}
}

func (f *fakeSession) begin(context.Context) (txn, error) {
	f.log = append(f.log, "begin")
	return fakeTxn{f}, nil
}

type fakeTxn struct{ f *fakeSession }

func (t fakeTxn) exec(_ context.Context, q string, args ...any) error {
	return t.f.record("tx ", q, args)
}
func (t fakeTxn) commit() error   { t.f.log = append(t.f.log, "commit"); return nil }
func (t fakeTxn) rollback() error { t.f.log = append(t.f.log, "rollback"); return nil }

type row struct{ v any }

func (r row) Scan(dest ...any) error {
	switch v := r.v.(type) {
	case error:
		return v
	case string:
		*dest[0].(*string) = v
	case bool:
		*dest[0].(*bool) = v
	}
	return nil
}

func (f *fakeSession) has(entry string) bool {
	for _, e := range f.log {
		if e == entry {
			return true
		}
	}
	return false
}

func (f *fakeSession) count(prefix string) int {
	n := 0
	for _, e := range f.log {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

var testMigrations = []Migration{
	{Version: 1, Name: "schema", SQL: "CREATE SCHEMA ops;"},
	{Version: 2, Name: "add-index", SQL: "CREATE INDEX x ON ops.t (a);"},
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMigrateEmptyDatabaseAppliesEverything(t *testing.T) {
	f := &fakeSession{}
	if err := migrate(context.Background(), f, testMigrations, nil, quiet()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("exec %s[%d]", sqlLock, lockKey),
		"exec " + sqlLedger,
		"tx CREATE SCHEMA ops;",
		"tx " + sqlRecord + "[1 schema]",
		"tx CREATE INDEX x ON ops.t (a);",
		"tx " + sqlRecord + "[2 add-index]",
		fmt.Sprintf("exec %s[%d]", sqlUnlock, lockKey),
	} {
		if !f.has(want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(f.log, "\n"))
		}
	}
	if f.count("begin") != 2 || f.count("commit") != 2 {
		t.Fatalf("want one transaction per migration, log:\n%s", strings.Join(f.log, "\n"))
	}
}

func TestMigrateUpToDateIsANoOp(t *testing.T) {
	f := &fakeSession{ledger: "1:schema,2:add-index"}
	if err := migrate(context.Background(), f, testMigrations, nil, quiet()); err != nil {
		t.Fatal(err)
	}
	if f.count("begin") != 0 || f.count("tx ") != 0 {
		t.Fatalf("an up-to-date database was changed:\n%s", strings.Join(f.log, "\n"))
	}
}

func TestMigrateAppliesOnlyPending(t *testing.T) {
	f := &fakeSession{ledger: "1:schema"}
	if err := migrate(context.Background(), f, testMigrations, nil, quiet()); err != nil {
		t.Fatal(err)
	}
	if f.has("tx CREATE SCHEMA ops;") || !f.has("tx CREATE INDEX x ON ops.t (a);") || f.count("begin") != 1 {
		t.Fatalf("want only version 2 applied:\n%s", strings.Join(f.log, "\n"))
	}
}

func TestMigrateRefuses(t *testing.T) {
	tests := map[string]struct {
		f       *fakeSession
		wantErr string
	}{
		"database newer":       {&fakeSession{ledger: "1:schema,2:add-index,3:later"}, "newer than this migrator"},
		"renamed migration":    {&fakeSession{ledger: "1:schema,2:other"}, `records "2:other"`},
		"unrecorded schema":    {&fakeSession{populated: true}, "refusing to apply schema.sql"},
		"lock unavailable":     {&fakeSession{failOn: "pg_advisory_lock"}, "advisory lock"},
		"failing migration":    {&fakeSession{failOn: "CREATE INDEX"}, "migration 0002-add-index"},
		"ledger not creatable": {&fakeSession{failOn: "CREATE TABLE IF NOT EXISTS"}, "schema_migration"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := migrate(context.Background(), tt.f, testMigrations, nil, quiet())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestFailedMigrationRollsBackAndRecordsNothing(t *testing.T) {
	f := &fakeSession{failOn: "CREATE INDEX"}
	if err := migrate(context.Background(), f, testMigrations, nil, quiet()); err == nil {
		t.Fatal("want an error")
	}
	if f.has("tx "+sqlRecord+"[2 add-index]") || f.count("rollback") != 1 || f.count("commit") != 1 {
		t.Fatalf("version 2 must roll back without a ledger row, version 1 must commit:\n%s", strings.Join(f.log, "\n"))
	}
	if !f.has(fmt.Sprintf("exec %s[%d]", sqlUnlock, lockKey)) {
		t.Fatal("the advisory lock was not released after a failure")
	}
}

func TestLogins(t *testing.T) {
	logins := []Login{
		{Login: "ingest-api", Role: "sac_ingest", ObjectID: oid},
		{Login: "query-api", Role: "sac_query", ObjectID: oid},
	}

	t.Run("creates a missing Entra login, then grants", func(t *testing.T) {
		f := &fakeSession{ledger: "1:schema,2:add-index", principalFn: true, logins: map[string]bool{"query-api": true}}
		if err := migrate(context.Background(), f, testMigrations, logins, quiet()); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"exec " + sqlCreatePrincipal + "[ingest-api " + oid + "]",
			`exec GRANT "sac_ingest" TO "ingest-api"`,
			`exec GRANT "sac_query" TO "query-api"`,
		} {
			if !f.has(want) {
				t.Errorf("missing %q in\n%s", want, strings.Join(f.log, "\n"))
			}
		}
		if f.count("exec "+sqlCreatePrincipal) != 1 {
			t.Fatalf("an existing login was created again:\n%s", strings.Join(f.log, "\n"))
		}
	})

	t.Run("refuses to invent a login where Entra principals are unavailable", func(t *testing.T) {
		f := &fakeSession{ledger: "1:schema,2:add-index"}
		err := migrate(context.Background(), f, testMigrations, logins, quiet())
		if err == nil || !strings.Contains(err.Error(), "pgaadauth_create_principal_with_oid is not available") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a login holds exactly its configured role", func(t *testing.T) {
		f := &fakeSession{
			ledger:   "1:schema,2:add-index",
			logins:   map[string]bool{"ingest-api": true, "query-api": true},
			memberOf: map[string]string{"ingest-api": "sac_control,sac_ingest"},
		}
		if err := migrate(context.Background(), f, testMigrations, logins, quiet()); err != nil {
			t.Fatal(err)
		}
		if !f.has(`exec REVOKE "sac_control" FROM "ingest-api"`) || f.has(`exec REVOKE "sac_ingest" FROM "ingest-api"`) {
			t.Fatalf("want only the stale role revoked:\n%s", strings.Join(f.log, "\n"))
		}
	})

	t.Run("no logins configured grants nothing", func(t *testing.T) {
		f := &fakeSession{ledger: "1:schema,2:add-index"}
		if err := migrate(context.Background(), f, testMigrations, nil, quiet()); err != nil {
			t.Fatal(err)
		}
		if f.count("exec GRANT") != 0 || f.count("query "+sqlRoleExists) != 0 {
			t.Fatalf("logins were touched:\n%s", strings.Join(f.log, "\n"))
		}
	})
}
