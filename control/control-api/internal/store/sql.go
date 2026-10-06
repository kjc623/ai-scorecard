package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The device and credential statements. Every statement names the tenant as $1 as well as being
// scoped by row-level security, so a session left with the wrong tenant still reads nothing.
const (
	// SQLSetTenant sets the row-level-security tenant for the rest of the transaction only, so a
	// pooled connection cannot carry one tenant's session into the next request.
	SQLSetTenant = `SELECT set_config('app.tenant_id', $1, true)`

	SQLTenant = `
SELECT status, ingest_enabled, residency_region, device_identity
  FROM ops.tenant
 WHERE tenant_id = $1::uuid`

	SQLFindDeviceByHardwareIdentity = `
SELECT ` + deviceColumns + `
  FROM ops.device
 WHERE tenant_id = $1::uuid AND hardware_identity_hash = $2`

	SQLDevice = `
SELECT ` + deviceColumns + `
  FROM ops.device
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid`

	// SQLUpsertDevice inserts a device or refreshes the one with the same id. COALESCE keeps a
	// stored value the request did not carry, so a re-enrolment without a hostname keeps it. The
	// Intune binding is never written here.
	SQLUpsertDevice = `
INSERT INTO ops.device (tenant_id, device_id, os, os_version, hardware_identity_hash,
                        hostname, hostname_hash, agent_version, managed_state, residency_region)
VALUES ($1::uuid, $2::uuid, $3, nullif($4, ''), nullif($5, ''),
        nullif($6, ''), nullif($7, ''), nullif($8, ''), coalesce(nullif($9, ''), 'unknown'), nullif($10, ''))
ON CONFLICT (tenant_id, device_id) DO UPDATE
   SET os                     = EXCLUDED.os,
       os_version             = coalesce(EXCLUDED.os_version, ops.device.os_version),
       hardware_identity_hash = coalesce(EXCLUDED.hardware_identity_hash, ops.device.hardware_identity_hash),
       hostname               = coalesce(EXCLUDED.hostname, ops.device.hostname),
       hostname_hash          = coalesce(EXCLUDED.hostname_hash, ops.device.hostname_hash),
       agent_version          = coalesce(EXCLUDED.agent_version, ops.device.agent_version),
       managed_state          = CASE WHEN nullif($9, '') IS NULL THEN ops.device.managed_state
                                     ELSE EXCLUDED.managed_state END,
       residency_region       = coalesce(EXCLUDED.residency_region, ops.device.residency_region)
RETURNING ` + deviceColumns

	// SQLRevokeDeviceCredentials closes every live credential of the device; it runs in the same
	// transaction as SQLInsertCredential.
	SQLRevokeDeviceCredentials = `
UPDATE ops.device_credential
   SET revoked_at = $3::timestamptz, revoked_reason = 'rotated'
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid AND revoked_at IS NULL`

	SQLInsertCredential = `
INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, public_key_thumbprint, issued_at, expires_at)
VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::timestamptz, $6::timestamptz)`

	SQLDeviceCredential = `
SELECT tenant_id::text, credential_id::text, device_id::text, public_key_thumbprint, issued_at, expires_at, revoked_at
  FROM ops.device_credential
 WHERE tenant_id = $1::uuid AND credential_id = $2::uuid`
)

const deviceColumns = `tenant_id::text, device_id::text, os, os_version, hardware_identity_hash,
       hostname, hostname_hash, agent_version, managed_state, residency_region,
       enrolled_at, revoked_at, intune_device_id`

// Statement names a statement, so the live test can execute every one by name.
type Statement struct {
	Name string
	SQL  string
}

// Statements is every statement the store issues.
var Statements = []Statement{
	{"set_tenant", SQLSetTenant},
	{"tenant", SQLTenant},
	{"find_device_by_hardware_identity", SQLFindDeviceByHardwareIdentity},
	{"device", SQLDevice},
	{"upsert_device", SQLUpsertDevice},
	{"revoke_device_credentials", SQLRevokeDeviceCredentials},
	{"insert_credential", SQLInsertCredential},
	{"device_credential", SQLDeviceCredential},
	{"collector_vocabulary", SQLCollectorVocabulary},
	{"upsert_collector_state", SQLUpsertCollectorState},
	{"set_device_health", SQLSetDeviceHealth},
	{"deployment_key_by_hash", SQLDeploymentKeyByHash},
	{"deployment_key_by_id", SQLDeploymentKeyByID},
	{"list_deployment_keys", SQLListDeploymentKeys},
	{"insert_deployment_key", SQLInsertDeploymentKey},
	{"revoke_deployment_key", SQLRevokeDeploymentKey},
	{"touch_deployment_key", SQLTouchDeploymentKey},
	{"device_verification", SQLDeviceVerification},
	{"set_device_verification", SQLSetDeviceVerification},
	{"identity_connection_summary", SQLIdentityConnectionSummary},
	{"device_summary", SQLDeviceSummary},
	{"scim_summary", SQLScimSummary},
	{"find_device_by_intune_id", SQLFindDeviceByIntuneID},
	{"set_device_intune_id", SQLSetDeviceIntuneID},
	{"insert_audit", SQLInsertAudit},
	{"policy_tenant", SQLPolicyTenant},
	{"interception_hosts", SQLInterceptionHosts},
	{"lock_tenant_policy", SQLLockTenantPolicy},
	{"latest_policy_bundle", SQLLatestPolicyBundle},
	{"insert_policy_bundle", SQLInsertPolicyBundle},
}

