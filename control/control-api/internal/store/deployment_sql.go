package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Every statement the deployment and policy paths issue, in one place, for the reason sql.go gives:
// the seam with database/schema.sql is reviewable in one screen. Each runs in a transaction that
// has set app.tenant_id first (withTenant), so row-level security scopes it like the rest.
//
// The tables are the contract's (§1): ops.deployment_key, ops.identity_connection,
// ops.scim_user/ops.scim_group (read for counts only), ops.tenant.device_verification,
// ops.policy_bundle.signed_envelope, and ops.device.intune_device_id with its per-tenant partial
// unique index, which is what makes "one Intune device is one product device" a database fact.

const (
	// SQLDeploymentKeyByHash resolves a presented key. The tenant comes from the key's clear prefix,
	// so the RLS session is set before this runs, as for the enrolment token.
	SQLDeploymentKeyByHash = `
SELECT key_id::text, tenant_id::text, key_hash, label, created_by, created_at, expires_at,
       revoked_at, enrolment_count, last_used_at
  FROM ops.deployment_key
 WHERE tenant_id = $1::uuid AND key_hash = $2`

	// SQLDeploymentKeyByID reads one key by id, for the revoke path.
	SQLDeploymentKeyByID = `
SELECT key_id::text, tenant_id::text, key_hash, label, created_by, created_at, expires_at,
       revoked_at, enrolment_count, last_used_at
  FROM ops.deployment_key
 WHERE tenant_id = $1::uuid AND key_id = $2::uuid`

	// SQLListDeploymentKeys lists the tenant's keys, newest first, for the admin page.
	SQLListDeploymentKeys = `
SELECT key_id::text, tenant_id::text, key_hash, label, created_by, created_at, expires_at,
       revoked_at, enrolment_count, last_used_at
  FROM ops.deployment_key
 WHERE tenant_id = $1::uuid
 ORDER BY created_at DESC, key_id`

	// SQLInsertDeploymentKey stores a freshly minted key's hash. $7 is NULL for a key that does not
	// expire, which is the default: an MDM package is reused for as long as it is assigned.
	SQLInsertDeploymentKey = `
INSERT INTO ops.deployment_key (key_id, tenant_id, key_hash, label, created_by, created_at, expires_at)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::timestamptz, $7::timestamptz)
RETURNING key_id::text, tenant_id::text, key_hash, label, created_by, created_at, expires_at,
          revoked_at, enrolment_count, last_used_at`

	// SQLRevokeDeploymentKey revokes a live key. `revoked_at IS NULL` makes a second revoke a
	// zero-row update, so the first revocation's time and audit row stand.
	SQLRevokeDeploymentKey = `
UPDATE ops.deployment_key
   SET revoked_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND key_id = $2::uuid AND revoked_at IS NULL
RETURNING key_id::text, tenant_id::text, key_hash, label, created_by, created_at, expires_at,
          revoked_at, enrolment_count, last_used_at`

	// SQLTouchDeploymentKey counts one enrolment. last_used_at only moves forward, so a slow
	// request finishing late cannot make the key look older than it is.
	SQLTouchDeploymentKey = `
UPDATE ops.deployment_key
   SET enrolment_count = enrolment_count + 1,
       last_used_at    = GREATEST(COALESCE(last_used_at, $3::timestamptz), $3::timestamptz)
 WHERE tenant_id = $1::uuid AND key_id = $2::uuid`

	// SQLDeviceVerification reads the tenant's mode and the Entra tenant of its active Entra
	// connection, the one customer tenant an Intune lookup may be made in (contract §5).
	SQLDeviceVerification = `
SELECT t.device_verification,
       COALESCE((SELECT c.entra_tenant_id
                   FROM ops.identity_connection c
                  WHERE c.tenant_id = t.tenant_id AND c.provider = 'entra' AND c.status = 'active'
                  ORDER BY c.activated_at DESC NULLS LAST, c.created_at DESC
                  LIMIT 1), '')
  FROM ops.tenant t
 WHERE t.tenant_id = $1::uuid`

	// SQLSetDeviceVerification changes the mode. The CHECK constraint on the column is the
	// vocabulary's last word; the caller has already refused anything else.
	SQLSetDeviceVerification = `
UPDATE ops.tenant
   SET device_verification = $2, updated_at = now()
 WHERE tenant_id = $1::uuid`

	// SQLIdentityConnectionSummary is the connection the admin page names: the active one if there
	// is one, else the newest.
	SQLIdentityConnectionSummary = `
SELECT provider, status, COALESCE(entra_tenant_id, ''), COALESCE(issuer, '')
  FROM ops.identity_connection
 WHERE tenant_id = $1::uuid
 ORDER BY (status = 'active') DESC, created_at DESC
 LIMIT 1`

	// SQLDeviceSummary counts the enrolled (unrevoked) devices and the latest enrolment.
	SQLDeviceSummary = `
SELECT count(*) FILTER (WHERE revoked_at IS NULL), max(enrolled_at)
  FROM ops.device
 WHERE tenant_id = $1::uuid`

	// SQLScimSummary counts what SCIM has provisioned. It reads counts and timestamps only, never a
	// sealed resource.
	SQLScimSummary = `
SELECT (SELECT count(*) FROM ops.scim_user  WHERE tenant_id = $1::uuid AND active),
       (SELECT count(*) FROM ops.scim_group WHERE tenant_id = $1::uuid),
       GREATEST((SELECT max(updated_at) FROM ops.scim_user  WHERE tenant_id = $1::uuid),
                (SELECT max(updated_at) FROM ops.scim_group WHERE tenant_id = $1::uuid))`

	// SQLFindDeviceByIntuneID is the Intune idempotency lookup, served by the partial unique index
	// on (tenant_id, intune_device_id).
	SQLFindDeviceByIntuneID = `
SELECT tenant_id::text, device_id::text, os, os_version, mdm_id, hardware_identity_hash,
       hostname, hostname_hash, agent_version, managed_state,
       residency_region, enrolled_at, revoked_at, intune_device_id
  FROM ops.device
 WHERE tenant_id = $1::uuid AND intune_device_id = $2`

	// SQLSetDeviceIntuneID binds a verified Intune id. A second device claiming the same id fails
	// the unique index, which the caller reports as ErrIntuneDeviceConflict.
	SQLSetDeviceIntuneID = `
UPDATE ops.device
   SET intune_device_id = $3
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid`

	// SQLInsertAudit writes one audit row. prev_hash and row_hash are absent on purpose: the
	// ops.audit_chain() trigger computes both, so no caller can forge a link.
	SQLInsertAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail, occurred_at)
