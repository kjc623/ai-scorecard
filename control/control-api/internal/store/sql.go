package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// =====================================================================================
// Every SQL statement this service issues, in one file.
// =====================================================================================
//
// The rule this file holds is the one ingest-api holds: there is exactly one place SQL appears, so
// the seam with database/schema.sql is reviewable in one screen and the statements can be executed
// against the real schema by a test without going through a Go driver. The live test under
// `sac_sql_driver` executes this text (placeholders and all) against a reachable PostgreSQL 17, so
// what is verified is the text below rather than a paraphrase.
//
// Every statement is tenant-scoped. The session tenant is set from the authenticated credential or
// the enrolment token before any other statement, and row-level security is forced, so a session
// with no tenant reads zero rows. That is the fail-closed behaviour C32 asks for.

const (
	// SQLSetTenant sets the row-level-security session tenant. `true` makes it transaction-local, so
	// a pooled connection cannot leak one tenant's session into the next request.
	SQLSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	// SQLTenant resolves the tenant's lifecycle state, pinned region and identity setting. $1 tenant.
	SQLTenant = `SELECT status, ingest_enabled, residency_region, device_identity, device_ca_pem
  FROM ops.tenant
 WHERE tenant_id = $1::uuid`

	// SQLResolveEnrolmentToken resolves a presented token by (tenant, hash). The tenant is taken from
	// the token, so it is known before this statement runs; the stored hash is the authority and the
	// caller compares the two in constant time.
	SQLResolveEnrolmentToken = `
SELECT tenant_id, token_hash, hardware_identity_hash, issued_at, expires_at, used_at, revoked_at
  FROM ops.enrolment_token
 WHERE tenant_id = $1::uuid AND token_hash = $2`

	// SQLFindDeviceByHardwareIdentity is the C11 idempotency lookup. The partial unique index
	// device_hardware_identity_uniq makes the result unique where a hash is present.
	SQLFindDeviceByHardwareIdentity = `
SELECT tenant_id, device_id, os, os_version, mdm_id, hardware_identity_hash,
       hostname, hostname_hash, agent_version, managed_state,
       residency_region, enrolled_at, revoked_at
  FROM ops.device
 WHERE tenant_id = $1::uuid AND hardware_identity_hash = $2`

	// SQLDevice reads one device by id.
	SQLDevice = `
SELECT tenant_id, device_id, os, os_version, mdm_id, hardware_identity_hash,
       hostname, hostname_hash, agent_version, managed_state,
       residency_region, enrolled_at, revoked_at
  FROM ops.device
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid`

	// SQLUpsertDevice inserts a device or refreshes the mutable fields of the one with the same id.
	// COALESCE keeps a previously recorded value if this request arrived without one, so a
	// re-enrolment that omits the hostname does not erase it. Which of hostname/hostname_hash is
	// populated is decided by the tenant setting before the call (ADR 0021).
	SQLUpsertDevice = `
INSERT INTO ops.device (tenant_id, device_id, os, os_version, mdm_id, hardware_identity_hash,
                        hostname, hostname_hash, agent_version, managed_state, residency_region)
VALUES ($1::uuid, $2::uuid, $3, nullif($4, ''), nullif($5, ''), nullif($6, ''),
        nullif($7, ''), nullif($8, ''), nullif($9, ''), nullif($10, ''), nullif($11, ''))
ON CONFLICT (tenant_id, device_id) DO UPDATE
   SET os                     = EXCLUDED.os,
       os_version             = EXCLUDED.os_version,
       mdm_id                 = EXCLUDED.mdm_id,
       hardware_identity_hash = COALESCE(EXCLUDED.hardware_identity_hash,
                                         ops.device.hardware_identity_hash),
       hostname               = COALESCE(EXCLUDED.hostname, ops.device.hostname),
       hostname_hash          = COALESCE(EXCLUDED.hostname_hash, ops.device.hostname_hash),
       agent_version          = COALESCE(EXCLUDED.agent_version, ops.device.agent_version),
       managed_state          = COALESCE(EXCLUDED.managed_state, ops.device.managed_state),
       residency_region       = COALESCE(EXCLUDED.residency_region,
                                         ops.device.residency_region)
RETURNING tenant_id, device_id, os, os_version, mdm_id, hardware_identity_hash,
          hostname, hostname_hash, agent_version, managed_state,
          residency_region, enrolled_at, revoked_at`

	// SQLRevokeDeviceCredentials closes every live credential of the device. It is paired with the
	// insert in one transaction, so a rotation cannot leave a device with two live credentials or
	// with none (docs/02 §2.2).
	SQLRevokeDeviceCredentials = `
UPDATE ops.device_credential
   SET revoked_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid AND revoked_at IS NULL`

	// SQLInsertCredential registers the new credential. credential_type is the ADR 0020 mode and
	// public_key_thumbprint is the single binding for both modes ($6 is NULL for x509).
	SQLInsertCredential = `
INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, credential_type,
                                   credential_origin, public_key_thumbprint, public_key_jwk, issued_at, expires_at)
VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7::jsonb, $8::timestamptz, $9::timestamptz)
RETURNING tenant_id, credential_id, device_id, credential_type, credential_origin, public_key_thumbprint,
          public_key_jwk, issued_at, expires_at, revoked_at`

	// SQLDeviceCredential reads one credential by id.
	SQLDeviceCredential = `
SELECT tenant_id, credential_id, device_id, credential_type, credential_origin, public_key_thumbprint,
       public_key_jwk, issued_at, expires_at, revoked_at
  FROM ops.device_credential
 WHERE tenant_id = $1::uuid AND credential_id = $2::uuid`

	// SQLDeviceCredentialByDevice reads the device's live credential: the one a re-enrolment or a
	// token request authenticates against.
	SQLDeviceCredentialByDevice = `
SELECT tenant_id, credential_id, device_id, credential_type, credential_origin, public_key_thumbprint,
       public_key_jwk, issued_at, expires_at, revoked_at
  FROM ops.device_credential
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid AND revoked_at IS NULL
 ORDER BY issued_at DESC
 LIMIT 1`

	// SQLMarkEnrolmentTokenUsed settles a token after a successful enrolment. The WHERE clause makes
	// a concurrent second redemption a zero-row update rather than a silent overwrite.
	SQLMarkEnrolmentTokenUsed = `
UPDATE ops.enrolment_token
   SET used_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND token_hash = $2 AND used_at IS NULL`
)