// SQLStore is the PostgreSQL implementation of Store.
type SQLStore struct {
	db *sql.DB
}

// NewSQL wraps an open pool. The caller owns and closes it.
func NewSQL(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// Ping implements Store.
func (s *SQLStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Tenant implements Store.
func (s *SQLStore) Tenant(ctx context.Context, tenantID string) (Tenant, error) {
	t := Tenant{TenantID: tenantID}
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, SQLTenant, tenantID).
			Scan(&t.Status, &t.IngestEnabled, &t.ResidencyRegion, &t.DeviceIdentity)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: tenant: %w", err)
		}
		return nil
	})
	return t, err
}

// FindDeviceByHardwareIdentity implements Store.
func (s *SQLStore) FindDeviceByHardwareIdentity(ctx context.Context, tenantID, hardwareIdentityHash string) (Device, error) {
	return s.device(ctx, tenantID, SQLFindDeviceByHardwareIdentity, hardwareIdentityHash)
}

// Device implements Store.
func (s *SQLStore) Device(ctx context.Context, tenantID, deviceID string) (Device, error) {
	return s.device(ctx, tenantID, SQLDevice, deviceID)
}

// FindDeviceByIntuneID implements Store.
func (s *SQLStore) FindDeviceByIntuneID(ctx context.Context, tenantID, intuneDeviceID string) (Device, error) {
	return s.device(ctx, tenantID, SQLFindDeviceByIntuneID, intuneDeviceID)
}

func (s *SQLStore) device(ctx context.Context, tenantID, query, key string) (Device, error) {
	var d Device
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		d, err = scanDevice(tx.QueryRowContext(ctx, query, tenantID, key))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeviceUnknown
		}
		if err != nil {
			return fmt.Errorf("store: device: %w", err)
		}
		return nil
	})
	return d, err
}

// UpsertDevice implements Store.
func (s *SQLStore) UpsertDevice(ctx context.Context, d Device) (Device, error) {
	var out Device
	err := s.withTenant(ctx, d.TenantID, func(tx *sql.Tx) error {
		var err error
		out, err = scanDevice(tx.QueryRowContext(ctx, SQLUpsertDevice,
			d.TenantID, d.DeviceID, d.OS, d.OSVersion, d.HardwareIdentityHash,
			d.Hostname, d.HostnameHash, d.AgentVersion, d.ManagedState, d.ResidencyRegion))
		if err != nil {
			return fmt.Errorf("store: upsert device: %w", err)
		}
		return nil
	})
	return out, err
}

// IssueCredential implements Store.
func (s *SQLStore) IssueCredential(ctx context.Context, c Credential) error {
	return s.withTenant(ctx, c.TenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLRevokeDeviceCredentials, c.TenantID, c.DeviceID, c.IssuedAt.UTC()); err != nil {
			return fmt.Errorf("store: revoke previous credential: %w", err)
		}
		if _, err := tx.ExecContext(ctx, SQLInsertCredential, c.TenantID, c.CredentialID, c.DeviceID,
			c.PublicKeyThumbprint, c.IssuedAt.UTC(), c.ExpiresAt.UTC()); err != nil {
			return fmt.Errorf("store: insert credential: %w", err)
		}
		return nil
	})
}

// DeviceCredential implements Store.
func (s *SQLStore) DeviceCredential(ctx context.Context, tenantID, credentialID string) (Credential, error) {
	var c Credential
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var revoked sql.NullTime
		err := tx.QueryRowContext(ctx, SQLDeviceCredential, tenantID, credentialID).
			Scan(&c.TenantID, &c.CredentialID, &c.DeviceID, &c.PublicKeyThumbprint, &c.IssuedAt, &c.ExpiresAt, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredentialUnknown
		}
		if err != nil {
			return fmt.Errorf("store: device credential: %w", err)
		}
		c.RevokedAt = nullTime(revoked)
		return nil
	})
	return c, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDevice(row rowScanner) (Device, error) {
	var d Device
	var osv, hwid, host, hosthash, agentv, region, intune sql.NullString
	var revoked sql.NullTime
	if err := row.Scan(&d.TenantID, &d.DeviceID, &d.OS, &osv, &hwid, &host, &hosthash, &agentv,
		&d.ManagedState, &region, &d.EnrolledAt, &revoked, &intune); err != nil {
		return Device{}, err
	}
	d.OSVersion, d.HardwareIdentityHash, d.ResidencyRegion = osv.String, hwid.String, region.String
	d.Hostname, d.HostnameHash, d.AgentVersion, d.IntuneDeviceID = host.String, hosthash.String, agentv.String, intune.String
	d.RevokedAt = nullTime(revoked)
	return d, nil
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	out := t.Time
	return &out
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// withTenant runs fn in a transaction whose row-level-security tenant is tenantID.
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

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
