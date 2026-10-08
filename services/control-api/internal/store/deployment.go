package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The deployment-key, admin-summary, audit and policy statements.
const (
	SQLDeploymentKeyByHash = `
SELECT ` + keyColumns + `
  FROM ops.deployment_key
 WHERE tenant_id = $1::uuid AND key_hash = $2`

	SQLDeploymentKeyByID = `
SELECT ` + keyColumns + `
  FROM ops.deployment_key
 WHERE tenant_id = $1::uuid AND key_id = $2::uuid`

	SQLListDeploymentKeys = `
SELECT ` + keyColumns + `
  FROM ops.deployment_key
 WHERE tenant_id = $1::uuid
 ORDER BY created_at DESC, key_id`

	// SQLInsertDeploymentKey stores a minted key's hash. $7 is NULL for a key that does not expire.
	SQLInsertDeploymentKey = `
INSERT INTO ops.deployment_key (key_id, tenant_id, key_hash, label, created_by, created_at, expires_at)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::timestamptz, $7::timestamptz)
RETURNING ` + keyColumns

	// SQLRevokeDeploymentKey revokes a live key; a second revoke is a zero-row update, so the first
	// revocation's time and audit row stand.
	SQLRevokeDeploymentKey = `
UPDATE ops.deployment_key
   SET revoked_at = $3::timestamptz
 WHERE tenant_id = $1::uuid AND key_id = $2::uuid AND revoked_at IS NULL
RETURNING ` + keyColumns

	// SQLTouchDeploymentKey counts one enrolment. last_used_at only moves forward.
	SQLTouchDeploymentKey = `
UPDATE ops.deployment_key
   SET enrolment_count = enrolment_count + 1,
       last_used_at    = greatest(coalesce(last_used_at, $3::timestamptz), $3::timestamptz)
 WHERE tenant_id = $1::uuid AND key_id = $2::uuid`

	// SQLDeviceVerification reads the tenant's mode and the Entra tenant of its active Entra
	// connection, the one customer tenant an Intune lookup may be made in.
	SQLDeviceVerification = `
SELECT t.device_verification,
       coalesce((SELECT c.entra_tenant_id
                   FROM ops.identity_connection c
                  WHERE c.tenant_id = t.tenant_id AND c.provider = 'entra' AND c.status = 'active'
                  ORDER BY c.activated_at DESC NULLS LAST, c.created_at DESC
                  LIMIT 1), '')
  FROM ops.tenant t
 WHERE t.tenant_id = $1::uuid`

	SQLSetDeviceVerification = `
UPDATE ops.tenant
   SET device_verification = $2, updated_at = now()
 WHERE tenant_id = $1::uuid`

	// SQLIdentityConnectionSummary names the active connection if there is one, else the newest.
	SQLIdentityConnectionSummary = `
SELECT provider, status, coalesce(entra_tenant_id, ''), coalesce(issuer, '')
  FROM ops.identity_connection
 WHERE tenant_id = $1::uuid
 ORDER BY (status = 'active') DESC, created_at DESC
 LIMIT 1`

	SQLDeviceSummary = `
SELECT count(*) FILTER (WHERE revoked_at IS NULL), max(enrolled_at)
  FROM ops.device
 WHERE tenant_id = $1::uuid`

	// SQLScimSummary reads counts and timestamps only, never a sealed resource.
	SQLScimSummary = `
SELECT (SELECT count(*) FROM ops.scim_user  WHERE tenant_id = $1::uuid AND active),
       (SELECT count(*) FROM ops.scim_group WHERE tenant_id = $1::uuid),
       greatest((SELECT max(updated_at) FROM ops.scim_user  WHERE tenant_id = $1::uuid),
                (SELECT max(updated_at) FROM ops.scim_group WHERE tenant_id = $1::uuid))`

	SQLFindDeviceByIntuneID = `
SELECT ` + deviceColumns + `
  FROM ops.device
 WHERE tenant_id = $1::uuid AND intune_device_id = $2`

	// SQLSetDeviceIntuneID binds a verified Intune id. The per-tenant unique index refuses a second
	// device claiming the same id.
	SQLSetDeviceIntuneID = `
UPDATE ops.device
   SET intune_device_id = $3
 WHERE tenant_id = $1::uuid AND device_id = $2::uuid`

	// SQLInsertAudit writes one audit row; the table's trigger computes the chain hashes.
	SQLInsertAudit = `
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail, occurred_at)
VALUES ($1::uuid, $2, $3, $4, $5, nullif($6, ''), $7::jsonb, $8::timestamptz)`

	SQLPolicyTenant = `
SELECT status, ingest_enabled, ceiling_mode, coalesce(collection_mode, ceiling_mode), tls_inspection
  FROM ops.tenant
 WHERE tenant_id = $1::uuid`

	// SQLInterceptionHosts is the tool catalogue's TLS hosts: what the device may decrypt.
	SQLInterceptionHosts = `
SELECT DISTINCT lower(evidence->>'host') AS host
  FROM ref.tool_catalogue
 WHERE signal_kind = 'tls' AND coalesce(evidence->>'host', '') <> ''
 ORDER BY host`

	// SQLScopeOverrides is the tenant's narrower per-tool modes, as tool_fingerprint -> mode.
	SQLScopeOverrides = `
SELECT key, value
  FROM ops.tenant t, jsonb_each_text(t.scope_overrides)
 WHERE t.tenant_id = $1::uuid`

	// SQLLockTenantPolicy serialises minting per tenant for the rest of the transaction.
	SQLLockTenantPolicy = `SELECT pg_advisory_xact_lock(hashtextextended('ops.policy_bundle:' || $1, 0))`

	SQLLatestPolicyBundle = `
SELECT bundle_version, scope_matrix, destination_allowlist, feature_state, spool_bounds,
       retention_class, signature_kid, signed_digest, signed_envelope,
       effective_from, created_by, created_at
  FROM ops.policy_bundle
 WHERE tenant_id = $1::uuid
 ORDER BY bundle_version DESC
 LIMIT 1`

	// SQLInsertPolicyBundle writes a signed bundle. The table's ceiling trigger refuses a scope
	// matrix above the tenant ceiling.
	SQLInsertPolicyBundle = `
INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix, destination_allowlist,
                               retention_class, spool_bounds, feature_state,
                               signature_kid, signed_digest, effective_from, created_by, signed_envelope)
VALUES ($1::uuid, $2::bigint, $3::jsonb, $4::jsonb, $5, $6::jsonb, $7::jsonb, $8, $9,
        $10::timestamptz, $11, $12::bytea)`
)

