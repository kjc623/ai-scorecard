package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/session"
)

// =====================================================================================
// Every SQL statement the identity service issues, in one file.
// =====================================================================================
//
// The rule control-api's other stores hold: constants, reviewable in one screen, prepared verbatim by
// the tagged live test. Three access paths, chosen by what is known when the statement runs:
//
//   - before any tenant is known (an Entra tid, a connection id from an attempt, an email domain), the
//     contract's SECURITY DEFINER functions, which are the only way past row-level security;
//   - ops.auth_signin, which is pre-tenant by nature and read and written by sac_control alone, with
//     no RLS (contract §1 leaves that to the schema; this file assumes it);
//   - everything else in a transaction that sets app.tenant_id first, so RLS is the final arbiter.
//
// Lists travel as comma-joined text split in SQL, as the session store's roles do: the values (user
// refs, role names) are closed identifiers with no comma, and text keeps the statements driver-neutral.

const connectionColumns = `connection_id::text, tenant_id::text, provider, coalesce(entra_tenant_id, ''),
       coalesce(issuer, ''), coalesce(client_id, ''), client_secret_enc, scopes, roles_claim,
       role_map::text, status, created_at, activated_at, coalesce(activated_by, '')`

const (
	sqlIdentitySetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	sqlConnectionForEntra = `SELECT ` + connectionColumns + `
  FROM ops.identity_connection_for_entra($1::text)`

	sqlConnectionByID = `SELECT ` + connectionColumns + `
  FROM ops.identity_connection_by_id($1::uuid)`

	sqlTenantForEmailDomain = `SELECT ops.tenant_for_email_domain($1::text)::text`

	sqlPutAttempt = `
INSERT INTO ops.auth_signin (attempt_hash, connection_id, state, nonce, code_verifier_enc, redirect_uri,
                             invite_id, created_at, expires_at)
VALUES ($1::bytea, nullif($2::text, '')::uuid, $3::text, nullif($4::text, ''), $5::bytea,
        nullif($6::text, ''), nullif($7::text, '')::uuid, $8::timestamptz, $9::timestamptz)`

	// Delete-returning is the single use: two concurrent completions cannot both read the row.
	sqlTakeAttempt = `
DELETE FROM ops.auth_signin WHERE attempt_hash = $1::bytea
RETURNING coalesce(connection_id::text, ''), coalesce(state, ''), coalesce(nonce, ''), code_verifier_enc,
          coalesce(redirect_uri, ''), coalesce(invite_id::text, ''), created_at, expires_at`

	sqlSweepAttempts = `DELETE FROM ops.auth_signin WHERE expires_at < $1::timestamptz`

	sqlTenantConnections = `SELECT ` + connectionColumns + `
  FROM ops.identity_connection WHERE tenant_id = $1::uuid ORDER BY created_at DESC`

	sqlTenantName = `SELECT name FROM ops.tenant WHERE tenant_id = $1::uuid`

	sqlTenantDomains = `SELECT domain FROM ops.tenant_email_domain WHERE tenant_id = $1::uuid ORDER BY domain`

	inviteColumns = `invite_id::text, tenant_id::text, token_hash, created_by, created_at, expires_at, used_at,
       coalesce(used_by, '')`

	sqlInviteByHash = `SELECT ` + inviteColumns + `
  FROM ops.onboarding_invite WHERE tenant_id = $1::uuid AND token_hash = $2::text`

	sqlInviteByID = `SELECT ` + inviteColumns + `
  FROM ops.onboarding_invite WHERE tenant_id = $1::uuid AND invite_id = $2::uuid`

	sqlRoleGrants = `
SELECT role FROM ops.role_grant WHERE tenant_id = $1::uuid AND connection_id = $2::uuid AND subject = $3::text`

	sqlUserRefKey = `SELECT user_ref_key_enc FROM ops.tenant WHERE tenant_id = $1::uuid`

	// The candidates in order (a session's recorded ref first), each resolved through the alias table
	// to the canonical ref the SCIM row carries (contract §4).
	sqlScimUser = `
SELECT s.user_ref, s.active
  FROM unnest(string_to_array($2::text, ',')) WITH ORDINALITY AS c(ref, ord)
  LEFT JOIN ops.user_ref_alias a ON a.tenant_id = $1::uuid AND a.alias_ref = c.ref
  JOIN ops.scim_user s ON s.tenant_id = $1::uuid AND s.user_ref = coalesce(a.user_ref, c.ref)
 ORDER BY c.ord
 LIMIT 1`

	sqlFindOwnConnection = `SELECT ` + connectionColumns + `
  FROM ops.identity_connection
 WHERE tenant_id = $1::uuid AND provider = $2::text
   AND (entra_tenant_id = nullif($3::text, '') OR issuer = nullif($4::text, ''))
 LIMIT 1`

	sqlUpdatePendingClient = `
UPDATE ops.identity_connection SET client_id = $3::text, client_secret_enc = $4::bytea
 WHERE tenant_id = $1::uuid AND connection_id = $2::uuid AND status = 'pending'`

	// The uniqueness of entra_tenant_id and issuer is enforced across tenants by the index whatever
	// row-level security hides, so DO NOTHING returning no row is "another tenant holds it" — the
	// caller has already looked for its own row.
	sqlInsertConnection = `
INSERT INTO ops.identity_connection (tenant_id, provider, entra_tenant_id, issuer, client_id, client_secret_enc,
                                     scopes)
VALUES ($1::uuid, $2::text, nullif($3::text, ''), nullif($4::text, ''), nullif($5::text, ''), $6::bytea,
        coalesce(nullif($7::text, ''), 'openid profile email'))
ON CONFLICT DO NOTHING
RETURNING ` + connectionColumns

	// Single use is the WHERE clause: a spent or expired invite updates nothing.
	sqlUseInvite = `
UPDATE ops.onboarding_invite SET used_at = $3::timestamptz, used_by = $4::text
 WHERE tenant_id = $1::uuid AND invite_id = $2::uuid AND used_at IS NULL AND expires_at > $3::timestamptz`

	sqlActivateConnection = `
UPDATE ops.identity_connection
   SET status = 'active', activated_at = coalesce(activated_at, $3::timestamptz),
       activated_by = coalesce(activated_by, $4::text)
 WHERE tenant_id = $1::uuid AND connection_id = $2::uuid AND status IN ('pending', 'active')`

	sqlGrantRole = `
INSERT INTO ops.role_grant (tenant_id, connection_id, subject, role, granted_by, granted_at)
VALUES ($1::uuid, $2::uuid, $3::text, $4::text, $5::text, $6::timestamptz)
ON CONFLICT DO NOTHING`

	sqlIdentityAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail, occurred_at)