// Statement pairs a statement with what it is for, so the set can be listed and executed by name.
type Statement struct {
	Name    string
	Purpose string
	SQL     string
}

// Statements is every statement the service issues.
var Statements = []Statement{
	{Name: "set_tenant", Purpose: "RLS session tenant (transaction-local)", SQL: SQLSetTenant},
	{Name: "tenant", Purpose: "§12 tenant lifecycle and pinned region", SQL: SQLTenant},
	{Name: "resolve_enrolment_token", Purpose: "§5.1 bootstrap token lookup by (tenant, hash)", SQL: SQLResolveEnrolmentToken},
	{Name: "find_device_by_hardware_identity", Purpose: "C11 idempotency lookup", SQL: SQLFindDeviceByHardwareIdentity},
	{Name: "device", Purpose: "read one device by id", SQL: SQLDevice},
	{Name: "upsert_device", Purpose: "enrolment inserts (or refreshes) the device row", SQL: SQLUpsertDevice},
	{Name: "revoke_device_credentials", Purpose: "§2.2 rotation closes the previous credential", SQL: SQLRevokeDeviceCredentials},
	{Name: "insert_credential", Purpose: "ADR 0020 §4 register the x509 or dpop credential", SQL: SQLInsertCredential},
	{Name: "device_credential", Purpose: "read one credential by id", SQL: SQLDeviceCredential},
	{Name: "device_credential_by_device", Purpose: "read the live credential for re-enrolment/token", SQL: SQLDeviceCredentialByDevice},
	{Name: "mark_enrolment_token_used", Purpose: "§5.1 single-use token", SQL: SQLMarkEnrolmentTokenUsed},
}

// SQLStore is the database/sql implementation. NewSQL takes an already-open *sql.DB; the driver is
// registered by the binary that embeds this service (see sqlpg), because the default build carries
// no PostgreSQL driver.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open database handle. The caller owns the driver.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Close implements Store.
func (s *SQLStore) Close() error { return s.db.Close() }

// Tenant implements Store.
func (s *SQLStore) Tenant(ctx context.Context, tenantID string) (Tenant, error) {
	var t Tenant
	var ca sql.NullString
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, SQLTenant, tenantID).
			Scan(&t.Status, &t.IngestEnabled, &t.ResidencyRegion, &t.DeviceIdentity, &ca)
		if err == sql.ErrNoRows {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: tenant: %w", err)
		}
		t.TenantID = tenantID
		if ca.Valid {
			t.DeviceCAPEM = ca.String
		}
		return nil
	})
	if err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// ResolveEnrolmentToken implements Store.
