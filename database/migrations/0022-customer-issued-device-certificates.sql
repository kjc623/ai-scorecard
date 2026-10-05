-- 0022-customer-issued-device-certificates.sql
--
-- The upgrade path for ADR 0022 (docs/adr/0022-customer-issued-device-certificates-are-registered-not-signed.md).
-- `database/schema.sql` is the authoritative, from-empty definition; this file brings an EXISTING
-- database to the same shape. It is additive and safe to run once. It changes no existing row's
-- meaning: every credential already stored becomes credential_origin='product', which is what it is.
--
-- Apply as the schema owner (the same role that applied schema.sql), with ON_ERROR_STOP:
--   psql "$DSN" -v ON_ERROR_STOP=1 -f database/migrations/0022-customer-issued-device-certificates.sql
--
-- Prerequisite: no two LIVE (revoked_at IS NULL) credentials share a public_key_thumbprint. The
-- unique index below fails otherwise, which is the point -- a duplicate would make the pre-tenant
-- resolver ambiguous. Check first with:
--   SELECT public_key_thumbprint, count(*) FROM ops.device_credential
--    WHERE revoked_at IS NULL GROUP BY 1 HAVING count(*) > 1;

-- The tenant's device trust anchor (public PEM bundle; NULL = product-issued certificates).
ALTER TABLE ops.tenant ADD COLUMN device_ca_pem text;

COMMENT ON COLUMN ops.tenant.device_ca_pem IS
  'The PEM bundle a tenant''s DEVICE certificates must chain to (ADR 0022). NULL means product-issued certificates (ADR 0020 decision 3); non-NULL means the customer issues device certificates and control-api only registers and verifies them. Public trust material, per tenant.';

-- Who issued the credential.
ALTER TABLE ops.device_credential ADD COLUMN credential_origin text NOT NULL DEFAULT 'product'
  CHECK (credential_origin IN ('product','customer'));

COMMENT ON COLUMN ops.device_credential.credential_origin IS
  'Who issued the credential (ADR 0022): product = control-api signed a leaf from the device CSR; customer = the customer''s PKI/MDM issued the certificate and control-api only registered it. Recorded, never inferred, so the issuance path is explicit.';

-- One live credential per public key, across all tenants.
CREATE UNIQUE INDEX device_credential_live_thumbprint
  ON ops.device_credential (public_key_thumbprint)
  WHERE revoked_at IS NULL;

COMMENT ON INDEX ops.device_credential_live_thumbprint IS
  'One live credential per public key, tenant-wide. The pre-tenant credential resolver depends on this uniqueness; a second live row for one key would make authentication ambiguous.';

-- The pre-tenant device-credential resolver (section 5c of schema.sql).
CREATE FUNCTION ops.device_credential_for_thumbprint(p_thumbprint text)
RETURNS TABLE (
  tenant_id       uuid,
  device_id       uuid,
  credential_id   uuid,
  credential_type text,
  expires_at      timestamptz,
  device_ca_pem   text
)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.tenant_id, c.device_id, c.credential_id, c.credential_type,
         c.expires_at, t.device_ca_pem
    FROM ops.device_credential c
    JOIN ops.tenant t ON t.tenant_id = c.tenant_id
   WHERE c.public_key_thumbprint = p_thumbprint
     AND c.revoked_at IS NULL
$$;

COMMENT ON FUNCTION ops.device_credential_for_thumbprint(text) IS
  'Pre-tenant (ADR 0022): the LIVE device credential for a certificate''s SPKI thumbprint, with the tenant''s device trust anchor. Returns nothing for a revoked or unregistered credential. Expiry is returned for the caller to enforce.';

-- RLS reach: a SELECT-only policy on each table the resolver reads.
CREATE POLICY pre_tenant_lookup ON ops.device_credential FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.tenant              FOR SELECT TO sac_resolver USING (true);

-- Privileges: sac_resolver owns the body and reads only these two tables; the function is revoked
-- from PUBLIC and granted to the two roles that call it.
GRANT SELECT ON ops.device_credential, ops.tenant TO sac_resolver;
ALTER FUNCTION ops.device_credential_for_thumbprint(text) OWNER TO sac_resolver;
REVOKE EXECUTE ON FUNCTION ops.device_credential_for_thumbprint(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ops.device_credential_for_thumbprint(text) TO sac_control, sac_ingest;