VALUES ($1::uuid, $2::text, $3::text, $4::text, $5::text, nullif($6::text, ''), $7::jsonb, $8::timestamptz)`
)

// Statement pairs a statement with what it is for, so the live test can prepare each by name.
type Statement struct {
	Name, Purpose, SQL string
}

// Statements is every statement the identity store issues.
var Statements = []Statement{
	{"set_tenant", "RLS session tenant (transaction-local)", sqlIdentitySetTenant},
	{"connection_for_entra", "tid -> active connection (definer)", sqlConnectionForEntra},
	{"connection_by_id", "attempt/session connection, any status (definer)", sqlConnectionByID},
	{"tenant_for_email_domain", "work email -> tenant (definer)", sqlTenantForEmailDomain},
	{"put_attempt", "hold PKCE/state/nonce for ten minutes", sqlPutAttempt},
	{"take_attempt", "single-use attempt redemption", sqlTakeAttempt},
	{"sweep_attempts", "drop expired attempts", sqlSweepAttempts},
	{"tenant_connections", "a tenant's connections", sqlTenantConnections},
	{"tenant_name", "onboarding page heading", sqlTenantName},
	{"tenant_domains", "onboarding page domains", sqlTenantDomains},
	{"invite_by_hash", "invite token lookup in its own tenant", sqlInviteByHash},
	{"invite_by_id", "an attempt's invite", sqlInviteByID},
	{"role_grants", "ops.role_grant half of the role rule", sqlRoleGrants},
	{"user_ref_key", "the tenant's sealed user_ref key", sqlUserRefKey},
	{"scim_user", "SCIM person behind a sign-in, through aliases", sqlScimUser},
	{"find_own_connection", "this tenant's connection for a tid/issuer", sqlFindOwnConnection},
	{"update_pending_client", "re-entered OIDC client on a pending connection", sqlUpdatePendingClient},
	{"insert_connection", "pending connection, refused when linked elsewhere", sqlInsertConnection},
	{"use_invite", "single-use invite", sqlUseInvite},
	{"activate_connection", "first successful sign-in activates", sqlActivateConnection},
	{"grant_role", "first-admin grant", sqlGrantRole},
	{"audit", "identity actions with the real actor", sqlIdentityAudit},
}

// SQLStore is the database/sql Store. The caller owns the driver and the handle.
type SQLStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQL wraps an open handle.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db, now: time.Now} }

func (s *SQLStore) withTenant(ctx context.Context, tenantID string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("identity: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sqlIdentitySetTenant, tenantID); err != nil {
		return fmt.Errorf("identity: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type scanner interface{ Scan(...any) error }

func scanConnection(row scanner) (Connection, error) {
	var c Connection
	var roleMap string
	var activated sql.NullTime
	err := row.Scan(&c.ID, &c.TenantID, &c.Provider, &c.EntraTenantID, &c.Issuer, &c.ClientID, &c.ClientSecretEnc,
		&c.Scopes, &c.RolesClaim, &roleMap, &c.Status, &c.CreatedAt, &activated, &c.ActivatedBy)
	if err != nil {
		return Connection{}, err
	}
	if activated.Valid {
		t := activated.Time
		c.ActivatedAt = &t
	}
	if len(c.ClientSecretEnc) == 0 {
		c.ClientSecretEnc = nil
	}
	// A role_map value that is not a string is ignored rather than fatal: the map is admin
	// configuration, and one bad entry should cost that entry, not every sign-in.
	var raw map[string]any
	if json.Unmarshal([]byte(roleMap), &raw) == nil {
		for k, v := range raw {
			if s, ok := v.(string); ok {
				if c.RoleMap == nil {
					c.RoleMap = map[string]string{}
				}
				c.RoleMap[k] = s
			}
		}
	}
	return c, nil
}

func (s *SQLStore) oneConnection(ctx context.Context, q string, arg any) (Connection, error) {
	c, err := scanConnection(s.db.QueryRowContext(ctx, q, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNotFound
	}
	if err != nil {
		return Connection{}, fmt.Errorf("identity: connection lookup: %w", err)
	}
	return c, nil
}

// ConnectionForEntraTenant implements Store.
func (s *SQLStore) ConnectionForEntraTenant(ctx context.Context, tid string) (Connection, error) {
	return s.oneConnection(ctx, sqlConnectionForEntra, tid)
}

// ConnectionByID implements Store.
func (s *SQLStore) ConnectionByID(ctx context.Context, id string) (Connection, error) {
	if !session.IsUUID(id) {
		return Connection{}, ErrNotFound
	}
	return s.oneConnection(ctx, sqlConnectionByID, id)
}

// TenantForEmailDomain implements Store.
func (s *SQLStore) TenantForEmailDomain(ctx context.Context, domain string) (string, error) {
	var tenant sql.NullString
	if err := s.db.QueryRowContext(ctx, sqlTenantForEmailDomain, domain).Scan(&tenant); err != nil {
		return "", fmt.Errorf("identity: domain lookup: %w", err)
	}
	if !tenant.Valid || tenant.String == "" {
		return "", ErrNotFound
	}
	return tenant.String, nil
}

// PutAttempt implements Store.
func (s *SQLStore) PutAttempt(ctx context.Context, a Attempt) error {
	_, err := s.db.ExecContext(ctx, sqlPutAttempt, a.Hash, a.ConnectionID, a.State, a.Nonce, nullable(a.VerifierEnc),
		a.RedirectURI, a.InviteID, a.CreatedAt, a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("identity: put attempt: %w", err)
	}
	return nil
}

// TakeAttempt implements Store.
func (s *SQLStore) TakeAttempt(ctx context.Context, hash []byte) (Attempt, error) {
	a := Attempt{Hash: hash}
	err := s.db.QueryRowContext(ctx, sqlTakeAttempt, hash).Scan(&a.ConnectionID, &a.State, &a.Nonce, &a.VerifierEnc,
		&a.RedirectURI, &a.InviteID, &a.CreatedAt, &a.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("identity: take attempt: %w", err)
	}
	return a, nil
}

// SweepAttempts implements Store.
func (s *SQLStore) SweepAttempts(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, sqlSweepAttempts, before)
	return err
}

// TenantConnections implements Store.
func (s *SQLStore) TenantConnections(ctx context.Context, tenantID string) ([]Connection, error) {
	var out []Connection
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, sqlTenantConnections, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanConnection(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("identity: tenant connections: %w", err)
	}
	return out, nil
}

// TenantSummary implements Store.
func (s *SQLStore) TenantSummary(ctx context.Context, tenantID string) (TenantSummary, error) {
	var sum TenantSummary
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, sqlTenantName, tenantID).Scan(&sum.Name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		rows, err := tx.QueryContext(ctx, sqlTenantDomains, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				return err
			}
			sum.Domains = append(sum.Domains, d)
		}
		return rows.Err()
	})
	return sum, err
}

func scanInvite(row scanner) (Invite, error) {
	var inv Invite
	var used sql.NullTime
	if err := row.Scan(&inv.ID, &inv.TenantID, &inv.TokenHash, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt,
		&used, &inv.UsedBy); err != nil {
		return Invite{}, err
	}
	if used.Valid {
		t := used.Time
		inv.UsedAt = &t
	}
	return inv, nil
}

func (s *SQLStore) oneInvite(ctx context.Context, tenantID, q, arg string) (Invite, error) {
	var inv Invite
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		inv, err = scanInvite(tx.QueryRowContext(ctx, q, tenantID, arg))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return inv, err
}

// Invite implements Store.
func (s *SQLStore) Invite(ctx context.Context, tenantID, tokenHash string) (Invite, error) {
	return s.oneInvite(ctx, tenantID, sqlInviteByHash, tokenHash)
}

// InviteByID implements Store.
func (s *SQLStore) InviteByID(ctx context.Context, tenantID, inviteID string) (Invite, error) {
	if !session.IsUUID(inviteID) {
		return Invite{}, ErrNotFound
	}
	return s.oneInvite(ctx, tenantID, sqlInviteByID, inviteID)
}

// RoleGrants implements Store.
func (s *SQLStore) RoleGrants(ctx context.Context, tenantID, connectionID, subject string) ([]string, error) {
	var out []string
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, sqlRoleGrants, tenantID, connectionID, subject)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("identity: role grants: %w", err)
	}
	return out, nil
}

// UserRefKey implements Store.
func (s *SQLStore) UserRefKey(ctx context.Context, tenantID string) ([]byte, error) {
	var sealed []byte
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, sqlUserRefKey, tenantID).Scan(&sealed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("identity: user_ref key: %w", err)
	}
	return sealed, nil
}

// ScimUser implements Store.
func (s *SQLStore) ScimUser(ctx context.Context, tenantID string, refs []string) (string, bool, error) {
	var ref string
	var active bool
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, sqlScimUser, tenantID, strings.Join(refs, ",")).Scan(&ref, &active)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return ref, active, err
}

// CreatePendingConnection implements Store.
func (s *SQLStore) CreatePendingConnection(ctx context.Context, nc NewConnection, audit AuditEntry) (Connection, error) {
	var out Connection
	err := s.withTenant(ctx, nc.TenantID, func(tx *sql.Tx) error {
		own, err := scanConnection(tx.QueryRowContext(ctx, sqlFindOwnConnection, nc.TenantID, nc.Provider,
			nc.EntraTenantID, nc.Issuer))
		switch {
		case err == nil:
			switch own.Status {
			case StatusDisabled:
				return ErrConnectionDisabled
			case StatusPending:
				if nc.Provider == ProviderOIDC {
					if _, err := tx.ExecContext(ctx, sqlUpdatePendingClient, nc.TenantID, own.ID, nc.ClientID,
						nullable(nc.ClientSecretEnc)); err != nil {
						return err
					}
					own.ClientID, own.ClientSecretEnc = nc.ClientID, nc.ClientSecretEnc
				}
			}
			out = own
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		out, err = scanConnection(tx.QueryRowContext(ctx, sqlInsertConnection, nc.TenantID, nc.Provider,
			nc.EntraTenantID, nc.Issuer, nc.ClientID, nullable(nc.ClientSecretEnc), nc.Scopes))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLinkedElsewhere
		}
		if err != nil {
			return err
		}
		audit.ObjectID = out.ID
		return s.audit(ctx, tx, nc.TenantID, audit)
	})
	return out, err
}

// Activate implements Store.
func (s *SQLStore) Activate(ctx context.Context, a Activation) error {
	return s.withTenant(ctx, a.TenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, sqlUseInvite, a.TenantID, a.InviteID, a.At, a.Actor)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrInviteUsed
		}
		res, err = tx.ExecContext(ctx, sqlActivateConnection, a.TenantID, a.ConnectionID, a.At, a.Actor)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrConnectionDisabled
		}
		if _, err := tx.ExecContext(ctx, sqlGrantRole, a.TenantID, a.ConnectionID, a.Subject, session.RoleAdmin,
			"onboarding-invite:"+a.InviteID, a.At); err != nil {
			return err
		}
		for _, e := range activationAudit(a) {
			if err := s.audit(ctx, tx, a.TenantID, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// Audit implements Store.
func (s *SQLStore) Audit(ctx context.Context, tenantID string, e AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error { return s.audit(ctx, tx, tenantID, e) })
}

func (s *SQLStore) audit(ctx context.Context, tx *sql.Tx, tenantID string, e AuditEntry) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, sqlIdentityAudit, tenantID, e.ActorType, e.ActorID, e.Action, e.ObjectType,
		e.ObjectID, string(b), s.now().UTC()); err != nil {
		return fmt.Errorf("identity: audit %s: %w", e.Action, err)
	}
	return nil
}

func nullable(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
