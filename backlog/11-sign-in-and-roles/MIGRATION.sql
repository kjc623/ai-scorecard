-- In-place migration: sign-in, roles, SCIM provisioning and device deployment
-- (backlog/11-sign-in-and-roles, the enterprise-onboarding contract §1). Brings a database built from
-- the previous database/schema.sql -- plus migrations 03 to 09, which the lab's already has -- to the
-- current one. Everything here is additive: new columns are nullable or defaulted, new tables are
-- created if absent, functions are replaced whole, and every policy and constraint is added only if
-- it is not there. No row of either tenant is written. Re-running is safe; it was run twice in
-- verification and the second run changed nothing.
--
-- Run as a role that can create roles and alter objects (the lab's `postgres`), in the `shadow`
-- database, after 09's migration:
--
--   psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/11-sign-in-and-roles/MIGRATION.sql
--
-- or, against the device-auth lab:
--
--   docker exec -i sac-authlab-postgres-1 psql -U postgres -d shadow -v ON_ERROR_STOP=1 \
--     < backlog/11-sign-in-and-roles/MIGRATION.sql
--
-- The definitions below are copied from database/schema.sql, not restated, so the two cannot
-- describe different tables; the comments that say why each exists are the schema's own.
BEGIN;

-- 1. sac_resolver: the owner of the pre-tenant lookup functions and nothing else. A role is
--    cluster-wide, so it is created only if a previous run (or another database) has not made it.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sac_resolver') THEN
    CREATE ROLE sac_resolver NOLOGIN;
  END IF;
END $$;

-- 2. The tenant's user-reference key (minted lazily by control-api) and its device-verification
--    setting. Existing tenants read 'none': nothing changes for them until an admin opts in.
ALTER TABLE ops.tenant
  ADD COLUMN IF NOT EXISTS user_ref_key_enc bytea;
ALTER TABLE ops.tenant
  ADD COLUMN IF NOT EXISTS device_verification text NOT NULL DEFAULT 'none'
    CONSTRAINT tenant_device_verification_known
    CHECK (device_verification IN ('none','intune'));

-- 3. The Intune device id Graph confirmed at enrolment, unique per tenant where present. No existing
--    device has one, so the index cannot fail on existing rows.
ALTER TABLE ops.device
  ADD COLUMN IF NOT EXISTS intune_device_id text;
CREATE UNIQUE INDEX IF NOT EXISTS device_intune_device_uniq
  ON ops.device (tenant_id, intune_device_id)
  WHERE intune_device_id IS NOT NULL;

-- 4. The signed envelope GET /v1/policy serves, and the digest constraints that expire the
--    signed_digest exemption now that control-api writes this table. Nothing wrote it before this
--    change, so there is no existing row for the constraints to refuse; if a database has one in
--    another spelling, the ADD fails and names it, which is the moment to look at that row.
ALTER TABLE ops.policy_bundle
  ADD COLUMN IF NOT EXISTS signed_envelope bytea;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'ops.policy_bundle'::regclass
                   AND conname = 'policy_bundle_digest_is_sha256') THEN
    ALTER TABLE ops.policy_bundle ADD CONSTRAINT policy_bundle_digest_is_sha256
      CHECK (signed_digest ~ '^sha256:[0-9a-f]{64}$');
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'ops.policy_bundle'::regclass
                   AND conname = 'policy_bundle_digest_names_envelope') THEN
    ALTER TABLE ops.policy_bundle ADD CONSTRAINT policy_bundle_digest_names_envelope
      CHECK (signed_envelope IS NULL
             OR signed_digest = 'sha256:' || encode(sha256(signed_envelope), 'hex'));
  END IF;
END $$;