func (s *SQLStore) ResolveEnrolmentToken(ctx context.Context, tenantID, tokenHash string) (EnrolmentToken, error) {
	var t EnrolmentToken
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var hwid sql.NullString
		var used, revoked sql.NullTime
		err := tx.QueryRowContext(ctx, SQLResolveEnrolmentToken, tenantID, tokenHash).
			Scan(&t.TenantID, &t.TokenHash, &hwid, &t.IssuedAt, &t.ExpiresAt, &used, &revoked)
		if err == sql.ErrNoRows {
			return ErrTokenUnknown
		}
		if err != nil {
			return fmt.Errorf("store: resolve enrolment token: %w", err)
		}
		t.HardwareIdentityHash = hwid.String
		t.UsedAt = nullTime(used)
		t.RevokedAt = nullTime(revoked)
		return nil
	})
	if err != nil {
		return EnrolmentToken{}, err
	}
	return t, nil
}

// FindDeviceByHardwareIdentity implements Store.
func (s *SQLStore) FindDeviceByHardwareIdentity(ctx context.Context, tenantID, hardwareIdentityHash string) (Device, error) {
	var d Device
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var hwid, osv, mdm, region, host, hosthash, agentv, managed sql.NullString
		var revoked sql.NullTime
		err := tx.QueryRowContext(ctx, SQLFindDeviceByHardwareIdentity, tenantID, hardwareIdentityHash).
			Scan(&d.TenantID, &d.DeviceID, &d.OS, &osv, &mdm, &hwid, &host, &hosthash, &agentv, &managed, &region, &d.EnrolledAt, &revoked)
		if err == sql.ErrNoRows {
			return ErrDeviceUnknown
		}
		if err != nil {
			return fmt.Errorf("store: find device by hardware identity: %w", err)
		}
		d.OSVersion, d.MDMID, d.HardwareIdentityHash, d.ResidencyRegion = osv.String, mdm.String, hwid.String, region.String
		d.Hostname, d.HostnameHash, d.AgentVersion, d.ManagedState = host.String, hosthash.String, agentv.String, managed.String
		d.RevokedAt = nullTime(revoked)
		return nil
	})
	if err != nil {
		return Device{}, err
	}
	return d, nil
}

// Device implements Store.
func (s *SQLStore) Device(ctx context.Context, tenantID, deviceID string) (Device, error) {
	var d Device
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var hwid, osv, mdm, region, host, hosthash, agentv, managed sql.NullString
		var revoked sql.NullTime
		err := tx.QueryRowContext(ctx, SQLDevice, tenantID, deviceID).
			Scan(&d.TenantID, &d.DeviceID, &d.OS, &osv, &mdm, &hwid, &host, &hosthash, &agentv, &managed, &region, &d.EnrolledAt, &revoked)
		if err == sql.ErrNoRows {
			return ErrDeviceUnknown
		}
		if err != nil {
			return fmt.Errorf("store: device: %w", err)
		}
		d.OSVersion, d.MDMID, d.HardwareIdentityHash, d.ResidencyRegion = osv.String, mdm.String, hwid.String, region.String
		d.Hostname, d.HostnameHash, d.AgentVersion, d.ManagedState = host.String, hosthash.String, agentv.String, managed.String
		d.RevokedAt = nullTime(revoked)
		return nil
	})
	if err != nil {
		return Device{}, err
	}
	return d, nil
}

// UpsertDevice implements Store.
func (s *SQLStore) UpsertDevice(ctx context.Context, d Device) (Device, error) {
	var out Device
	err := s.withTenant(ctx, d.TenantID, func(tx *sql.Tx) error {
		var hwid, osv, mdm, region, host, hosthash, agentv, managed sql.NullString
		var revoked sql.NullTime
		err := tx.QueryRowContext(ctx, SQLUpsertDevice,
			d.TenantID, d.DeviceID, d.OS, d.OSVersion, d.MDMID, d.HardwareIdentityHash,
			d.Hostname, d.HostnameHash, d.AgentVersion, d.ManagedState, d.ResidencyRegion).
			Scan(&out.TenantID, &out.DeviceID, &out.OS, &osv, &mdm, &hwid, &host, &hosthash, &agentv, &managed, &region, &out.EnrolledAt, &revoked)
		if err != nil {
			return fmt.Errorf("store: upsert device: %w", err)
		}
		out.OSVersion, out.MDMID, out.HardwareIdentityHash, out.ResidencyRegion = osv.String, mdm.String, hwid.String, region.String
		out.Hostname, out.HostnameHash, out.AgentVersion, out.ManagedState = host.String, hosthash.String, agentv.String, managed.String
		out.RevokedAt = nullTime(revoked)
		return nil
	})
	if err != nil {
		return Device{}, err
	}
	return out, nil
}