VALUES ($1::uuid, $2, $3, $4, $5, nullif($6, ''), $7::jsonb, $8::timestamptz)`

	// SQLPolicyTenant reads the tenant fields a bundle is composed from.
	SQLPolicyTenant = `
SELECT status, ingest_enabled, ceiling_mode
  FROM ops.tenant
 WHERE tenant_id = $1::uuid`

	// SQLInterceptionHosts is the tool catalogue's TLS hosts: the interception scope a composed
	// bundle names, exactly what the lab passes sac-bundle as --hosts. It decides what the device
	// may decrypt, never what counts as an AI tool.
	SQLInterceptionHosts = `
SELECT DISTINCT lower(evidence->>'host') AS host
  FROM ref.tool_catalogue
 WHERE signal_kind = 'tls' AND COALESCE(evidence->>'host', '') <> ''
 ORDER BY host`

	// SQLServableClassifierRelease picks the release a bundle names: the latest promoted enforcing
	// release, else the newest in shadow. A retired or rolled-back release is never named.
	SQLServableClassifierRelease = `
SELECT release_version, state
  FROM ref.classifier_release
 WHERE state IN ('enforcing', 'shadow')
 ORDER BY (state = 'enforcing') DESC, promoted_at DESC NULLS LAST, created_at DESC
 LIMIT 1`

	// SQLLockTenantPolicy serialises minting per tenant for the rest of the transaction, so two
	// replicas that see the same changed inputs write one version, not two.
	SQLLockTenantPolicy = `SELECT pg_advisory_xact_lock(hashtextextended('ops.policy_bundle:' || $1, 0))`

	// SQLLatestPolicyBundle reads the highest version. signed_envelope is NULL only for a row
	// written before the column existed; such a row is not served but still bounds the next version.
	SQLLatestPolicyBundle = `
