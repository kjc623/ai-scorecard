// Package database holds the product's PostgreSQL schema and the migrator that applies it.
//
// schema.sql is schema version 1 and always describes the full current schema; it is applied to an
// empty database. Every later change is also shipped as migrations/NNNN-name.sql (NNNN from 0002,
// contiguous) and applied, in order, to databases that already exist. Each applied version is
// recorded in public.schema_migration, so running the migrator again changes nothing.
package database

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql migrations
var files embed.FS

// EnvLogins is the variable cmd/migrate reads the component logins from.
const EnvLogins = "SAC_DB_LOGINS"

// RuntimeRoles are the roles schema.sql creates for the components. A login is granted exactly one.
var RuntimeRoles = []string{"sac_ingest", "sac_control", "sac_vault", "sac_query", "sac_ops"}

// lockKey names the session advisory lock that serialises migrators on one database.
const lockKey int64 = 0x5341435f4d494752 // "SAC_MIGR"

// The migrator's own statements. Named, so the test double can answer them by identity.
const (
	sqlLock            = `SELECT pg_advisory_lock($1)`
	sqlUnlock          = `SELECT pg_advisory_unlock($1)`
	sqlLedger          = `CREATE TABLE IF NOT EXISTS public.schema_migration (version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`
	sqlApplied         = `SELECT coalesce(string_agg(version::text || ':' || name, ',' ORDER BY version), '') FROM public.schema_migration`
	sqlRecord          = `INSERT INTO public.schema_migration (version, name) VALUES ($1, $2)`
	sqlPopulated       = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname IN ('ref', 'ops', 'ingest', 'mart'))`
	sqlRoleExists      = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`
	sqlPrincipalFunc   = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_proc WHERE proname = 'pgaadauth_create_principal_with_oid')`
	sqlCreatePrincipal = `SELECT pgaadauth_create_principal_with_oid($1, $2, 'service', false, false)`
	sqlMemberOf        = `SELECT coalesce(string_agg(r.rolname, ',' ORDER BY r.rolname), '')
  FROM pg_catalog.pg_auth_members m
  JOIN pg_catalog.pg_roles r ON r.oid = m.roleid
  JOIN pg_catalog.pg_roles u ON u.oid = m.member
 WHERE u.rolname = $1
   AND r.rolname IN ('sac_ingest', 'sac_control', 'sac_vault', 'sac_query', 'sac_ops')`
)

// Migration is one version of the schema.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns every version in order: schema.sql as version 1, then migrations/*.sql.
func Migrations() ([]Migration, error) {
	return loadMigrations(files)
}

var migrationFile = regexp.MustCompile(`^([0-9]{4})-([a-z0-9]+(?:-[a-z0-9]+)*)\.sql$`)

func loadMigrations(fsys fs.FS) ([]Migration, error) {
	schema, err := fs.ReadFile(fsys, "schema.sql")
	if err != nil {
		return nil, fmt.Errorf("schema.sql: %w", err)
	}
	out := []Migration{{Version: 1, Name: "schema", SQL: string(schema)}}

	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	for _, e := range entries { // ReadDir sorts by name, and the zero-padded prefix sorts by version
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations/%s: a migration is named NNNN-name.sql (lower-case name, words joined by '-')", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		if want := len(out) + 1; version != want {
			return nil, fmt.Errorf("migrations/%s: expected version %04d next; versions start at 0002 and are contiguous", e.Name(), want)
		}
		body, err := fs.ReadFile(fsys, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrations/%s: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("migrations/%s: empty", e.Name())
		}
		out = append(out, Migration{Version: version, Name: m[2], SQL: string(body)})
	}
	return out, nil
}

// Login maps a database login (in Azure, a managed identity's Microsoft Entra principal) to the
// runtime role it is granted.
type Login struct {
	Login    string `json:"login"`
	Role     string `json:"role"`
	ObjectID string `json:"objectId"`
}

var (
	loginName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	guid      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Validate reports the first problem with l.
func (l Login) Validate() error {
	switch {
	case !loginName.MatchString(l.Login):
		return fmt.Errorf("login %q: want 1-63 letters, digits, '_', '.' or '-', starting with a letter or digit", l.Login)
	case strings.HasPrefix(strings.ToLower(l.Login), "pg_"), strings.HasPrefix(strings.ToLower(l.Login), "azure_"):
		return fmt.Errorf("login %q: reserved prefix", l.Login)
	case slices.Contains(RuntimeRoles, l.Login) || l.Login == "sac_resolver":
		return fmt.Errorf("login %q: is a role the schema creates, not a login", l.Login)
	case !slices.Contains(RuntimeRoles, l.Role):
		return fmt.Errorf("login %q: role %q is not one of %s", l.Login, l.Role, strings.Join(RuntimeRoles, ", "))
	case !guid.MatchString(l.ObjectID):
		return fmt.Errorf("login %q: objectId %q is not a GUID", l.Login, l.ObjectID)
	}
	return nil
}

// ParseLogins reads the value of SAC_DB_LOGINS: a JSON array of {"login", "role", "objectId"}.
// Empty means there are no logins to grant.
func ParseLogins(s string) ([]Login, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var logins []Login
	if err := dec.Decode(&logins); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvLogins, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: unexpected data after the array", EnvLogins)
	}
	seen := make(map[string]bool, len(logins))
	for i, l := range logins {
		if err := l.Validate(); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", EnvLogins, i, err)
		}
		if seen[l.Login] {
			return nil, fmt.Errorf("%s[%d]: login %q is listed twice", EnvLogins, i, l.Login)
		}
		seen[l.Login] = true
	}
	return logins, nil
}

// grantStatement grants l its role, with both names quoted as identifiers.
func grantStatement(l Login) string {
	return "GRANT " + pgx.Identifier{l.Role}.Sanitize() + " TO " + pgx.Identifier{l.Login}.Sanitize()
}

// revokeStatement takes role away from login.
func revokeStatement(role, login string) string {
	return "REVOKE " + pgx.Identifier{role}.Sanitize() + " FROM " + pgx.Identifier{login}.Sanitize()
}

// Migrate brings db to the newest schema version, then makes each login a member of exactly its
// configured runtime role, creating the login first when it does not exist (Azure only). It holds
// one connection for the whole run, under a session advisory lock, so concurrent runs serialise.
// Each migration commits in its own transaction together with its ledger row.
func Migrate(ctx context.Context, db *sql.DB, logins []Login) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	return migrate(ctx, sqlSession{conn}, migrations, logins, slog.Default())
}

// session is the part of a dedicated connection the migrator uses; tests substitute a double.
type session interface {
	exec(ctx context.Context, query string, args ...any) error
	queryRow(ctx context.Context, query string, args ...any) scanner
	begin(ctx context.Context) (txn, error)
}

type txn interface {
	exec(ctx context.Context, query string, args ...any) error
	commit() error
	rollback() error
}

type scanner interface {
	Scan(dest ...any) error
}

func migrate(ctx context.Context, s session, migrations []Migration, logins []Login, log *slog.Logger) (err error) {
	if err := s.exec(ctx, sqlLock, lockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if uerr := s.exec(context.WithoutCancel(ctx), sqlUnlock, lockKey); uerr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", uerr)
		}
	}()

	if err := s.exec(ctx, sqlLedger); err != nil {
		return fmt.Errorf("schema_migration: %w", err)
	}
	current, err := appliedVersion(ctx, s, migrations)
	if err != nil {
		return err
	}
	if current == 0 {
		var populated bool
		if err := s.queryRow(ctx, sqlPopulated).Scan(&populated); err != nil {
			return fmt.Errorf("inspect database: %w", err)
		}
		if populated {
			return errors.New("the database has the product schemas but public.schema_migration records nothing; refusing to apply schema.sql over them")
		}
	}

	latest := migrations[len(migrations)-1].Version
	applied := 0
	for _, m := range migrations[current:] {
		log.Info("applying migration", "version", m.Version, "name", m.Name)
		if err := apply(ctx, s, m); err != nil {
			return fmt.Errorf("migration %04d-%s: %w", m.Version, m.Name, err)
		}
		applied++
	}
	log.Info("schema up to date", "version", latest, "applied", applied)

	return grantLogins(ctx, s, logins, log)
}

// appliedVersion reads the ledger and checks it is a prefix of migrations: versions 1..n, each
// under the name this migrator knows it by.
func appliedVersion(ctx context.Context, s session, migrations []Migration) (int, error) {
	var ledger string
	if err := s.queryRow(ctx, sqlApplied).Scan(&ledger); err != nil {
		return 0, fmt.Errorf("read schema_migration: %w", err)
	}
	if ledger == "" {
		return 0, nil
	}
	rows := strings.Split(ledger, ",")
	if len(rows) > len(migrations) {
		return 0, fmt.Errorf("the database is at schema version %d, newer than this migrator's %d", len(rows), migrations[len(migrations)-1].Version)
	}
	for i, row := range rows {
		version, name, _ := strings.Cut(row, ":")
		want := migrations[i]
		if version != strconv.Itoa(want.Version) || name != want.Name {
			return 0, fmt.Errorf("public.schema_migration records %q where this migrator has %04d-%s", row, want.Version, want.Name)
		}
	}
	return len(rows), nil
}

func apply(ctx context.Context, s session, m Migration) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := tx.exec(ctx, m.SQL); err != nil {
		return errors.Join(err, tx.rollback())
	}
	if err := tx.exec(ctx, sqlRecord, m.Version, m.Name); err != nil {
		return errors.Join(fmt.Errorf("record version: %w", err), tx.rollback())
	}
	if err := tx.commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func grantLogins(ctx context.Context, s session, logins []Login, log *slog.Logger) error {
	if len(logins) == 0 {
		log.Info("no logins configured", "variable", EnvLogins)
		return nil
	}
	var canCreate *bool
	for _, l := range logins {
		var exists bool
		if err := s.queryRow(ctx, sqlRoleExists, l.Login).Scan(&exists); err != nil {
			return fmt.Errorf("login %q: %w", l.Login, err)
		}
		if !exists {
			if canCreate == nil {
				var ok bool
				if err := s.queryRow(ctx, sqlPrincipalFunc).Scan(&ok); err != nil {
					return fmt.Errorf("inspect database: %w", err)
				}
				canCreate = &ok
			}
			if !*canCreate {
				return fmt.Errorf("login %q does not exist, and pgaadauth_create_principal_with_oid is not available to create it: logins are created only on Azure Database for PostgreSQL with Microsoft Entra authentication", l.Login)
			}
			if err := s.exec(ctx, sqlCreatePrincipal, l.Login, l.ObjectID); err != nil {
				return fmt.Errorf("create login %q: %w", l.Login, err)
			}
			log.Info("created login", "login", l.Login, "objectId", l.ObjectID)
		}

		var memberOf string
		if err := s.queryRow(ctx, sqlMemberOf, l.Login).Scan(&memberOf); err != nil {
			return fmt.Errorf("login %q: %w", l.Login, err)
		}
		for _, role := range strings.Split(memberOf, ",") {
			if role == "" || role == l.Role {
				continue
			}
			if err := s.exec(ctx, revokeStatement(role, l.Login)); err != nil {
				return fmt.Errorf("login %q: revoke %s: %w", l.Login, role, err)
			}
			log.Info("revoked role", "login", l.Login, "role", role)
		}
		if err := s.exec(ctx, grantStatement(l)); err != nil {
			return fmt.Errorf("login %q: grant %s: %w", l.Login, l.Role, err)
		}
		log.Info("granted role", "login", l.Login, "role", l.Role)
	}
	return nil
}

// sqlSession adapts a dedicated *sql.Conn. A statement without arguments runs over the simple query
// protocol, which is what lets one Exec carry a whole migration file.
type sqlSession struct{ conn *sql.Conn }

func (s sqlSession) exec(ctx context.Context, query string, args ...any) error {
	_, err := s.conn.ExecContext(ctx, query, args...)
	return err
}

func (s sqlSession) queryRow(ctx context.Context, query string, args ...any) scanner {
	return s.conn.QueryRowContext(ctx, query, args...)
}

func (s sqlSession) begin(ctx context.Context) (txn, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return sqlTxn{tx}, nil
}

type sqlTxn struct{ tx *sql.Tx }

func (t sqlTxn) exec(ctx context.Context, query string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, query, args...)
	return err
}

func (t sqlTxn) commit() error   { return t.tx.Commit() }
func (t sqlTxn) rollback() error { return t.tx.Rollback() }