-- 5. The identity, provisioning and deployment tables (schema.sql section 5c).
-- One identity provider connection per row. A tenant normally has one; the unique lookup keys are
-- what make a second tenant claiming the same Entra tenant or issuer unrepresentable.
CREATE TABLE IF NOT EXISTS ops.identity_connection (
  connection_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  provider          text NOT NULL CHECK (provider IN ('entra','oidc')),
  -- The customer's Entra directory id, lower-case, exactly as a token's `tid` spells it. The issuer
  -- of an Entra token is derived from it (https://login.microsoftonline.com/{tid}/v2.0), so it is
  -- the mapping key and there is no issuer to store.
  entra_tenant_id   text UNIQUE
                      CONSTRAINT identity_connection_entra_tenant_is_uuid
                      CHECK (entra_tenant_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
  -- The exact `iss` an OIDC provider signs. Compared byte for byte: normalising a trailing slash or
  -- a case would let two spellings map to two tenants, or one spelling be accepted for another.
  issuer            text UNIQUE,
  -- An OIDC client registered in the customer's provider. Entra uses the vendor's one multi-tenant
  -- application, so an Entra row carries neither.
  client_id         text,
  client_secret_enc bytea,
  scopes            text NOT NULL DEFAULT 'openid profile email',
  roles_claim       text NOT NULL DEFAULT 'roles',
  -- {"<provider value>": "<product role>"}. Empty means the provider already emits product role
  -- names. A user whose values map to no role is refused, never defaulted (contract §3).
  role_map          jsonb NOT NULL DEFAULT '{}'::jsonb,
  status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','disabled')),
  created_at        timestamptz NOT NULL DEFAULT now(),
  activated_at      timestamptz,
  activated_by      text,
  -- Composite key for the session and grant foreign keys, so a row in one tenant cannot point at
  -- another tenant's connection.
  UNIQUE (tenant_id, connection_id),
  CONSTRAINT identity_connection_entra_has_tid
    CHECK ((provider = 'entra') = (entra_tenant_id IS NOT NULL)),
  CONSTRAINT identity_connection_oidc_has_issuer_and_client
    CHECK ((provider = 'oidc') = (issuer IS NOT NULL AND client_id IS NOT NULL)),
  CONSTRAINT identity_connection_credentials_only_for_oidc
    CHECK (provider = 'oidc' OR (client_id IS NULL AND client_secret_enc IS NULL)),
  -- Activation is what lets a provider sign people into a tenant, so it is attributed like a gate
  -- closure is: an active connection always names who activated it and when.
  CONSTRAINT identity_connection_activation_attributed
    CHECK (status <> 'active' OR (activated_at IS NOT NULL AND activated_by IS NOT NULL)),
  -- A mapping onto a role the product does not have would be a grant that silently grants nothing.
  CONSTRAINT identity_connection_role_map_names_product_roles
    CHECK (jsonb_typeof(role_map) = 'object'
           AND NOT jsonb_path_exists(role_map,
             '$.* ? (!(@ == "viewer" || @ == "analyst" || @ == "content_reader" || @ == "admin"))'))
);

COMMENT ON TABLE ops.identity_connection IS
  'A customer identity provider linked to a tenant: Entra (keyed by the customer''s directory id) or any OIDC provider (keyed by its exact issuer). Both keys are unique across the database, so the provider cannot choose the tenant; the mapping is the product''s. pending until the customer''s first admin completes one sign-in through it.';

COMMENT ON COLUMN ops.identity_connection.client_secret_enc IS
  'The OIDC client secret, sealed under the tenant''s directory-derived key. NULL for Entra, which authenticates as the vendor''s application.';

-- Email domains for sign-in discovery: "you@contoso.com" -> the tenant whose connection signs you
-- in. Set by the vendor when it creates the onboarding invite, never claimed by the customer: a
-- domain a customer could claim is a sign-in page another customer's staff could be steered to.
CREATE TABLE IF NOT EXISTS ops.tenant_email_domain (
  domain      text PRIMARY KEY
                CONSTRAINT tenant_email_domain_is_lower_hostname
                CHECK (domain = lower(domain)
                       AND domain ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$'),
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  text
);

CREATE INDEX IF NOT EXISTS tenant_email_domain_by_tenant ON ops.tenant_email_domain (tenant_id);

-- The one-time onboarding link the vendor issues. The token carries the tenant id in clear (as an
-- enrolment token does) and only its hash is stored. Single use: used_at and used_by are set
-- together when the first admin's sign-in activates the connection.
CREATE TABLE IF NOT EXISTS ops.onboarding_invite (
  invite_id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  token_hash  text NOT NULL UNIQUE
                CONSTRAINT onboarding_invite_hash_is_sha256
                CHECK (token_hash ~ '^sha256:[0-9a-f]{64}$'),
  created_by  text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  used_by     text,
  CONSTRAINT onboarding_invite_expiry_after_issue CHECK (expires_at > created_at),
  CONSTRAINT onboarding_invite_use_is_whole CHECK ((used_at IS NULL) = (used_by IS NULL))
);

CREATE INDEX IF NOT EXISTS onboarding_invite_by_tenant ON ops.onboarding_invite (tenant_id, created_at);

-- Roles granted to one person by the product rather than by the provider's claim: the first admin
-- at onboarding, and any grant an admin makes. The effective roles are the role_map of the claim
-- united with these rows. Keyed by the provider's subject (Entra: `oid`), which, unlike an email
-- address, a provider never reassigns to someone else.
CREATE TABLE IF NOT EXISTS ops.role_grant (
  tenant_id      uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  connection_id  uuid NOT NULL,
  subject        text NOT NULL,
  role           text NOT NULL CHECK (role IN ('viewer','analyst','content_reader','admin')),
  granted_by     text NOT NULL,
  granted_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, connection_id, subject, role),
  FOREIGN KEY (tenant_id, connection_id) REFERENCES ops.identity_connection(tenant_id, connection_id)
);

-- The server-side session behind the dashboard's cookie. The cookie value is never stored, only its
-- sha256, so a database read cannot be replayed as a session. roles is resolved at sign-in and is
-- never empty: a person with no role is refused, not given a session with nothing in it.
CREATE TABLE IF NOT EXISTS ops.auth_session (
  session_hash          bytea PRIMARY KEY
                          CONSTRAINT auth_session_hash_is_sha256
                          CHECK (octet_length(session_hash) = 32),
  tenant_id             uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  connection_id         uuid NOT NULL,
  subject               text NOT NULL,
  actor                 text NOT NULL,
  roles                 text[] NOT NULL
                          CONSTRAINT auth_session_roles_are_product_roles
                          CHECK (cardinality(roles) >= 1
                                 AND roles <@ ARRAY['viewer','analyst','content_reader','admin']::text[]),
  -- The person's canonical directory ref when SCIM knows them, so deactivating them in the provider
  -- ends this session at the next token refresh (contract §3).
  user_ref              text,
  idp_refresh_token_enc bytea,
  -- When the provider refresh token was last exercised. The refresh runs at most every 30 minutes,
  -- and a failed one ends the session; without a timestamp "at most" would be a guess.
  idp_refreshed_at      timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  last_seen_at          timestamptz NOT NULL DEFAULT now(),
  expires_at            timestamptz NOT NULL,
  revoked_at            timestamptz,
  FOREIGN KEY (tenant_id, connection_id) REFERENCES ops.identity_connection(tenant_id, connection_id),
  CONSTRAINT auth_session_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS auth_session_by_user_ref ON ops.auth_session (tenant_id, user_ref) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS auth_session_expiry ON ops.auth_session (tenant_id, expires_at);

-- A sign-in attempt between /auth/begin and /auth/complete: the PKCE verifier, state and nonce
-- control-api generated, for ten minutes. It is the one table here with no tenant, because an
-- attempt that starts with "Sign in with Microsoft" does not know its tenant until the provider
-- answers. Row-level security still applies: only sac_control has a policy, so no other role can
-- see an attempt even if a grant were added by mistake.
CREATE TABLE IF NOT EXISTS ops.auth_signin (
  attempt_hash       bytea PRIMARY KEY
                       CONSTRAINT auth_signin_hash_is_sha256
                       CHECK (octet_length(attempt_hash) = 32),
  connection_id      uuid REFERENCES ops.identity_connection(connection_id),
  state              text,
  nonce              text,
  code_verifier_enc  bytea,
  redirect_uri       text,
  invite_id          uuid REFERENCES ops.onboarding_invite(invite_id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  expires_at         timestamptz NOT NULL,
  CONSTRAINT auth_signin_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS auth_signin_expiry ON ops.auth_signin (expires_at);

-- The bearer an identity provider's SCIM client presents. Same spelling as an enrolment token
-- (sacscim_ prefix, tenant id in clear, 256-bit tail), stored as its hash; revoked, never deleted,
-- so the audit trail's token ids keep resolving.
CREATE TABLE IF NOT EXISTS ops.scim_token (
  token_id    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  token_hash  text NOT NULL UNIQUE
                CONSTRAINT scim_token_hash_is_sha256
                CHECK (token_hash ~ '^sha256:[0-9a-f]{64}$'),
  label       text,
  created_by  text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at  timestamptz
);

CREATE INDEX IF NOT EXISTS scim_token_by_tenant ON ops.scim_token (tenant_id, created_at);

-- A person as the identity provider last provisioned them. The resource itself is sealed: it holds
-- names and addresses, and GET must return what was sent. Filters (`userName eq "…"`) are answered
-- through keyed hashes, so a lookup never needs the plaintext and the table holds no clear identifier.
CREATE TABLE IF NOT EXISTS ops.scim_user (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  scim_id           uuid NOT NULL DEFAULT gen_random_uuid(),
  -- HMAC-SHA256(the tenant's user-reference key, lower(userName)); likewise for externalId.
  user_name_hash    bytea NOT NULL CONSTRAINT scim_user_name_hash_is_hmac CHECK (octet_length(user_name_hash) = 32),
  external_id_hash  bytea CONSTRAINT scim_user_external_id_hash_is_hmac CHECK (octet_length(external_id_hash) = 32),
  resource_enc      bytea NOT NULL,
  -- The canonical ref, DeriveUserRef(upn, userName) at creation and never changed afterwards: a
  -- rename becomes an alias, so a person's history does not split at the rename.
  user_ref          text NOT NULL CONSTRAINT scim_user_ref_is_derived CHECK (user_ref ~ '^u_[0-9a-f]{32}$'),
  active            boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, scim_id),
  UNIQUE (tenant_id, user_name_hash),
  -- Two people sharing one canonical ref would merge their histories silently. A recycled userName
  -- derives the ref of its previous owner, so this is reachable, and it must fail loudly.
  UNIQUE (tenant_id, user_ref)
);

CREATE INDEX IF NOT EXISTS scim_user_by_external_id ON ops.scim_user (tenant_id, external_id_hash)
  WHERE external_id_hash IS NOT NULL;

-- Groups as provisioned. A group name is organisational, not personal, so it is stored in clear.
CREATE TABLE IF NOT EXISTS ops.scim_group (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  scim_id       uuid NOT NULL DEFAULT gen_random_uuid(),
  display_name  text NOT NULL,
  external_id   text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, scim_id)
);

-- Membership dies with either side: a SCIM DELETE of a user or a group removes it in the same
-- statement, so there is no second deletion path to forget.
CREATE TABLE IF NOT EXISTS ops.scim_group_member (
  tenant_id  uuid NOT NULL,
  group_id   uuid NOT NULL,
  user_id    uuid NOT NULL,
  PRIMARY KEY (tenant_id, group_id, user_id),
  FOREIGN KEY (tenant_id, group_id) REFERENCES ops.scim_group(tenant_id, scim_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, user_id) REFERENCES ops.scim_user(tenant_id, scim_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS scim_group_member_by_user ON ops.scim_group_member (tenant_id, user_id);

-- Device-derived ref -> canonical ref. A device derives a person's ref from whatever it can resolve
-- (a UPN, an Entra object id, an account name), and a UPN changes on a rename, so one person reaches
-- the server under several refs. ingest.record_event resolves through this table at write time, so
-- every stored event already carries the canonical ref and no read has to join it. Rows are kept
-- after the person is deprovisioned: an alias is what stops a later event from starting a second
-- history for the same person.
CREATE TABLE IF NOT EXISTS ops.user_ref_alias (
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  alias_ref   text NOT NULL CONSTRAINT user_ref_alias_is_derived CHECK (alias_ref ~ '^u_[0-9a-f]{32}$'),
  user_ref    text NOT NULL CONSTRAINT user_ref_alias_target_is_derived CHECK (user_ref ~ '^u_[0-9a-f]{32}$'),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, alias_ref)
);

-- Subject export and erasure start from the canonical ref and need every alias of it.
CREATE INDEX IF NOT EXISTS user_ref_alias_by_canonical ON ops.user_ref_alias (tenant_id, user_ref);

-- A per-tenant key a device package carries (sacdk_ prefix, tenant id in clear), replacing the
-- single-use enrolment token for fleet deployment: one key enrols many devices, so it is counted,
-- revocable and optionally time-bounded rather than consumed. A new key is minted per package
-- download, so revoking one leaked package leaves the others working.
CREATE TABLE IF NOT EXISTS ops.deployment_key (
  key_id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  key_hash         text NOT NULL UNIQUE
                     CONSTRAINT deployment_key_hash_is_sha256
                     CHECK (key_hash ~ '^sha256:[0-9a-f]{64}$'),
  label            text NOT NULL,
  created_by       text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  expires_at       timestamptz,
  revoked_at       timestamptz,
  enrolment_count  bigint NOT NULL DEFAULT 0 CHECK (enrolment_count >= 0),
  last_used_at     timestamptz,
  CONSTRAINT deployment_key_expiry_after_creation CHECK (expires_at IS NULL OR expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS deployment_key_by_tenant ON ops.deployment_key (tenant_id, created_at);

COMMENT ON TABLE ops.deployment_key IS
  'A per-tenant deployment key, stored only as its sha256. Many devices enrol with one key, so it is counted (enrolment_count, last_used_at) and revoked rather than consumed; for a tenant whose device_verification is intune, the key alone is not enough and Graph must confirm the device is managed.';

-- 6. The pre-tenant lookups. Replaced whole; a replaced function keeps its owner and grants, and a
--    new one is handed to sac_resolver in step 8.
-- The pre-tenant lookups. Each answers one exact-key question before control-api can set
-- app.tenant_id, and returns only what may be used: an active connection, an unused invite, a live
-- session, an unrevoked token. Filtering here rather than in the caller means a caller bug cannot
-- resurrect a revoked session or sign in through a disabled connection.
--
-- How they cross tenants, stated because it is the one place in this file that does: they are
-- SECURITY DEFINER and owned by sac_resolver (section 10), a NOLOGIN role with no members whose only
-- privileges are SELECT on these five tables and a SELECT-only policy on each (section 9). Not the
-- table owner, which is subject to the forced policies like everyone else, and not a BYPASSRLS
-- role, which would let a definer body read any table. search_path is pinned and every name is
-- schema-qualified, so a caller cannot substitute an object. EXECUTE is revoked from PUBLIC and
-- granted to sac_control alone.
CREATE OR REPLACE FUNCTION ops.identity_connection_for_entra(p_entra_tenant text)
RETURNS SETOF ops.identity_connection
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.*
    FROM ops.identity_connection c
   WHERE c.provider = 'entra'
     AND c.entra_tenant_id = lower(p_entra_tenant)
     AND c.status = 'active'
$$;

COMMENT ON FUNCTION ops.identity_connection_for_entra(text) IS
  'Pre-tenant: the ACTIVE Entra connection for a token''s tid, or nothing. A pending or disabled connection signs nobody in.';

CREATE OR REPLACE FUNCTION ops.identity_connection_for_issuer(p_issuer text)
RETURNS SETOF ops.identity_connection
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.*
    FROM ops.identity_connection c
   WHERE c.provider = 'oidc'
     AND c.issuer = p_issuer
     AND c.status = 'active'
$$;

COMMENT ON FUNCTION ops.identity_connection_for_issuer(text) IS
  'Pre-tenant: the ACTIVE OIDC connection whose issuer is exactly p_issuer, or nothing. No normalisation: the issuer is compared as the provider signs it.';

-- Any status, deliberately: onboarding signs the first admin in through a connection that is still
-- pending, and the attempt row names the connection it began with.
CREATE OR REPLACE FUNCTION ops.identity_connection_by_id(p_connection uuid)
RETURNS SETOF ops.identity_connection
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.*
    FROM ops.identity_connection c
   WHERE c.connection_id = p_connection
$$;

COMMENT ON FUNCTION ops.identity_connection_by_id(uuid) IS
  'Pre-tenant: one connection by id in any status, for completing a sign-in that began against it (onboarding completes through a pending one). The caller decides what each status permits.';

CREATE OR REPLACE FUNCTION ops.tenant_for_email_domain(p_domain text)
RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT d.tenant_id
    FROM ops.tenant_email_domain d
   WHERE d.domain = lower(p_domain)
$$;

COMMENT ON FUNCTION ops.tenant_for_email_domain(text) IS
  'Pre-tenant: the tenant a sign-in email domain belongs to, or NULL. Only the tenant id: which connection signs the person in is then read under that tenant''s row-level security.';

CREATE OR REPLACE FUNCTION ops.onboarding_invite_by_hash(p_hash text)
RETURNS SETOF ops.onboarding_invite
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT i.*
    FROM ops.onboarding_invite i
   WHERE i.token_hash = p_hash
     AND i.used_at IS NULL
     AND i.expires_at > now()
$$;

COMMENT ON FUNCTION ops.onboarding_invite_by_hash(text) IS
  'Pre-tenant: an onboarding invite that is still unused and unexpired, or nothing. A used invite does not resolve, so a forwarded link cannot onboard a second admin.';

CREATE OR REPLACE FUNCTION ops.auth_session_by_hash(p_hash bytea)
RETURNS SETOF ops.auth_session
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT s.*
    FROM ops.auth_session s
   WHERE s.session_hash = p_hash
     AND s.revoked_at IS NULL
     AND s.expires_at > now()
$$;

COMMENT ON FUNCTION ops.auth_session_by_hash(bytea) IS
  'Pre-tenant: the live session behind a cookie (by the sha256 of its value), or nothing once it is revoked or past expires_at. The idle limit and SCIM deactivation are checked by the caller, which then holds the tenant.';

CREATE OR REPLACE FUNCTION ops.tenant_for_scim_token(p_hash text)
RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT t.tenant_id
    FROM ops.scim_token t
   WHERE t.token_hash = p_hash
     AND t.revoked_at IS NULL
$$;

COMMENT ON FUNCTION ops.tenant_for_scim_token(text) IS
  'Pre-tenant: the tenant an UNREVOKED SCIM bearer belongs to, or NULL. A revoked token resolves to nothing, so provisioning stops the moment an admin revokes it.';

-- 7. Row-level security: the eleven tenant-scoped tables join the same forced tenant_isolation
--    policy as every other ops table, the lookups' owner gets its SELECT-only policy on the five
--    tables it reads, and the tenant-less sign-in table is forced with a policy for sac_control
--    alone. ENABLE and FORCE are idempotent; a policy is created only if absent.
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['ops.identity_connection',
                           'ops.tenant_email_domain',
                           'ops.onboarding_invite',
                           'ops.role_grant',
                           'ops.auth_session',
                           'ops.scim_token',
                           'ops.scim_user',
                           'ops.scim_group',
                           'ops.scim_group_member',
                           'ops.user_ref_alias',
                           'ops.deployment_key'] LOOP
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', t);
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname || '.' || tablename = t
                     AND policyname = 'tenant_isolation') THEN
      EXECUTE format(
        'CREATE POLICY tenant_isolation ON %s
           USING (tenant_id = ops.current_tenant())
           WITH CHECK (tenant_id = ops.current_tenant())', t);
    END IF;
  END LOOP;

  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'ops'
                   AND tablename = 'identity_connection' AND policyname = 'pre_tenant_lookup') THEN
    CREATE POLICY pre_tenant_lookup ON ops.identity_connection FOR SELECT TO sac_resolver USING (true);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'ops'
                   AND tablename = 'tenant_email_domain' AND policyname = 'pre_tenant_lookup') THEN
    CREATE POLICY pre_tenant_lookup ON ops.tenant_email_domain FOR SELECT TO sac_resolver USING (true);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'ops'
                   AND tablename = 'onboarding_invite' AND policyname = 'pre_tenant_lookup') THEN
    CREATE POLICY pre_tenant_lookup ON ops.onboarding_invite   FOR SELECT TO sac_resolver USING (true);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'ops'
                   AND tablename = 'auth_session' AND policyname = 'pre_tenant_lookup') THEN
    CREATE POLICY pre_tenant_lookup ON ops.auth_session        FOR SELECT TO sac_resolver USING (true);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'ops'
                   AND tablename = 'scim_token' AND policyname = 'pre_tenant_lookup') THEN
    CREATE POLICY pre_tenant_lookup ON ops.scim_token          FOR SELECT TO sac_resolver USING (true);
  END IF;

  ALTER TABLE ops.auth_signin ENABLE ROW LEVEL SECURITY;
  ALTER TABLE ops.auth_signin FORCE ROW LEVEL SECURITY;
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'ops'
                   AND tablename = 'auth_signin' AND policyname = 'control_plane_only') THEN
    CREATE POLICY control_plane_only ON ops.auth_signin TO sac_control
      USING (true)
      WITH CHECK (true);
  END IF;
END $$;

-- 8. Grants. GRANT and REVOKE are idempotent; ALTER ... OWNER to the current owner is a no-op.
-- record_event resolves a device-derived user_ref to the person's canonical one as it writes
-- (section 5c). It runs with the caller's rights, so the read is a grant, and RLS keeps it to the
-- batch's own tenant. Read-only: the aliases are control-api's, written from SCIM.
GRANT SELECT ON ops.user_ref_alias TO sac_ingest;

-- The identity service (section 5c). A credential the audit trail may name -- an invite, a SCIM
-- token, a deployment key, a connection -- is revoked or disabled by a timestamp or a status, never
-- deleted, so there is no DELETE on those. DELETE is granted only where removing the row is the
-- operation: a role taken away, a domain the vendor withdraws, SCIM's DELETE of a user or a group
-- (memberships cascade), and the sweep of expired sessions and sign-in attempts. Aliases are never
-- deleted by control-api: one is what stops a later event from starting a second history for a
-- person who was deprovisioned.
GRANT SELECT, INSERT, UPDATE ON ops.identity_connection, ops.tenant_email_domain,
      ops.onboarding_invite, ops.role_grant, ops.auth_session, ops.auth_signin, ops.scim_token,
      ops.scim_user, ops.scim_group, ops.scim_group_member, ops.user_ref_alias, ops.deployment_key
  TO sac_control;
GRANT DELETE ON ops.tenant_email_domain, ops.role_grant, ops.auth_session, ops.auth_signin,
      ops.scim_user, ops.scim_group, ops.scim_group_member
  TO sac_control;

-- The pre-tenant lookups run as sac_resolver (see section 5c for why). It reads exactly the five
-- tables they answer from; its RLS reach is the SELECT-only pre_tenant_lookup policy on each.
GRANT USAGE ON SCHEMA ops TO sac_resolver;
GRANT SELECT ON ops.identity_connection, ops.tenant_email_domain, ops.onboarding_invite,
      ops.auth_session, ops.scim_token
  TO sac_resolver;
ALTER FUNCTION ops.identity_connection_for_entra(text)  OWNER TO sac_resolver;
ALTER FUNCTION ops.identity_connection_for_issuer(text) OWNER TO sac_resolver;
ALTER FUNCTION ops.identity_connection_by_id(uuid)      OWNER TO sac_resolver;
ALTER FUNCTION ops.tenant_for_email_domain(text)        OWNER TO sac_resolver;
ALTER FUNCTION ops.onboarding_invite_by_hash(text)      OWNER TO sac_resolver;
ALTER FUNCTION ops.auth_session_by_hash(bytea)          OWNER TO sac_resolver;
ALTER FUNCTION ops.tenant_for_scim_token(text)          OWNER TO sac_resolver;
-- A function is executable by PUBLIC unless revoked, and these cross tenants, so the default is
-- taken away before the one grant is made.
REVOKE EXECUTE ON FUNCTION ops.identity_connection_for_entra(text), ops.identity_connection_for_issuer(text),
      ops.identity_connection_by_id(uuid), ops.tenant_for_email_domain(text),
      ops.onboarding_invite_by_hash(text), ops.auth_session_by_hash(bytea),
      ops.tenant_for_scim_token(text)
  FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ops.identity_connection_for_entra(text), ops.identity_connection_for_issuer(text),
      ops.identity_connection_by_id(uuid), ops.tenant_for_email_domain(text),
      ops.onboarding_invite_by_hash(text), ops.auth_session_by_hash(bytea),
      ops.tenant_for_scim_token(text)
  TO sac_control;

-- 9. ingest.record_event, replaced whole: it now stores the canonical user_ref, resolved through
--    ops.user_ref_alias. CREATE OR REPLACE keeps sac_ingest's EXECUTE grant.
-- Idempotent event recording: the whole of brief C12 and R9 in one place, so that the
-- transport layer cannot implement the tie-break differently from the storage layer.
--
-- Input is the stored envelope as jsonb, which keeps this function in step with
-- contracts/event-envelope.schema.json rather than with a parameter list that drifts.
-- Returns 'inserted' | 'duplicate' | 'upgraded' | 'recorded', plus the submission id, so the
-- batch response can report per-event outcomes without a second query.
CREATE OR REPLACE FUNCTION ingest.record_event(p_envelope jsonb, p_received_at timestamptz)
RETURNS TABLE(event_outcome text, event_submission_id uuid)
LANGUAGE plpgsql AS $$
DECLARE
  v_tenant        uuid := (p_envelope->>'tenant_id')::uuid;
  v_event_id      uuid := (p_envelope->>'event_id')::uuid;
  v_kind          text := p_envelope->>'kind';
  v_mode          text := p_envelope->>'collection_mode';
  v_source        text := p_envelope->>'source';
  v_fidelity      int;
  v_occurred      timestamptz := (p_envelope->>'occurred_at')::timestamptz;
  v_sub_id        uuid;
  v_rowcount      int;
  v_ttl           int;
  v_is_exact      boolean;
  v_exact         text;
  v_weak          text;
  v_subject_name  text;
  v_user_ref      text;
BEGIN
  SELECT f.fidelity_rank INTO v_fidelity FROM ref.route_fidelity f WHERE f.source = v_source;
  IF v_fidelity IS NULL THEN
    RAISE EXCEPTION 'unknown route %', v_source;
  END IF;

  -- Retention is resolved once, here, and materialised onto both rows.
  v_ttl := ops.event_ttl_days(v_tenant, v_mode, p_envelope->'labels');

  -- Which dedup tier this observation belongs to is derived, never sent. A prompt carrying a
  -- content digest was read and therefore has an exact key. Anything else -- M0 by
  -- construction, a canvas UI, an established WebSocket, a rollup, a detection -- does not,
  -- and is filed under the server-derived weak key instead.
  v_is_exact := (v_kind = 'prompt' AND p_envelope->>'content_digest' IS NOT NULL);
  v_exact    := p_envelope->>'dedup_key';
  v_weak     := ingest.weak_dedup_key(v_tenant, (p_envelope->>'device_id')::uuid,
                                      p_envelope->>'tool_fingerprint', v_kind, v_occurred,
                                      (p_envelope->>'size_bytes')::bigint);

  -- The clear account name is stored only for a tenant whose identity setting is 'clear'. The
  -- server is authoritative: a stale device that still sends a name to a 'hashed' tenant has it
  -- dropped here, so the setting is a real storage control and not a display filter (ADR 0021).
  SELECT CASE WHEN t.device_identity = 'clear' THEN nullif(p_envelope->>'subject_name', '') END
    INTO v_subject_name
    FROM ops.tenant t WHERE t.tenant_id = v_tenant;

  -- The person, as the directory knows them. A device derives user_ref from what it can resolve (a
  -- UPN, an Entra object id, an account name), so one person can arrive under several refs; SCIM
  -- records each of them as an alias of the canonical ref (section 5c). Resolving here, once, means
  -- every stored row -- observation, submission, the device's last user -- carries the canonical
  -- ref, and no aggregate, count of people or subject export has to know aliases exist. A ref with
  -- no alias is stored as sent: a tenant without SCIM, or a person SCIM has not provisioned yet.
  -- The dedup keys never include the person, so resolution cannot change what merges with what.
  v_user_ref := coalesce(
    (SELECT a.user_ref FROM ops.user_ref_alias a
      WHERE a.tenant_id = v_tenant AND a.alias_ref = p_envelope->>'user_ref'),
    p_envelope->>'user_ref');

  -- Step 1: record the observation. ON CONFLICT DO NOTHING is what makes a retry free
  -- (brief C12: idempotency enforced by the store, not by a check in code). The primary key
  -- is (tenant_id, event_id), so a replayed batch inserts nothing and reports duplicates.
  INSERT INTO ingest.observation (
    tenant_id, event_id, device_id, user_ref, subject_name, tool_fingerprint, direction, kind,
    occurred_at, received_at, monotonic_offset_ms, source, confidence, collection_mode,
    size_bytes, content_digest, labels, classifier_version, content_excerpt, policy_decision,
    window_start, window_end, submission_count, bytes_total, detection_basis,
    prompt_kind, dedup_key, schema_version, expires_at
  )
  VALUES (
    v_tenant,
    v_event_id,
    (p_envelope->>'device_id')::uuid,
    v_user_ref,
    v_subject_name,
    p_envelope->>'tool_fingerprint',
    p_envelope->>'direction',
    v_kind,
    v_occurred,
    p_received_at,
    (p_envelope->>'monotonic_offset_ms')::bigint,
    v_source,
    p_envelope->>'confidence',
    v_mode,
    (p_envelope->>'size_bytes')::bigint,
    p_envelope->>'content_digest',
    p_envelope->'labels',
    p_envelope->>'classifier_version',
    p_envelope->'content_excerpt',
    p_envelope->'policy_decision',
    (p_envelope->>'window_start')::timestamptz,
    (p_envelope->>'window_end')::timestamptz,
    (p_envelope->>'submission_count')::bigint,
    (p_envelope->>'bytes_total')::bigint,
    p_envelope->>'detection_basis',
    nullif(p_envelope->>'prompt_kind', ''),
    p_envelope->>'dedup_key',
    p_envelope->>'schema_version',
    p_received_at + make_interval(days => v_ttl)
  )
  ON CONFLICT (tenant_id, event_id) DO NOTHING;

  -- ROW_COUNT after INSERT .. ON CONFLICT DO NOTHING is 0 when the row already existed, which
  -- is what makes a replayed batch free rather than merely tolerable (brief C12).
  GET DIAGNOSTICS v_rowcount = ROW_COUNT;

  IF v_rowcount = 0 THEN
    -- The observation already existed, so this batch has already been committed. Find which
    -- logical submission it belongs to and report the duplicate. Exact matches are preferred
    -- over weak ones when both exist.
    SELECT s.submission_id INTO v_sub_id
      FROM ingest.submission s
     WHERE s.tenant_id = v_tenant
       AND ((v_is_exact AND s.dedup_key = v_exact) OR s.dedup_weak_key = v_weak)
     ORDER BY (s.dedup_key IS NULL)
     LIMIT 1;
    RETURN QUERY SELECT 'duplicate'::text, v_sub_id;
    RETURN;
  END IF;

  -- The most recent user, for the Devices read. Guarded like last_seen_at so an out-of-order batch
  -- (a spool flush, a retry) cannot move the device's user backwards; the clear name is gated by the
  -- tenant setting as v_subject_name above already is (ADR 0021).
  UPDATE ops.device
     SET last_user_ref     = v_user_ref,
         last_subject_name = v_subject_name
   WHERE tenant_id = v_tenant
     AND device_id = (p_envelope->>'device_id')::uuid
     AND (last_seen_at IS NULL OR last_seen_at <= p_received_at);

  -- Step 2: fold the observation into the logical submission. Three cases, in order:
  --
  --   1. an exact match on this observation's own key -- merge into it;
  --   2. no exact match, but an existing row holding only a weak key for the same device,
  --      tool, 300-second bucket and size -- adopt it, because this observation supplies the
  --      key that row was missing;
  --   3. otherwise, a new logical submission.
  --
  -- Case 2 is what stops a submission first seen by a route that could not read content from
  -- being counted a second time when a route that could read it reports the same thing. The
  -- lookup matches only rows whose dedup_key IS NULL, which is why the weak key can never
  -- pull a confident row into a merge it does not belong in.
  IF v_is_exact THEN
    SELECT s.submission_id INTO v_sub_id
      FROM ingest.submission s
     WHERE s.tenant_id = v_tenant AND s.dedup_key = v_exact;
  END IF;

  IF v_sub_id IS NULL THEN
    SELECT s.submission_id INTO v_sub_id
      FROM ingest.submission s
     WHERE s.tenant_id = v_tenant
       AND s.dedup_weak_key = v_weak
       AND s.dedup_key IS NULL;
  END IF;

  IF v_sub_id IS NOT NULL THEN
    -- The tie-break is fidelity: lower rank wins. A higher-fidelity route upgrades the
    -- content-bearing fields; a lower-fidelity one only adds itself to observed_routes.
    -- Fields are replaced wholesale and never partially merged, because a blend of two
    -- observations of one submission would correspond to neither of them.
    --
    -- The exact key is the one exception, and it is not optional. When this observation is
    -- exact and the row it matched is still weak-only, the row is being ADOPTED: it takes this
    -- observation's exact key because it had none, and it must take the content digest that
    -- key was derived from in the same breath, because CHECK submission_exact_key_implies_digest
    -- requires the two to arrive together. v_is_exact is defined above as "kind = 'prompt' AND
    -- content_digest IS NOT NULL", so an exact observation always carries the digest this needs.
    -- Taking the key alone -- which is what this statement used to do -- left the row holding an
    -- exact key with a NULL digest, and the UPDATE was rejected whenever the exact observation
    -- arrived on an equal- or worse-ranked route than the weak row's winner. Note that only
    -- strictly-better fidelity reached the upgrade branch, so the strictly-better case worked
    -- and hid the defect; see the equal-rank assertion in db/invariants.test.sql.
    --
    -- AT EQUAL RANK THIS IS DELIBERATE, NOT A BLEND OF TWO OBSERVATIONS. Docs/02 section 4.5
    -- ranks by route fidelity, so equal rank means the tie-break does not apply and the
    -- first-seen observation stays the winner: the adopting observation contributes only its
    -- key and its digest, while the incumbent keeps labels, classifier_version and confidence.
    -- Those content fields stay coherent because the exact key matched -- that same key is the
    -- evidence that both routes observed one submission, read the same content and derived the
    -- same digest -- and merge_confidence promotes to 'high' precisely because the row now
    -- carries an exact identity. This is a decision, recorded here so a later reader does not
    -- have to work out whether it was an oversight.
    UPDATE ingest.submission s
       SET first_occurred_at  = least(s.first_occurred_at, v_occurred),
           last_occurred_at   = greatest(s.last_occurred_at, v_occurred),
           observed_routes    = CASE WHEN v_source = ANY(s.observed_routes)
                                     THEN s.observed_routes
                                     ELSE s.observed_routes || v_source END,
           observation_count  = s.observation_count + 1,
           dedup_key          = coalesce(s.dedup_key, CASE WHEN v_is_exact THEN v_exact END),
           winning_source     = CASE WHEN v_fidelity < s.winning_fidelity THEN v_source ELSE s.winning_source END,
           winning_fidelity   = least(s.winning_fidelity, v_fidelity),
           collection_mode    = CASE WHEN v_fidelity < s.winning_fidelity THEN v_mode ELSE s.collection_mode END,
           -- The request kind travels with the winning observation like collection_mode, except
           -- that an observation which did not decide one does not erase a kind an earlier route
           -- did supply: coalesce keeps the first known value.
           prompt_kind        = CASE WHEN v_fidelity < s.winning_fidelity
                                     THEN nullif(p_envelope->>'prompt_kind', '')
                                     ELSE coalesce(s.prompt_kind, nullif(p_envelope->>'prompt_kind', '')) END,
           size_bytes         = CASE WHEN v_fidelity < s.winning_fidelity THEN (p_envelope->>'size_bytes')::bigint ELSE s.size_bytes END,
           -- Same coalesce discipline as dedup_key above: adopt the digest with the key, so the
           -- pair can never diverge. The strictly-better branch is unchanged.
           content_digest     = CASE WHEN v_fidelity < s.winning_fidelity
                                     THEN p_envelope->>'content_digest'
                                     ELSE coalesce(s.content_digest,
                                                   CASE WHEN v_is_exact THEN p_envelope->>'content_digest' END) END,
           labels             = CASE WHEN v_fidelity < s.winning_fidelity THEN p_envelope->'labels' ELSE s.labels END,
           -- The name travels with the winning observation like the other per-observation fields,
           -- except that a route which could not attribute a person does not erase a name an earlier
           -- route did supply: coalesce keeps the first known name. A name is not content, so it is
           -- safe to combine across two observations of the same submission, which is one person.
           subject_name       = CASE WHEN v_fidelity < s.winning_fidelity
                                     THEN v_subject_name
                                     ELSE coalesce(s.subject_name, v_subject_name) END,
           classifier_version = CASE WHEN v_fidelity < s.winning_fidelity THEN p_envelope->>'classifier_version' ELSE s.classifier_version END,
           confidence         = CASE WHEN v_fidelity < s.winning_fidelity THEN p_envelope->>'confidence' ELSE s.confidence END,
           -- A row that still has no exact key is the honest lower bound on that count, so it
           -- stays flagged: brief §7 requires an undercount to be visible rather than silent.
           -- "still has no exact key" is a claim about the row AFTER this update, so the adopt
           -- case promotes to 'high' instead of keeping the old flag. Leaving an adopted row
           -- flagged 'low' would count a submission we did merge among the ones we could not,
           -- which is the same class of quiet error the flag exists to prevent, in the other
           -- direction.
           merge_confidence   = CASE WHEN s.dedup_key IS NULL AND NOT v_is_exact THEN 'low'
                                     WHEN s.dedup_key IS NULL AND v_is_exact THEN 'high'
                                     ELSE s.merge_confidence END,
           expires_at         = greatest(s.expires_at, p_received_at + make_interval(days => v_ttl))
     WHERE s.tenant_id = v_tenant AND s.submission_id = v_sub_id;

    RETURN QUERY SELECT 'merged'::text, v_sub_id;
    RETURN;
  END IF;

  INSERT INTO ingest.submission (
    tenant_id, submission_id, dedup_key, dedup_weak_key, kind, prompt_kind, device_id, user_ref, subject_name,
    tool_fingerprint, first_occurred_at, last_occurred_at, received_at, collection_mode, size_bytes,
    content_digest, labels, classifier_version, confidence, policy_action, policy_rule_id,
    decided_locally, winning_source, winning_fidelity, observed_routes, observation_count,
    merge_confidence, expires_at
  )
  VALUES (
    v_tenant,
    gen_random_uuid(),
    CASE WHEN v_is_exact THEN v_exact END,
    v_weak,
    v_kind,
    nullif(p_envelope->>'prompt_kind', ''),
    (p_envelope->>'device_id')::uuid,
    v_user_ref,
    v_subject_name,
    p_envelope->>'tool_fingerprint',
    v_occurred, v_occurred, p_received_at, v_mode,
    (p_envelope->>'size_bytes')::bigint,
    p_envelope->>'content_digest',
    p_envelope->'labels',
    p_envelope->>'classifier_version',
    p_envelope->>'confidence',
    p_envelope->'policy_decision'->>'action',
    p_envelope->'policy_decision'->>'rule_id',
    (p_envelope->'policy_decision'->>'decided_locally')::boolean,
    v_source, v_fidelity, ARRAY[v_source], 1,
    CASE WHEN v_is_exact THEN 'high' ELSE 'low' END,
    p_received_at + make_interval(days => v_ttl)
  )
  RETURNING ingest.submission.submission_id INTO v_sub_id;

  RETURN QUERY SELECT 'inserted'::text, v_sub_id;
END $$;

COMMENT ON FUNCTION ingest.record_event(jsonb, timestamptz) IS
  'The single write path for events. Records the observation idempotently, then folds it into the logical submission using the fidelity tie-break, and returns the per-event outcome the batch response reports. Implements brief C12 (idempotency enforced by the store, not by a check in code) and R9 (no double-counting, and every count explainable from observed_routes and winning_source). Retention is materialised here from ops.retention_policy via ops.event_ttl_days. The person is stored under their canonical ref, resolved through ops.user_ref_alias; a ref with no alias is stored as sent.';

-- 99. Two grants the services need under their own roles (schema.sql states why). The audit chain
--     trigger reads the previous row's hash as the inserting role, so INSERT alone fails; and
--     control-api builds the policy bundle's interception hosts from the tool catalogue. GRANT is
--     idempotent, so a re-run changes nothing.
GRANT SELECT (tenant_id, audit_seq, row_hash) ON ops.audit TO sac_ingest, sac_control, sac_vault, sac_ops;
GRANT SELECT ON ref.tool_catalogue TO sac_control;

COMMIT;