SELECT bundle_version, scope_matrix, destination_allowlist, feature_state, spool_bounds,
       classifier_release, retention_class, signature_kid, signed_digest, signed_envelope,
       effective_from, created_by, created_at
  FROM ops.policy_bundle
 WHERE tenant_id = $1::uuid
 ORDER BY bundle_version DESC
 LIMIT 1`

	// SQLInsertPolicyBundle writes a signed bundle. The policy_bundle_ceiling trigger refuses one
	// whose scope matrix exceeds the tenant ceiling, so the ceiling holds even against this writer.
	SQLInsertPolicyBundle = `
INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix, destination_allowlist,
                               classifier_release, retention_class, spool_bounds, feature_state,
                               signature_kid, signed_digest, effective_from, created_by, signed_envelope)
VALUES ($1::uuid, $2::bigint, $3::jsonb, $4::jsonb, $5, $6, $7::jsonb, $8::jsonb, $9, $10,
        $11::timestamptz, $12, $13::bytea)`
)

// DeploymentStatements names the deployment and policy statements, so a live test can execute them.
var DeploymentStatements = []Statement{
	{Name: "deployment_key_by_hash", Purpose: "contract §5 deployment-key lookup by (tenant, hash)", SQL: SQLDeploymentKeyByHash},
	{Name: "deployment_key_by_id", Purpose: "read one deployment key", SQL: SQLDeploymentKeyByID},
	{Name: "list_deployment_keys", Purpose: "admin page key list", SQL: SQLListDeploymentKeys},
	{Name: "insert_deployment_key", Purpose: "mint a key per package download", SQL: SQLInsertDeploymentKey},
	{Name: "revoke_deployment_key", Purpose: "admin revoke, first revocation stands", SQL: SQLRevokeDeploymentKey},
	{Name: "touch_deployment_key", Purpose: "enrolment_count and last_used_at", SQL: SQLTouchDeploymentKey},
	{Name: "device_verification", Purpose: "tenant mode and active Entra tenant", SQL: SQLDeviceVerification},
	{Name: "set_device_verification", Purpose: "admin verification setting", SQL: SQLSetDeviceVerification},
	{Name: "identity_connection_summary", Purpose: "admin page connection", SQL: SQLIdentityConnectionSummary},
	{Name: "device_summary", Purpose: "admin page device count", SQL: SQLDeviceSummary},
	{Name: "scim_summary", Purpose: "admin page SCIM counts", SQL: SQLScimSummary},
	{Name: "find_device_by_intune_id", Purpose: "Intune idempotency lookup", SQL: SQLFindDeviceByIntuneID},
	{Name: "set_device_intune_id", Purpose: "bind a verified Intune id", SQL: SQLSetDeviceIntuneID},
	{Name: "insert_audit", Purpose: "audit row, chained by trigger", SQL: SQLInsertAudit},
	{Name: "policy_tenant", Purpose: "docs/02 §5.2 bundle inputs: tenant", SQL: SQLPolicyTenant},
	{Name: "interception_hosts", Purpose: "docs/02 §5.2 bundle inputs: catalogue hosts", SQL: SQLInterceptionHosts},
	{Name: "servable_classifier_release", Purpose: "docs/02 §5.2 bundle inputs: classifier release", SQL: SQLServableClassifierRelease},
	{Name: "lock_tenant_policy", Purpose: "serialise minting per tenant", SQL: SQLLockTenantPolicy},
	{Name: "latest_policy_bundle", Purpose: "docs/02 §5.2 the bundle in force", SQL: SQLLatestPolicyBundle},
	{Name: "insert_policy_bundle", Purpose: "docs/02 §5.2 a newly signed bundle", SQL: SQLInsertPolicyBundle},
}

// Compile-time proof that both stores implement the deployment and policy seams.
var (
	_ DeploymentStore = (*SQLStore)(nil)
	_ PolicyStore     = (*SQLStore)(nil)
	_ DeploymentStore = (*Memory)(nil)
	_ PolicyStore     = (*Memory)(nil)
)

func scanDeploymentKey(row rowScanner) (DeploymentKey, error) {
	var k DeploymentKey
	var expires, revoked, used sql.NullTime
	if err := row.Scan(&k.KeyID, &k.TenantID, &k.KeyHash, &k.Label, &k.CreatedBy, &k.CreatedAt,
		&expires, &revoked, &k.EnrolmentCount, &used); err != nil {
		return DeploymentKey{}, err
	}
	k.ExpiresAt, k.RevokedAt, k.LastUsedAt = nullTime(expires), nullTime(revoked), nullTime(used)
	return k, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// DeploymentKeyByHash implements DeploymentStore.
func (s *SQLStore) DeploymentKeyByHash(ctx context.Context, tenantID, keyHash string) (DeploymentKey, error) {
	var k DeploymentKey
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		k, err = scanDeploymentKey(tx.QueryRowContext(ctx, SQLDeploymentKeyByHash, tenantID, keyHash))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeploymentKeyUnknown
		}
		if err != nil {
			return fmt.Errorf("store: deployment key: %w", err)
		}
		return nil
	})
	return k, err
}

// DeviceVerification implements DeploymentStore.
func (s *SQLStore) DeviceVerification(ctx context.Context, tenantID string) (DeviceVerification, error) {
	var v DeviceVerification
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, SQLDeviceVerification, tenantID).Scan(&v.Mode, &v.EntraTenantID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: device verification: %w", err)
		}
		return nil
	})
	return v, err
}

// FindDeviceByIntuneID implements DeploymentStore.
func (s *SQLStore) FindDeviceByIntuneID(ctx context.Context, tenantID, intuneDeviceID string) (Device, error) {
	var d Device
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var hwid, osv, mdm, region, host, hosthash, agentv, managed, intune sql.NullString
		var revoked sql.NullTime
		err := tx.QueryRowContext(ctx, SQLFindDeviceByIntuneID, tenantID, intuneDeviceID).
			Scan(&d.TenantID, &d.DeviceID, &d.OS, &osv, &mdm, &hwid, &host, &hosthash, &agentv, &managed,
				&region, &d.EnrolledAt, &revoked, &intune)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeviceUnknown
		}
		if err != nil {
			return fmt.Errorf("store: find device by intune id: %w", err)
		}
		d.OSVersion, d.MDMID, d.HardwareIdentityHash, d.ResidencyRegion = osv.String, mdm.String, hwid.String, region.String
		d.Hostname, d.HostnameHash, d.AgentVersion, d.ManagedState = host.String, hosthash.String, agentv.String, managed.String
		d.RevokedAt = nullTime(revoked)
		d.IntuneDeviceID = intune.String
		return nil
	})
	return d, err
}

// SetDeviceIntuneID implements DeploymentStore.
func (s *SQLStore) SetDeviceIntuneID(ctx context.Context, tenantID, deviceID, intuneDeviceID string) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, SQLSetDeviceIntuneID, tenantID, deviceID, intuneDeviceID)
		if isUniqueViolation(err) {
			return ErrIntuneDeviceConflict
		}
		if err != nil {
			return fmt.Errorf("store: set device intune id: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrDeviceUnknown
		}
		return nil
	})
}

// RecordDeploymentEnrolment implements DeploymentStore.
func (s *SQLStore) RecordDeploymentEnrolment(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLTouchDeploymentKey, tenantID, keyID, at.UTC()); err != nil {
			return fmt.Errorf("store: count deployment enrolment: %w", err)
		}
		return insertAudit(ctx, tx, audit)
	})
}

// Audit implements DeploymentStore.
func (s *SQLStore) Audit(ctx context.Context, audit AuditEntry) error {
	return s.withTenant(ctx, audit.TenantID, func(tx *sql.Tx) error {
		return insertAudit(ctx, tx, audit)
	})
}

// CreateDeploymentKey implements DeploymentStore.
func (s *SQLStore) CreateDeploymentKey(ctx context.Context, k DeploymentKey, audit AuditEntry) (DeploymentKey, error) {
	var out DeploymentKey
	err := s.withTenant(ctx, k.TenantID, func(tx *sql.Tx) error {
		var err error
		out, err = scanDeploymentKey(tx.QueryRowContext(ctx, SQLInsertDeploymentKey,
			k.KeyID, k.TenantID, k.KeyHash, k.Label, k.CreatedBy, k.CreatedAt.UTC(), nullableTime(k.ExpiresAt)))
		if err != nil {
			return fmt.Errorf("store: insert deployment key: %w", err)
		}
		return insertAudit(ctx, tx, audit)
	})
	return out, err
}

// RevokeDeploymentKey implements DeploymentStore.
func (s *SQLStore) RevokeDeploymentKey(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) (DeploymentKey, error) {
	var out DeploymentKey
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		out, err = scanDeploymentKey(tx.QueryRowContext(ctx, SQLRevokeDeploymentKey, tenantID, keyID, at.UTC()))
		if errors.Is(err, sql.ErrNoRows) {
			// Either already revoked (the first revocation stands, with its audit row) or unknown.
			out, err = scanDeploymentKey(tx.QueryRowContext(ctx, SQLDeploymentKeyByID, tenantID, keyID))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDeploymentKeyUnknown
			}
			if err != nil {
				return fmt.Errorf("store: read deployment key: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: revoke deployment key: %w", err)
		}
		return insertAudit(ctx, tx, audit)
	})
	return out, err
}

// SetDeviceVerification implements DeploymentStore.
func (s *SQLStore) SetDeviceVerification(ctx context.Context, tenantID, mode string, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var current, entra string
		err := tx.QueryRowContext(ctx, SQLDeviceVerification, tenantID).Scan(&current, &entra)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: device verification: %w", err)
		}
		if mode == VerificationIntune && entra == "" {
			return ErrNoEntraConnection
		}
		if _, err := tx.ExecContext(ctx, SQLSetDeviceVerification, tenantID, mode); err != nil {
			return fmt.Errorf("store: set device verification: %w", err)
		}
		if audit.Detail == nil {
			audit.Detail = map[string]any{}
		}
		audit.Detail["previous"] = current
		return insertAudit(ctx, tx, audit)
	})
}

// DeploymentSummary implements DeploymentStore.
func (s *SQLStore) DeploymentSummary(ctx context.Context, tenantID string) (DeploymentSummary, error) {
	var out DeploymentSummary
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var entra string
		err := tx.QueryRowContext(ctx, SQLDeviceVerification, tenantID).Scan(&out.DeviceVerification, &entra)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: device verification: %w", err)
		}
		var c IdentityConnection
		err = tx.QueryRowContext(ctx, SQLIdentityConnectionSummary, tenantID).
			Scan(&c.Provider, &c.Status, &c.EntraTenantID, &c.Issuer)
		switch {
		case err == nil:
			out.Connection = &c
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: identity connection: %w", err)
		}
		rows, err := tx.QueryContext(ctx, SQLListDeploymentKeys, tenantID)
		if err != nil {
			return fmt.Errorf("store: list deployment keys: %w", err)
		}
		for rows.Next() {
			k, err := scanDeploymentKey(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("store: scan deployment key: %w", err)
			}
			out.Keys = append(out.Keys, k)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("store: list deployment keys: %w", err)
		}
		rows.Close()
		var lastEnrolled sql.NullTime
		if err := tx.QueryRowContext(ctx, SQLDeviceSummary, tenantID).Scan(&out.DevicesEnrolled, &lastEnrolled); err != nil {
			return fmt.Errorf("store: device summary: %w", err)
		}
		out.LastEnrolledAt = nullTime(lastEnrolled)
		var lastProvisioned sql.NullTime
		if err := tx.QueryRowContext(ctx, SQLScimSummary, tenantID).Scan(&out.ScimUsers, &out.ScimGroups, &lastProvisioned); err != nil {
			return fmt.Errorf("store: scim summary: %w", err)
		}
		out.LastProvisionedAt = nullTime(lastProvisioned)
		return nil
	})
	return out, err
}

// PolicyInputs implements PolicyStore.
func (s *SQLStore) PolicyInputs(ctx context.Context, tenantID string) (PolicyInputs, error) {
	var in PolicyInputs
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		t := PolicyTenant{TenantID: tenantID}
		err := tx.QueryRowContext(ctx, SQLPolicyTenant, tenantID).Scan(&t.Status, &t.IngestEnabled, &t.CeilingMode)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: policy tenant: %w", err)
		}
		in.Tenant = t
		rows, err := tx.QueryContext(ctx, SQLInterceptionHosts)
		if err != nil {
			return fmt.Errorf("store: interception hosts: %w", err)
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan interception host: %w", err)
			}
			in.InterceptionHosts = append(in.InterceptionHosts, h)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("store: interception hosts: %w", err)
		}
		rows.Close()
		var r ClassifierRelease
		err = tx.QueryRowContext(ctx, SQLServableClassifierRelease).Scan(&r.Version, &r.State)
		switch {
		case err == nil:
			in.Classifier = &r
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: classifier release: %w", err)
		}
		return nil
	})
	return in, err
}

func scanPolicyBundle(row rowScanner, tenantID string) (PolicyBundle, error) {
	b := PolicyBundle{TenantID: tenantID}
	var scope, allow, feature, spool []byte
	if err := row.Scan(&b.Version, &scope, &allow, &feature, &spool, &b.ClassifierRelease, &b.RetentionClass,
		&b.SignatureKID, &b.SignedDigest, &b.SignedEnvelope, &b.EffectiveFrom, &b.CreatedBy, &b.CreatedAt); err != nil {
		return PolicyBundle{}, err
	}
	b.ScopeMatrix, b.DestinationAllowlist, b.FeatureState, b.SpoolBounds = scope, allow, feature, spool
	return b, nil
}

// LatestPolicyBundle implements PolicyStore.
func (s *SQLStore) LatestPolicyBundle(ctx context.Context, tenantID string) (PolicyBundle, error) {
	var out PolicyBundle
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		out, err = scanPolicyBundle(tx.QueryRowContext(ctx, SQLLatestPolicyBundle, tenantID), tenantID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoPolicyBundle
		}
		if err != nil {
			return fmt.Errorf("store: latest policy bundle: %w", err)
		}
		return nil
	})
	return out, err
}

// MintPolicyBundle implements PolicyStore.
func (s *SQLStore) MintPolicyBundle(ctx context.Context, tenantID string, decide func(latest *PolicyBundle) (MintDecision, error)) (PolicyBundle, error) {
	var out PolicyBundle
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLLockTenantPolicy, tenantID); err != nil {
			return fmt.Errorf("store: lock tenant policy: %w", err)
		}
		var latest *PolicyBundle
		b, err := scanPolicyBundle(tx.QueryRowContext(ctx, SQLLatestPolicyBundle, tenantID), tenantID)
		switch {
		case err == nil:
			latest = &b
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: latest policy bundle: %w", err)
		}
		d, err := decide(latest)
		if err != nil {
			return err
		}
		if d.Bundle == nil {
			if latest == nil {
				return ErrNoPolicyBundle
			}
			out = *latest
			return nil
		}
		nb := *d.Bundle
		nb.TenantID = tenantID
		if _, err := tx.ExecContext(ctx, SQLInsertPolicyBundle,
			tenantID, nb.Version, jsonText(nb.ScopeMatrix, "{}"), jsonText(nb.DestinationAllowlist, "[]"),
			nb.ClassifierRelease, nb.RetentionClass, jsonText(nb.SpoolBounds, "{}"), jsonText(nb.FeatureState, "{}"),
			nb.SignatureKID, nb.SignedDigest, nb.EffectiveFrom.UTC(), nb.CreatedBy, nb.SignedEnvelope); err != nil {
			return fmt.Errorf("store: insert policy bundle: %w", err)
		}
		if d.Audit != nil {
			if err := insertAudit(ctx, tx, *d.Audit); err != nil {
				return err
			}
		}
		nb.CreatedAt = nb.EffectiveFrom
		out = nb
		return nil
	})
	return out, err
}

func jsonText(raw []byte, empty string) string {
	if len(raw) == 0 {
		return empty
	}
	return string(raw)
}

func insertAudit(ctx context.Context, tx *sql.Tx, a AuditEntry) error {
	detail, err := a.detailJSON()
	if err != nil {
		return fmt.Errorf("store: audit detail: %w", err)
	}
	at := a.OccurredAt
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := tx.ExecContext(ctx, SQLInsertAudit, a.TenantID, a.ActorType, a.ActorID, a.Action,
		a.ObjectType, a.ObjectID, detail, at.UTC()); err != nil {
		return fmt.Errorf("store: insert audit: %w", err)
	}
	return nil
}

// isUniqueViolation reports SQLSTATE 23505 without importing a driver: pgx's error exposes
// SQLState(), and so does every driver worth using.
func isUniqueViolation(err error) bool {
	var coded interface{ SQLState() string }
	return err != nil && errors.As(err, &coded) && coded.SQLState() == "23505"
}