const keyColumns = `key_id::text, tenant_id::text, key_hash, label, created_by, created_at, expires_at,
       revoked_at, enrolment_count, last_used_at`

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

// DeploymentKeyByHash implements Store.
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

// DeviceVerification implements Store.
func (s *SQLStore) DeviceVerification(ctx context.Context, tenantID string) (DeviceVerification, error) {
	var v DeviceVerification
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		v, err = deviceVerification(ctx, tx, tenantID)
		return err
	})
	return v, err
}

func deviceVerification(ctx context.Context, tx *sql.Tx, tenantID string) (DeviceVerification, error) {
	var v DeviceVerification
	err := tx.QueryRowContext(ctx, SQLDeviceVerification, tenantID).Scan(&v.Mode, &v.EntraTenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrUnknownTenant
	}
	if err != nil {
		return v, fmt.Errorf("store: device verification: %w", err)
	}
	return v, nil
}

// SetDeviceIntuneID implements Store.
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

// RecordDeploymentEnrolment implements Store.
func (s *SQLStore) RecordDeploymentEnrolment(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, SQLTouchDeploymentKey, tenantID, keyID, at.UTC()); err != nil {
			return fmt.Errorf("store: count deployment enrolment: %w", err)
		}
		return insertAudit(ctx, tx, audit)
	})
}

// Audit implements Store.
func (s *SQLStore) Audit(ctx context.Context, audit AuditEntry) error {
	return s.withTenant(ctx, audit.TenantID, func(tx *sql.Tx) error {
		return insertAudit(ctx, tx, audit)
	})
}

// CreateDeploymentKey implements Store.
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