// IssueCredential implements Store: one transaction that revokes the live credentials and inserts
// the new one, so a rotation is atomic.
func (s *SQLStore) IssueCredential(ctx context.Context, in IssueCredential) (Credential, error) {
	var out Credential
	err := s.withTenant(ctx, in.TenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLRevokeDeviceCredentials,
			in.TenantID, in.DeviceID, in.RevokedAt); err != nil {
			return fmt.Errorf("store: revoke previous credential: %w", err)
		}
		var jwk any
		if len(in.PublicKeyJWK) > 0 {
			jwk = string(in.PublicKeyJWK)
		}
		var err error
		out, err = scanCredential(tx.QueryRowContext(ctx, SQLInsertCredential,
			in.TenantID, in.CredentialID, in.DeviceID, string(in.Type), string(in.Origin),
			in.PublicKeyThumbprint, jwk, in.IssuedAt, in.ExpiresAt))
		if err != nil {
			return fmt.Errorf("store: insert credential: %w", err)
		}
		return nil
	})
	if err != nil {
		return Credential{}, err
	}
	return out, nil
}

// DeviceCredential implements Store.
func (s *SQLStore) DeviceCredential(ctx context.Context, tenantID, credentialID string) (Credential, error) {
	var out Credential
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		out, err = scanCredential(tx.QueryRowContext(ctx, SQLDeviceCredential, tenantID, credentialID))
		if err == sql.ErrNoRows {
			return ErrCredentialUnknown
		}
		if err != nil {
			return fmt.Errorf("store: device credential: %w", err)
		}
		return nil
	})
	if err != nil {
		return Credential{}, err
	}
	return out, nil
}

// DeviceCredentialByDevice implements Store.
func (s *SQLStore) DeviceCredentialByDevice(ctx context.Context, tenantID, deviceID string) (Credential, error) {
	var out Credential
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		out, err = scanCredential(tx.QueryRowContext(ctx, SQLDeviceCredentialByDevice, tenantID, deviceID))
		if err == sql.ErrNoRows {
			return ErrCredentialUnknown
		}
		if err != nil {
			return fmt.Errorf("store: device credential by device: %w", err)
		}
		return nil
	})
	if err != nil {
		return Credential{}, err
	}
	return out, nil
}

// MarkEnrolmentTokenUsed implements Store.
func (s *SQLStore) MarkEnrolmentTokenUsed(ctx context.Context, tenantID, tokenHash string, at time.Time) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, SQLMarkEnrolmentTokenUsed, tenantID, tokenHash, at)
		if err != nil {
			return fmt.Errorf("store: mark enrolment token used: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: mark enrolment token used: %w", err)
		}
		if n != 1 {
			return ErrTokenUsed
		}
		return nil
	})
}

// UseDPoPJti is intentionally absent: RFC 9449 replay storage lives in ops.dpop_replay, which this
// service is not granted in this build, so the request path verifies the proof (signature, htm, htu,
// iat, jti presence) but does not persist the jti. See the README's "What is not built yet".

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCredential(row rowScanner) (Credential, error) {
	var c Credential
	var credType, origin string
	var jwk []byte
	var revoked sql.NullTime
	if err := row.Scan(&c.TenantID, &c.CredentialID, &c.DeviceID, &credType, &origin,
		&c.PublicKeyThumbprint, &jwk, &c.IssuedAt, &c.ExpiresAt, &revoked); err != nil {
		return Credential{}, err
	}
	c.Type = protocol.AuthMode(credType)
	c.Origin = CredentialOrigin(origin)
	if len(jwk) > 0 {
		c.PublicKeyJWK = json.RawMessage(jwk)
	}
	c.RevokedAt = nullTime(revoked)
	return c, nil
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	out := t.Time
	return &out
}

func (s *SQLStore) withTenant(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, SQLSetTenant, tenantID); err != nil {
		return fmt.Errorf("store: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Bind renders a statement's placeholders as psql-executable PREPARE/EXECUTE, so a test can run the
// exact text above against a live server without a Go driver.
func Bind(stmt string, args ...any) (string, error) {
	if strings.Count(stmt, "$") != len(args) {
		return "", fmt.Errorf("store: statement expects %d arguments, got %d", strings.Count(stmt, "$"), len(args))
	}
	literals := make([]string, 0, len(args))
	for _, a := range args {
		literals = append(literals, literal(a))
	}
	return fmt.Sprintf("%s(%s)", stmt, strings.Join(literals, ", ")), nil
}

func literal(a any) string {
	switch v := a.(type) {
	case string:
		tag := "$sac$"
		for strings.Contains(v, tag) {
			tag = "$" + tag + "$"
		}
		return tag + v + tag
	case time.Time:
		return "'" + v.UTC().Format(time.RFC3339Nano) + "'::timestamptz"
	case nil:
		return "NULL"
	default:
		return fmt.Sprintf("%v", v)
	}
}