// RevokeDeploymentKey implements Store.
func (s *SQLStore) RevokeDeploymentKey(ctx context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) (DeploymentKey, error) {
	var out DeploymentKey
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		out, err = scanDeploymentKey(tx.QueryRowContext(ctx, SQLRevokeDeploymentKey, tenantID, keyID, at.UTC()))
		if errors.Is(err, sql.ErrNoRows) {
			// Already revoked (the first revocation stands) or unknown.
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

// SetDeviceVerification implements Store.
func (s *SQLStore) SetDeviceVerification(ctx context.Context, tenantID, mode string, audit AuditEntry) error {
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		current, err := deviceVerification(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if mode == VerificationIntune && current.EntraTenantID == "" {
			return ErrNoEntraConnection
		}
		if _, err := tx.ExecContext(ctx, SQLSetDeviceVerification, tenantID, mode); err != nil {
			return fmt.Errorf("store: set device verification: %w", err)
		}
		if audit.Detail == nil {
			audit.Detail = map[string]any{}
		}
		audit.Detail["previous"] = current.Mode
		return insertAudit(ctx, tx, audit)
	})
}

// DeploymentSummary implements Store.
func (s *SQLStore) DeploymentSummary(ctx context.Context, tenantID string) (DeploymentSummary, error) {
	var out DeploymentSummary
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		v, err := deviceVerification(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		out.DeviceVerification = v.Mode
		var c IdentityConnection
		err = tx.QueryRowContext(ctx, SQLIdentityConnectionSummary, tenantID).
			Scan(&c.Provider, &c.Status, &c.EntraTenantID, &c.Issuer)
		switch {
		case err == nil:
			out.Connection = &c
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: identity connection: %w", err)
		}
		if out.Keys, err = listDeploymentKeys(ctx, tx, tenantID); err != nil {
			return err
		}
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

func listDeploymentKeys(ctx context.Context, tx *sql.Tx, tenantID string) ([]DeploymentKey, error) {
	rows, err := tx.QueryContext(ctx, SQLListDeploymentKeys, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list deployment keys: %w", err)
	}
	defer rows.Close()
	var keys []DeploymentKey
	for rows.Next() {
		k, err := scanDeploymentKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list deployment keys: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// PolicyInputs implements Store.
func (s *SQLStore) PolicyInputs(ctx context.Context, tenantID string) (PolicyInputs, error) {
	var in PolicyInputs
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		t := PolicyTenant{TenantID: tenantID}
		err := tx.QueryRowContext(ctx, SQLPolicyTenant, tenantID).Scan(&t.Status, &t.IngestEnabled, &t.CeilingMode, &t.CollectionMode, &t.TLSInspection)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("store: policy tenant: %w", err)
		}
		in.Tenant = t
		var err2 error
		in.InterceptionHosts, err2 = interceptionHosts(ctx, tx)
		if err2 != nil {
			return err2
		}
		if in.ScopeOverrides, err2 = scopeOverrides(ctx, tx, tenantID); err2 != nil {
			return err2
		}
		if in.Endpoint, err2 = endpointSettings(ctx, tx, tenantID); err2 != nil {
			return err2
		}
		if in.Rules, err2 = enforcementRules(ctx, tx, tenantID); err2 != nil {
			return err2
		}
		in.SanctionedTools, err2 = sanctionedTools(ctx, tx, tenantID)
		return err2
	})
	return in, err
}

func scopeOverrides(ctx context.Context, tx *sql.Tx, tenantID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, SQLScopeOverrides, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: scope overrides: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scope overrides: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

func interceptionHosts(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, SQLInterceptionHosts)
	if err != nil {
		return nil, fmt.Errorf("store: interception hosts: %w", err)
	}
	defer rows.Close()
	var hosts []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: interception hosts: %w", err)
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func scanPolicyBundle(row rowScanner, tenantID string) (PolicyBundle, error) {
	b := PolicyBundle{TenantID: tenantID}
	var scope, allow, feature, spool []byte
	if err := row.Scan(&b.Version, &scope, &allow, &feature, &spool, &b.RetentionClass,
		&b.SignatureKID, &b.SignedDigest, &b.SignedEnvelope, &b.EffectiveFrom, &b.CreatedBy, &b.CreatedAt); err != nil {
		return PolicyBundle{}, err
	}
	b.ScopeMatrix, b.DestinationAllowlist = json.RawMessage(scope), json.RawMessage(allow)
	b.FeatureState, b.SpoolBounds = json.RawMessage(feature), json.RawMessage(spool)
	return b, nil
}

// LatestPolicyBundle implements Store.
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

// MintPolicyBundle implements Store.
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
			nb.RetentionClass, jsonText(nb.SpoolBounds, "{}"), jsonText(nb.FeatureState, "{}"),
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

func insertAudit(ctx context.Context, tx *sql.Tx, a AuditEntry) error {
	detail := []byte("{}")
	if len(a.Detail) > 0 {
		var err error
		if detail, err = json.Marshal(a.Detail); err != nil {
			return fmt.Errorf("store: audit detail: %w", err)
		}
	}
	at := a.OccurredAt
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := tx.ExecContext(ctx, SQLInsertAudit, a.TenantID, a.ActorType, a.ActorID, a.Action,
		a.ObjectType, a.ObjectID, string(detail), at.UTC()); err != nil {
		return fmt.Errorf("store: insert audit: %w", err)
	}
	return nil
}
