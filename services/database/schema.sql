-- =====================================================================================
-- Shadow AI Capture: PostgreSQL 16 schema
-- =====================================================================================
-- Records what employees send to generative AI tools from managed devices.
--
-- This file is schema version 1 and always describes the full current schema. The migrate job
-- applies it to an empty database; every later change is also shipped as a numbered file in
-- migrations/ so that existing databases reach the same shape.
--
-- Four schemas:
--   ref     shared reference data, not tenant-scoped
--   ops     tenants, devices, identity, configuration, content and evidence (audit, receipts)
--   ingest  immutable observations and the deduplicated logical submission
--   mart    aggregates and findings, rebuildable from ingest; no workflow state
--
-- Tenant isolation is enforced by the database. Every tenant-scoped table has row-level security
-- enabled and forced, and its policy compares tenant_id with the session setting app.tenant_id, so
-- a session that has not set a tenant reads nothing. Tenant is the leading column of every primary
-- key and index.
--
-- The file applies as a non-superuser that may create roles (an Azure Database for PostgreSQL
-- administrator). It needs the pg_trgm and btree_gin extensions.
-- =====================================================================================


-- =====================================================================================
-- 1. Schemas and extensions
-- =====================================================================================

CREATE SCHEMA ref;
CREATE SCHEMA ops;
CREATE SCHEMA ingest;
CREATE SCHEMA mart;

-- Both serve the content search index: pg_trgm for substring and fuzzy matching over attachment
-- filenames, btree_gin so a GIN index can lead with tenant_id.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS btree_gin;

COMMENT ON SCHEMA ref    IS 'Shared reference data. Not tenant-scoped; readable by the runtime roles.';
COMMENT ON SCHEMA ops    IS 'Tenants, devices, identity, configuration, stored content and evidence.';
COMMENT ON SCHEMA ingest IS 'Immutable observations and deduplicated logical submissions. Rows are removed only by whole-record expiry or erasure.';
COMMENT ON SCHEMA mart   IS 'Derived aggregates and findings. Rebuildable from ingest; holds no workflow state.';


-- =====================================================================================
-- 2. Roles
-- =====================================================================================
-- One role per runtime component, so what a component may touch is a grant. None can log in: the
-- migrate job creates a login per component and grants it the matching role.

CREATE ROLE sac_ingest   NOLOGIN;   -- ingest-api
CREATE ROLE sac_control  NOLOGIN;   -- control-api
CREATE ROLE sac_vault    NOLOGIN;   -- content-vault
CREATE ROLE sac_query    NOLOGIN;   -- query-api
CREATE ROLE sac_ops      NOLOGIN;   -- scheduled jobs (aggregate, expire)
-- Owns the SECURITY DEFINER functions that read across tenants (section 5d) and nothing else. No
-- runtime role is a member, so its reach is exactly those function bodies.
CREATE ROLE sac_resolver NOLOGIN;

GRANT USAGE ON SCHEMA ref    TO sac_ingest, sac_control, sac_vault, sac_query, sac_ops;
GRANT USAGE ON SCHEMA ops    TO sac_ingest, sac_control, sac_vault, sac_query, sac_ops, sac_resolver;
GRANT USAGE ON SCHEMA ingest TO sac_ingest, sac_control, sac_vault, sac_query, sac_ops;
GRANT USAGE ON SCHEMA mart   TO sac_query, sac_ops, sac_control;


-- =====================================================================================
-- 3. Helper functions
-- =====================================================================================

-- The tenant of the current session. The application sets app.tenant_id from the authenticated
-- principal, never from a request body. Unset yields NULL, and a policy comparison with NULL
-- excludes every row.
CREATE FUNCTION ops.current_tenant() RETURNS uuid
LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;

COMMENT ON FUNCTION ops.current_tenant() IS
  'Tenant from the session setting app.tenant_id. NULL when unset, which makes every row-level security comparison exclude every row.';

-- Collection modes are ordered, and the most restrictive applicable mode wins. Encoding the order
-- once keeps the ceiling trigger and the application in agreement.
CREATE FUNCTION ops.mode_rank(mode text) RETURNS int
LANGUAGE sql IMMUTABLE AS $$
  SELECT CASE mode
           WHEN 'm0' THEN 0
           WHEN 'm1' THEN 1
           WHEN 'm2' THEN 2
           WHEN 'm3' THEN 3
         END
$$;

COMMENT ON FUNCTION ops.mode_rank(text) IS
  'Numeric order of collection modes. NULL for an unrecognised mode, which callers treat as invalid rather than permissive.';


-- =====================================================================================
-- 4. ref: shared reference data
-- =====================================================================================

-- The classification catalogue. A label is a class code with a confidence, never a boolean.
CREATE TABLE ref.data_class (
  class_code        text PRIMARY KEY,
  category          text NOT NULL,
  description       text NOT NULL,
  default_severity  text NOT NULL CHECK (default_severity IN ('low','medium','high','critical')),
  detector_family   text NOT NULL CHECK (detector_family IN ('deterministic','statistical','both')),
  created_at        timestamptz NOT NULL DEFAULT now()
);

-- Rule metadata, so a finding can be shown with a human-readable rule without shipping rule bodies
-- to the query layer.
CREATE TABLE ref.rule (
  rule_id           text PRIMARY KEY,
  class_code        text NOT NULL REFERENCES ref.data_class(class_code),
  detector_kind     text NOT NULL CHECK (detector_kind IN ('deterministic','statistical')),
  severity          text NOT NULL CHECK (severity IN ('low','medium','high','critical')),
  title             text NOT NULL,
  description       text NOT NULL
);

-- The app catalog: the AI apps a device can find by what is installed or running on it. Devices
-- receive it in the policy bundle and report a match as the fingerprint 'app:' || app_key.
-- source_url names where the app's signals were verified.
CREATE TABLE ref.app (
  app_key       text PRIMARY KEY CHECK (app_key ~ '^[a-z][a-z0-9_]{1,63}$'),
  display_name  text NOT NULL,
  vendor        text NOT NULL,
  category      text NOT NULL CHECK (category IN ('chat_assistant','coding_agent','ide_assistant',
                  'ide','local_runtime','inference_api','ai_feature')),
  source_url    text NOT NULL
);

-- What identifies an app on a device. An inference_domain is an exact host or, with a leading dot,
-- a host suffix; a model_store may begin with %USERPROFILE% or ~, expanded per user.
CREATE TABLE ref.app_signal (
  app_key   text NOT NULL REFERENCES ref.app,
  platform  text NOT NULL CHECK (platform IN ('windows','macos','linux','any')),
  kind      text NOT NULL CHECK (kind IN ('windows_exe','windows_uninstall_name','windows_appx',
              'macos_bundle_id','linux_package','publisher','cli_binary','npm_package',
              'pipx_package','ide_extension_id','inference_domain','listen_port','model_store')),
  value     text NOT NULL,
  PRIMARY KEY (app_key, platform, kind, value)
);

-- The shared mapping from a behaviour-derived tool_fingerprint to the tool it belongs to. Devices
-- never emit a brand, only a fingerprint of what they observed; this table names the common ones
-- and links each to its app in ref.app, the unit a tenant sanctions. signal_kind and evidence record
-- how a fingerprint was derived, so a row can be regenerated and reviewed. An `endpoint` row is a
-- catalog app's own fingerprint, `app:<app_key>`.
CREATE TABLE ref.tool_catalogue (
  tool_fingerprint  text PRIMARY KEY,
  display_name      text NOT NULL,
  vendor            text,
  signal_kind       text NOT NULL CHECK (signal_kind IN ('tls','process','extension','other','endpoint')),
  evidence          jsonb NOT NULL DEFAULT '{}'::jsonb,
  app_key           text NOT NULL REFERENCES ref.app
);

-- Which of two observations of one submission wins. LOWER RANK IS BETTER, and the rank is about the
-- quality of the observed content, not how many tools a route covers.
CREATE TABLE ref.route_fidelity (
  source            text PRIMARY KEY,
  fidelity_rank     int NOT NULL UNIQUE,
  yields_content    boolean NOT NULL,
  description       text NOT NULL
);

-- The collection components, so ops.collector_state cannot name a path that does not exist.
CREATE TABLE ref.collector (
  collector_code    text PRIMARY KEY,
  component         text NOT NULL CHECK (component IN ('capture_extension','capture_core','classifier_host')),
  modes_supported   text[] NOT NULL,
  description       text NOT NULL
);

-- Retention classes for stored rows and content.
CREATE TABLE ref.retention_class (
  retention_class   text PRIMARY KEY,
  default_ttl_days  int NOT NULL CHECK (default_ttl_days > 0),
  description       text NOT NULL
);


-- =====================================================================================
-- 5. ops: tenants, devices, configuration, content and evidence
-- =====================================================================================

CREATE TABLE ops.tenant (
  tenant_id                    uuid PRIMARY KEY,
  name                         text NOT NULL,
  status                       text NOT NULL CHECK (status IN ('active','suspended','offboarding','closed')),
  -- The Azure region the tenant's data lives in. ingest-api and control-api refuse a tenant whose
  -- region differs from the deployment's.
  residency_region             text NOT NULL,
  -- The highest collection mode any policy bundle for this tenant may use (section 8).
  ceiling_mode                 text NOT NULL CHECK (ceiling_mode IN ('m0','m1','m2','m3')),
  -- The requested collection mode, set by an admin on the Settings page and delivered to devices as
  -- the bundle's tenant_default_mode. NULL means "follow the ceiling"; a value must not exceed it.
  collection_mode              text CHECK (collection_mode IN ('m0','m1','m2','m3')),
  -- Narrower per-tool modes (ref.app key -> mode), applied on top of collection_mode. An
  -- override must not exceed the requested mode, so it can never widen collection; enforced by
  -- enforce_scope_overrides (section 8).
  scope_overrides              jsonb NOT NULL DEFAULT '{}'::jsonb,
  -- Content search tier:
  --   disabled          structured filtering only (tool, user, date, class, rule, severity)
  --   attachment_names  adds substring and fuzzy search over attachment filenames (collected at M1)
  --   full_text         adds full-text search over prompt text (collected at M3)
  content_search               text NOT NULL DEFAULT 'disabled'
                                 CHECK (content_search IN ('disabled','attachment_names','full_text')),
  -- Ceiling on content bytes accepted per day; it bounds the one cost that grows with usage.
  content_budget_bytes_per_day bigint NOT NULL DEFAULT 0 CHECK (content_budget_bytes_per_day >= 0),
  -- 'clear': devices send, and reads return, the hostname and the submitting account name.
  -- 'hashed': devices send only a hostname hash and no account name.
  device_identity              text NOT NULL DEFAULT 'clear'
                                 CHECK (device_identity IN ('clear','hashed')),
  directory_source             text,
  -- 32 random bytes, sealed under the tenant's directory key, from which every device and the SCIM
  -- endpoint derive the same pseudonymous user_ref for one person. NULL until control-api first
  -- needs it. It cannot be regenerated: every enrolled device holds a copy.
  user_ref_key_enc             bytea,
  -- 'intune' also requires Microsoft Graph to confirm the device is Intune-managed at enrolment.
  -- control-api accepts it only while the tenant has an active Entra connection.
  device_verification          text NOT NULL DEFAULT 'none'
                                 CONSTRAINT tenant_device_verification_known
                                 CHECK (device_verification IN ('none','intune')),
  -- The two enforcement gates, separate from the commercial label in `status`. Closing ingest
  -- loses data that can never be collected again (devices drop it when their spools fill);
  -- closing reads is fully reversible. A person decides each, and the decision is attributed.
  ingest_enabled               boolean NOT NULL DEFAULT true,
  read_enabled                 boolean NOT NULL DEFAULT true,
  status_reason                text,
  status_changed_by            text,
  status_changed_at            timestamptz,
  created_at                   timestamptz NOT NULL DEFAULT now(),
  updated_at                   timestamptz NOT NULL DEFAULT now(),
  -- Whether devices intercept TLS: the local proxy, the CLI shim's proxy and CA environment, the
  -- Windows desktop-app PAC and the per-device root in the trust store. Set by an admin on the
  -- Settings page and delivered as the bundle's interception.enabled.
  tls_inspection               boolean NOT NULL DEFAULT false,
  -- A search tier is a capability over data the tenant can collect: filenames cross at M1, prompt
  -- text only at M3.
  CONSTRAINT tenant_search_tier_requires_collection_mode
    CHECK (content_search = 'disabled'
           OR (content_search = 'attachment_names' AND ceiling_mode <> 'm0')
           OR (content_search = 'full_text' AND ceiling_mode = 'm3')),
  -- The requested mode may not exceed the ceiling it is collected under.
  CONSTRAINT tenant_collection_within_ceiling
    CHECK (collection_mode IS NULL
           OR ops.mode_rank(collection_mode) <= ops.mode_rank(ceiling_mode)),
  CONSTRAINT tenant_closed_implies_gates_shut
    CHECK (status <> 'closed' OR (NOT ingest_enabled AND NOT read_enabled)),
  -- A closed gate always records who closed it, when and why.
  CONSTRAINT tenant_gate_closure_attributed
    CHECK ((ingest_enabled AND read_enabled)
           OR (status_reason IS NOT NULL
               AND status_changed_by IS NOT NULL
               AND status_changed_at IS NOT NULL))
);

COMMENT ON COLUMN ops.tenant.content_search IS
  'Ceiling for content search. The signed policy bundle narrows it per scope, so search is never enabled across everything at once.';

-- The organisational dimension, synchronised from the customer's directory.
CREATE TABLE ops.user_dim (
  tenant_id               uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  user_ref                text NOT NULL,
  -- The directory identifier, encrypted. The only column that maps a pseudonymous user_ref to a
  -- person; it exists for subject export and erasure.
  directory_object_id_enc bytea,
  department              text,
  population              text,
  manager_ref             text,
  -- The directory's display name. Written only while the tenant's device_identity is 'clear'.
  display_name            text,
  status                  text NOT NULL DEFAULT 'unknown' CHECK (status IN ('active','inactive','unknown')),
  synced_at               timestamptz,
  PRIMARY KEY (tenant_id, user_ref)
);

CREATE TABLE ops.device (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  device_id        uuid NOT NULL,
  -- The clear machine name while the tenant's device_identity is 'clear'; otherwise NULL, and
  -- hostname_hash carries the device-supplied hash.
  hostname         text,
  hostname_hash    text,
  -- A hash of the hardware identity: re-enrolment after a re-image returns the existing device_id.
  -- NULL when the device supplies none.
  hardware_identity_hash text,
  os               text NOT NULL CHECK (os IN ('windows','macos','linux')),
  os_version       text,
  agent_version    text,
  -- The Intune managed-device id Microsoft Graph confirmed at enrolment, for a tenant whose
  -- device_verification is 'intune'. A re-imaged machine Intune still knows keeps its device_id.
  intune_device_id text,
  managed_state    text NOT NULL DEFAULT 'unknown' CHECK (managed_state IN ('managed','unmanaged','unknown')),
  -- The base collection mode the device resolved from its signed bundle, as last reported.
  collection_mode  text CHECK (collection_mode IN ('m0','m1','m2','m3')),
  -- The most recent person on the device, kept by the ingest path (section 8) so the Devices view
  -- needs no join. last_subject_name is present only while device_identity is 'clear'.
  last_user_ref     text,
  last_subject_name text,
  residency_region text,
  enrolled_at      timestamptz NOT NULL DEFAULT now(),
  revoked_at       timestamptz,
  revoked_by       text,
  revoked_reason   text,
  last_seen_at     timestamptz,
  PRIMARY KEY (tenant_id, device_id)
);

-- One device per hardware identity per tenant. Partial, because the column is nullable and a
-- UNIQUE constraint would say nothing about the rows that matter while reading as if it did.
CREATE UNIQUE INDEX device_hardware_identity_uniq
  ON ops.device (tenant_id, hardware_identity_hash)
  WHERE hardware_identity_hash IS NOT NULL;

-- One Intune device is one product device.
CREATE UNIQUE INDEX device_intune_device_uniq
  ON ops.device (tenant_id, intune_device_id)
  WHERE intune_device_id IS NOT NULL;

-- The silent-device list and the coverage block order and filter on last_seen_at.
CREATE INDEX device_by_last_seen ON ops.device (tenant_id, last_seen_at);

-- A device's X.509 credential. control-api signs the certificate at enrolment from the device's
-- CSR; the private key never leaves the device. public_key_thumbprint is the SHA-256 of the
-- certificate's SubjectPublicKeyInfo, which the origins compare with the forwarded certificate.
-- Rotation revokes the previous row and inserts a new one, so the history is kept.
CREATE TABLE ops.device_credential (
  tenant_id             uuid NOT NULL,
  credential_id         uuid NOT NULL,
  device_id             uuid NOT NULL,
  public_key_thumbprint text NOT NULL,
  issued_at             timestamptz NOT NULL DEFAULT now(),
  expires_at            timestamptz NOT NULL,
  revoked_at            timestamptz,
  revoked_reason        text,
  last_used_at          timestamptz,
  PRIMARY KEY (tenant_id, credential_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),
  CONSTRAINT credential_expiry_after_issue CHECK (expires_at > issued_at)
);

CREATE INDEX device_credential_live_by_device
  ON ops.device_credential (tenant_id, device_id)
  WHERE revoked_at IS NULL;

-- Per-collector health, as an upsert per key rather than an event stream: hourly health events
-- would outnumber the prompt events the product collects. mart.agg_device_period is the daily
-- rollup. The four states are distinct facts and are never merged.
CREATE TABLE ops.collector_state (
  tenant_id            uuid NOT NULL,
  device_id            uuid NOT NULL,
  collector            text NOT NULL REFERENCES ref.collector(collector_code),
  state                text NOT NULL CHECK (state IN ('healthy','degraded','absent','tampered')),
  version              text,
  permissions          jsonb NOT NULL DEFAULT '{}'::jsonb,
  last_success_at      timestamptz,
  last_report_at       timestamptz NOT NULL DEFAULT now(),
  spool_depth          bigint CHECK (spool_depth >= 0),
  spool_capacity       bigint CHECK (spool_capacity >= 0),
  -- Monotonic count of events dropped because the device spool was full, so an undercount is
  -- visible rather than silent.
  spool_dropped_total  bigint NOT NULL DEFAULT 0 CHECK (spool_dropped_total >= 0),
  error_code           text,
  detail               jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (tenant_id, device_id, collector),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id)
);

CREATE INDEX collector_state_by_state ON ops.collector_state (tenant_id, state);

-- The signed policy bundle. A device that fails to verify a new bundle keeps the last version it
-- verified, which is what bundle_version orders.
CREATE TABLE ops.policy_bundle (
  tenant_id             uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  bundle_version        bigint NOT NULL,
  -- Collection mode per scope (tool, data class, population). The ceiling trigger (section 8)
  -- refuses a bundle in which any scope exceeds the tenant's ceiling.
  scope_matrix          jsonb NOT NULL,
  destination_allowlist jsonb NOT NULL DEFAULT '[]'::jsonb,
  retention_class       text NOT NULL REFERENCES ref.retention_class(retention_class),
  spool_bounds          jsonb NOT NULL DEFAULT '{}'::jsonb,
  feature_state         jsonb NOT NULL DEFAULT '{}'::jsonb,
  signature_kid         text NOT NULL,
  signed_digest         text NOT NULL,
  -- The exact bytes GET /v1/policy serves: the Ed25519 envelope {key_id, algorithm, payload,
  -- signature}. Stored rather than re-signed per request, so every device fetching one version
  -- receives identical bytes and the ETag names them.
  signed_envelope       bytea,
  effective_from        timestamptz NOT NULL DEFAULT now(),
  created_by            text NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, bundle_version),
  CONSTRAINT policy_bundle_digest_is_sha256
    CHECK (signed_digest ~ '^sha256:[0-9a-f]{64}$'),
  -- When the served bytes are stored, the digest is theirs.
  CONSTRAINT policy_bundle_digest_names_envelope
    CHECK (signed_envelope IS NULL
           OR signed_digest = 'sha256:' || encode(sha256(signed_envelope), 'hex'))
);

-- Per-tenant sanction decision for each tool (ref.app), one decision covering every fingerprint of
-- the tool. The same tool may be sanctioned by one tenant and not by another, and `unknown` is a
-- first-class value distinct from `unsanctioned`. A tool with no row is `unknown`.
CREATE TABLE ops.tool_sanction (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  tool_key          text NOT NULL REFERENCES ref.app,
  sanctioned_state  text NOT NULL CHECK (sanctioned_state IN ('sanctioned','unsanctioned','unknown')),
  decided_by        text,
  decided_at        timestamptz,
  PRIMARY KEY (tenant_id, tool_key),
  CONSTRAINT tool_sanction_attributed
    CHECK (sanctioned_state = 'unknown' OR (decided_by IS NOT NULL AND decided_at IS NOT NULL))
);

-- Which version of the monitoring notice a user acknowledged, and how.
CREATE TABLE ops.notice_acknowledgement (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  user_ref         text NOT NULL,
  notice_version   text NOT NULL,
  acknowledged_at  timestamptz NOT NULL,
  source           text NOT NULL CHECK (source IN ('enrolment','self_service','imported')),
  recorded_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_ref, notice_version)
);

-- Retention per kind of record, data class and collection mode. Events and content have separate
-- lifecycles.
CREATE TABLE ops.retention_policy (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  applies_to       text NOT NULL CHECK (applies_to IN ('event','content')),
  data_class       text NOT NULL REFERENCES ref.data_class(class_code),
  collection_mode  text NOT NULL CHECK (collection_mode IN ('m0','m1','m2','m3')),
  ttl_days         int NOT NULL CHECK (ttl_days > 0),
  updated_by       text NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, applies_to, data_class, collection_mode)
);

-- The tenant's endpoint collector switches, set on the Settings page and delivered in the policy
-- bundle's endpoint section. A tenant without a row is served these defaults: the policy read
-- applies them, so no row is inserted for a tenant that never changed a switch.
CREATE TABLE ops.endpoint_setting (
  tenant_id          uuid PRIMARY KEY REFERENCES ops.tenant(tenant_id),
  inventory          boolean NOT NULL DEFAULT true,
  processes          boolean NOT NULL DEFAULT true,
  flows              boolean NOT NULL DEFAULT true,
  otel               boolean NOT NULL DEFAULT true,
  hooks              boolean NOT NULL DEFAULT true,
  -- Tools run only the hooks the agent manages, not a user's own.
  hooks_managed_only boolean NOT NULL DEFAULT false
);

-- Per-tool native collectors. A tool's switch takes effect only while the collector it names is on
-- in ops.endpoint_setting. A tool without a row is served its defaults by the policy read: each
-- collector the tool supports is on. Ollama has no native collector: its one switch, loopback, has
-- the device move Ollama to another port and hold Ollama's own port with the loopback broker.
CREATE TABLE ops.endpoint_tool_setting (
  tenant_id  uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  tool_key   text NOT NULL CHECK (tool_key IN ('claude_code','codex','copilot','cursor','ollama')),
  otel       boolean NOT NULL,
  hooks      boolean NOT NULL,
  loopback   boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, tool_key)
);

-- The tenant's enforcement rules, in order, delivered in the policy bundle's rules list. The device
-- applies the first rule whose every non-empty match list matches. The list is replaced as a whole,
-- so position is dense from 0. A label must be a ref.data_class code (ops.enforce_rule_labels).
CREATE TABLE ops.enforcement_rule (
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  position    int NOT NULL CHECK (position >= 0),
  rule_id     text NOT NULL CHECK (rule_id ~ '^[a-z][a-z0-9_.-]{0,127}$'),
  action      text NOT NULL CHECK (action IN ('allow','warn','block')),
  match       jsonb NOT NULL DEFAULT '{}'::jsonb,
  message     text NOT NULL DEFAULT '' CHECK (char_length(message) <= 280),
  link        text CHECK (link LIKE 'https://_%'),
  updated_by  text NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, rule_id),
  CONSTRAINT enforcement_rule_position_unique UNIQUE (tenant_id, position),
  -- match holds only the five match lists, each an array of strings.
  CONSTRAINT enforcement_rule_match_shape CHECK (
    jsonb_typeof(match) = 'object'
    AND match - ARRAY['labels','tools','categories','sanction','routes'] = '{}'::jsonb
    AND NOT jsonb_path_exists(match, 'strict $.* ? (@.type() != "array")')
    AND NOT jsonb_path_exists(match, 'strict $.*[*] ? (@.type() != "string")', '{}', true))
);

-- The tenant's tripped kill switches, one per interception route, set on the Settings page and
-- delivered in the policy bundle's kill_switches. From effective_at the devices stop decrypting and
-- enforcing on that route and carry its traffic unread. Clearing a switch deletes its row.
CREATE TABLE ops.kill_switch (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  route         text NOT NULL CHECK (route IN ('proxy.tls','proxy.loopback')),
  reason_code   text NOT NULL CHECK (reason_code ~ '^[a-z][a-z0-9_.-]{0,63}$'),
  effective_at  timestamptz NOT NULL DEFAULT now(),
  set_by        text NOT NULL,
  PRIMARY KEY (tenant_id, route)
);

-- The append-only audit log: every read of subject-level data, mode change, content grant, content
-- reveal, export and configuration change, written before the data is returned. Append-only three
-- ways: no runtime role holds UPDATE or DELETE, a trigger refuses both for every role, and each row
-- hashes its predecessor in the same tenant, so removing a row breaks the chain.
CREATE TABLE ops.audit (
  tenant_id       uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  audit_seq       bigint GENERATED ALWAYS AS IDENTITY,
  occurred_at     timestamptz NOT NULL DEFAULT now(),
  actor_type      text NOT NULL CHECK (actor_type IN ('user','device','service','system')),
  actor_id        text NOT NULL,
  action          text NOT NULL,
  object_type     text NOT NULL,
  object_id       text,
  subject_ref     text,
  case_reference  text,
  detail          jsonb NOT NULL DEFAULT '{}'::jsonb,
  prev_hash       text,
  row_hash        text NOT NULL,
  PRIMARY KEY (tenant_id, audit_seq)
);

COMMENT ON COLUMN ops.audit.row_hash IS
  'sha256 over this row''s content and the previous row''s hash in the same tenant, computed by the ops.audit_chain() trigger.';

-- The per-event content upload grant. Content never arrives unsolicited: the device asks, control-api
-- decides, and a denial carries a reason. A grant is single-use: content-vault claims it by setting
-- used_at in the same transaction that stores the content, with
--   UPDATE ops.grant SET used_at = now()
--    WHERE ... AND used_at IS NULL AND decision = 'granted' AND expires_at > now()
-- so a second claim matches nothing. There is no bulk grant, so an "upload everything" state cannot
-- be configured.
CREATE TABLE ops.grant (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  grant_id          uuid NOT NULL,
  event_id          uuid NOT NULL,
  submission_id     uuid,
  device_id         uuid NOT NULL,
  requested_at      timestamptz NOT NULL DEFAULT now(),
  decided_at        timestamptz,
  decision          text NOT NULL CHECK (decision IN ('pending','granted','denied','expired','voided')),
  denial_reason     text CHECK (denial_reason IN ('retention_expired','not_policy_relevant','over_budget','mode_not_permitted')),
  -- When a granted upload must have happened by.
  expires_at        timestamptz,
  used_at           timestamptz,
  PRIMARY KEY (tenant_id, grant_id),
  -- Target of ops.content's foreign key, which ties stored content to the event it was granted for.
  UNIQUE (tenant_id, grant_id, event_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),
  CONSTRAINT grant_denial_requires_reason
    CHECK ((decision = 'denied') = (denial_reason IS NOT NULL)),
  CONSTRAINT grant_granted_has_expiry
    CHECK (decision <> 'granted' OR expires_at IS NOT NULL),
  CONSTRAINT grant_expiry_after_request
    CHECK (expires_at IS NULL OR expires_at > requested_at)
);

CREATE INDEX grant_by_event ON ops.grant (tenant_id, event_id, requested_at);

-- Stored prompt content, one object per event, encrypted by content-vault. ciphertext is the 12-byte
-- nonce followed by the AES-256-GCM output (ciphertext and 16-byte tag) under the tenant key derived
-- from the master key named by key_version; the additional data is tenant_id || '/' || object_id.
-- Only content-vault reads it. The expire job sees only the columns it needs to delete expired rows;
-- erasure and expiry are row deletes.
CREATE TABLE ops.content (
  tenant_id             uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  object_id             uuid NOT NULL,
  event_id              uuid NOT NULL,
  submission_id         uuid,
  grant_id              uuid NOT NULL,
  key_version           text NOT NULL,
  ciphertext            bytea NOT NULL,
  plaintext_size_bytes  int NOT NULL CHECK (plaintext_size_bytes BETWEEN 0 AND 16777216),
  -- sha256 of the plaintext as the device sent it, so an analyst can verify what they retrieve.
  raw_digest            text NOT NULL CHECK (raw_digest ~ '^sha256:[0-9a-f]{64}$'),
  retention_class       text NOT NULL REFERENCES ref.retention_class(retention_class),
  -- The device's request kind. A 'client_generated' object has no prompt text a person wrote and is
  -- never indexed for search. NULL reads as 'unknown'.
  prompt_kind           text CHECK (prompt_kind IN ('user','client_generated','unknown')),
  created_at            timestamptz NOT NULL DEFAULT now(),
  expires_at            timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, object_id),
  UNIQUE (tenant_id, event_id),
  -- One object per grant, and only for the event the grant was issued for.
  UNIQUE (tenant_id, grant_id),
  FOREIGN KEY (tenant_id, grant_id, event_id) REFERENCES ops.grant(tenant_id, grant_id, event_id),
  CONSTRAINT content_expiry_after_creation CHECK (expires_at > created_at),
  -- Nonce plus tag: a value of any other length is not the sealed form of plaintext_size_bytes bytes,
  -- which also refuses plaintext stored by mistake.
  CONSTRAINT content_ciphertext_is_sealed
    CHECK (octet_length(ciphertext) = plaintext_size_bytes + 28)
);

-- Ciphertext does not compress; store it out of line without trying.
ALTER TABLE ops.content ALTER COLUMN ciphertext SET STORAGE EXTERNAL;

CREATE INDEX content_expiry ON ops.content (tenant_id, expires_at);
CREATE INDEX content_by_submission ON ops.content (tenant_id, submission_id)
  WHERE submission_id IS NOT NULL;

-- A single-use grant to retrieve one stored content object, issued under two-person control.
-- Redemption is a conditional UPDATE matching used_at IS NULL, so a second redemption affects no
-- rows; the retrieval_grant_single_use trigger (section 8) also refuses any update of a redeemed
-- row, which catches a statement that forgets the guard. Rows are never deleted: a redeemed grant
-- is the record that content left the vault.
CREATE TABLE ops.retrieval_grant (
  tenant_id       uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  grant_id        uuid NOT NULL,
  event_id        uuid NOT NULL,
  object_id       uuid NOT NULL,
  -- NULL when the stored content was granted before its event joined a submission.
  submission_id   uuid,
  principal       text NOT NULL,
  case_reference  text NOT NULL,
  second_approver text NOT NULL,
  issued_at       timestamptz NOT NULL,
  expires_at      timestamptz NOT NULL,
  used_at         timestamptz,
  used_by         text,
  -- The digest of the bytes the retrieval returns, copied from ops.content.raw_digest.
  raw_digest      text NOT NULL,
  PRIMARY KEY (tenant_id, grant_id),
  CONSTRAINT retrieval_grant_second_approver_distinct CHECK (second_approver <> principal),
  CONSTRAINT retrieval_grant_window_bounded CHECK (expires_at > issued_at),
  CONSTRAINT retrieval_grant_raw_digest_is_sha256
    CHECK (raw_digest ~ '^sha256:[0-9a-f]{64}$'),
  -- A redeemed grant names both when and by whom, or neither.
  CONSTRAINT retrieval_grant_claim_is_whole CHECK ((used_at IS NULL) = (used_by IS NULL))
);

-- A verifiable record of what an erasure or expiry removed, when, and by which mechanism.
CREATE TABLE ops.erasure_receipt (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  receipt_id       uuid NOT NULL,
  scope_kind       text NOT NULL CHECK (scope_kind IN ('subject','tenant','retention')),
  subject_ref      text,
  requested_by     text NOT NULL,
  requested_at     timestamptz NOT NULL,
  completed_at     timestamptz,
  mechanisms       text[] NOT NULL DEFAULT '{}',
  removed_counts   jsonb NOT NULL DEFAULT '{}'::jsonb,
  -- What deliberately survived, and why, so a receipt never claims more than was removed.
  remaining_counts jsonb NOT NULL DEFAULT '{}'::jsonb,
  receipt_hash     text,
  PRIMARY KEY (tenant_id, receipt_id),
  CONSTRAINT erasure_subject_requires_ref CHECK (scope_kind <> 'subject' OR subject_ref IS NOT NULL)
);

-- A generated, downloadable artifact: a list-export CSV or a subject-export archive. The link names
-- an opaque id, the row is marked used on redemption, and the expire job sweeps rows past expires_at,
-- so it is single-use and short-lived. A subject export's archive holds the stored prompts the admin
-- asked for, so its payload is exactly as sensitive as the prompts themselves.
CREATE TABLE ops.export (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  export_id     uuid NOT NULL,
  kind          text NOT NULL CHECK (kind IN ('list','subject')),
  -- A list export: which list source the CSV holds. A subject export leaves this NULL.
  source        text CHECK (source IN ('ingest.submission','mart.v_finding')),
  -- A subject export: the person the archive holds. A list export leaves this NULL.
  subject_ref   text,
  row_count     int CHECK (row_count >= 0),
  content_type  text NOT NULL,
  payload       bytea NOT NULL,
  requested_by  text NOT NULL,
  requested_at  timestamptz NOT NULL,
  expires_at    timestamptz NOT NULL,
  used_at       timestamptz,
  PRIMARY KEY (tenant_id, export_id),
  CONSTRAINT export_kind_scope CHECK (
    (kind = 'list' AND source IS NOT NULL AND subject_ref IS NULL)
    OR (kind = 'subject' AND subject_ref IS NOT NULL AND source IS NULL)),
  CONSTRAINT export_expiry_after_request CHECK (expires_at > requested_at)
);

CREATE INDEX export_expiry ON ops.export (tenant_id, expires_at);

-- A pending subject erasure: an admin asked that one person's data be removed. query-api records the
-- request and audits it; the erase job performs the removal and writes the receipt, which is the
-- proof it happened.
CREATE TABLE ops.erasure_request (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  request_id    uuid NOT NULL,
  subject_ref   text NOT NULL,
  requested_by  text NOT NULL,
  requested_at  timestamptz NOT NULL,
  completed_at  timestamptz,
  receipt_id    uuid,
  PRIMARY KEY (tenant_id, request_id)
);

-- How current each aggregate is, so the dashboard never shows a number without its freshness.
CREATE TABLE ops.aggregate_watermark (
  tenant_id            uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  aggregate_name       text NOT NULL,
  bucket_size          text NOT NULL CHECK (bucket_size IN ('hour','day')),
  last_complete_bucket timestamptz NOT NULL,
  last_run_at          timestamptz NOT NULL,
  last_run_rows        bigint,
  PRIMARY KEY (tenant_id, aggregate_name, bucket_size)
);

-- Expected versus observed collection per device, collector and day, so a coverage gap is a named
-- row rather than a silent omission. An unexplained gap is recorded as `unknown`, never left blank.
CREATE TABLE ops.coverage_snapshot (
  tenant_id      uuid NOT NULL,
  snapshot_day   date NOT NULL,
  device_id      uuid NOT NULL,
  collector      text NOT NULL REFERENCES ref.collector(collector_code),
  expected       boolean NOT NULL,
  observed       boolean NOT NULL,
  gap_reason     text CHECK (gap_reason IN ('not_enrolled','not_managed','client_bypassed_proxy','pinned_certificate','permission_denied','process_excluded','tampered','unknown')),
  PRIMARY KEY (tenant_id, snapshot_day, device_id, collector),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),
  CONSTRAINT coverage_gap_requires_reason CHECK (observed OR gap_reason IS NOT NULL)
);

-- The gap list reads only rows where a collector was expected and did not report.
CREATE INDEX coverage_snapshot_unobserved
  ON ops.coverage_snapshot (tenant_id, snapshot_day) WHERE NOT observed;


-- =====================================================================================
-- 5b. Commercial state
-- =====================================================================================

-- The commercial arrangement. It holds no price: the system produces billable usage and pricing
-- lives in the billing system.
CREATE TABLE ops.subscription (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  subscription_id   uuid NOT NULL,
  plan_code         text NOT NULL,
  billing_basis     text[] NOT NULL DEFAULT ARRAY['tenant','device']
                      CHECK (billing_basis <@ ARRAY['tenant','device','events','content_bytes']
                             AND array_length(billing_basis, 1) >= 1),
  -- At most 28, so no billing period is shortened by February.
  period_anchor_day smallint NOT NULL DEFAULT 1 CHECK (period_anchor_day BETWEEN 1 AND 28),
  started_on        date NOT NULL,
  ended_on          date,
  -- Usage on or before this date has been invoiced and does not change.
  billed_through    date,
  notes             text,
  updated_by        text,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, subscription_id),
  CONSTRAINT subscription_period_ordered CHECK (ended_on IS NULL OR ended_on >= started_on)
);

-- The billable usage ledger: one row per tenant per day, counts only. It holds no subject reference
-- and no device identifier, so an erasure neither touches it nor changes what it says. It is written
-- forward as events are accepted and never recomputed from ingest, because a bill must not fall when
-- a subject's data is erased.
CREATE TABLE ops.usage_daily (
  tenant_id           uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  usage_day           date NOT NULL,
  -- Accepted, non-duplicate events, counted by ingest-api in the transaction that stores them.
  events_accepted     bigint NOT NULL DEFAULT 0 CHECK (events_accepted >= 0),
  content_bytes_added bigint NOT NULL DEFAULT 0 CHECK (content_bytes_added >= 0),
  -- End-of-day snapshot. A quiet device is still enrolled and still billable.
  devices_enrolled    int CHECK (devices_enrolled >= 0),
  devices_reporting   int CHECK (devices_reporting >= 0),
  snapshot_at         timestamptz,
  PRIMARY KEY (tenant_id, usage_day)
);


-- =====================================================================================
-- 5c. Identity, provisioning and deployment
-- =====================================================================================
-- control-api's state as the identity service: the relying party for each customer identity
-- provider, the server-side session, the SCIM endpoint and the source of each tenant's device
-- package.
--
-- The product tenant is decided by the product's own mapping (an Entra tenant id, an OIDC issuer or
-- an email domain, each unique across the database), never by a claim the customer's provider
-- chooses. Credentials are stored as hashes (sha256:<hex>) or sealed (`*_enc`: AES-256-GCM under a
-- per-tenant key derived from SAC_DIRECTORY_KEY), so a database read is never a credential.

-- One identity provider connection per row.
CREATE TABLE ops.identity_connection (
  connection_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  provider          text NOT NULL CHECK (provider IN ('entra','oidc')),
  -- The customer's Entra directory id, lower-case, as a token's `tid` spells it. The issuer is
  -- derived from it, so there is no issuer to store.
  entra_tenant_id   text UNIQUE
                      CONSTRAINT identity_connection_entra_tenant_is_uuid
                      CHECK (entra_tenant_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
  -- The exact `iss` an OIDC provider signs, compared byte for byte so two spellings cannot map to
  -- two tenants.
  issuer            text UNIQUE,
  -- An OIDC client registered in the customer's provider. Entra uses the vendor's multi-tenant
  -- application, so an Entra row carries neither.
  client_id         text,
  client_secret_enc bytea,
  scopes            text NOT NULL DEFAULT 'openid profile email',
  roles_claim       text NOT NULL DEFAULT 'roles',
  -- {"<provider value>": "<product role>"}. Empty means the provider already emits product role
  -- names. A user whose values map to no role is refused.
  role_map          jsonb NOT NULL DEFAULT '{}'::jsonb,
  status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','disabled')),
  created_at        timestamptz NOT NULL DEFAULT now(),
  activated_at      timestamptz,
  activated_by      text,
  -- Target of the session and role-grant foreign keys, so a row cannot point at another tenant's
  -- connection.
  UNIQUE (tenant_id, connection_id),
  CONSTRAINT identity_connection_entra_has_tid
    CHECK ((provider = 'entra') = (entra_tenant_id IS NOT NULL)),
  CONSTRAINT identity_connection_oidc_has_issuer_and_client
    CHECK ((provider = 'oidc') = (issuer IS NOT NULL AND client_id IS NOT NULL)),
  CONSTRAINT identity_connection_credentials_only_for_oidc
    CHECK (provider = 'oidc' OR (client_id IS NULL AND client_secret_enc IS NULL)),
  CONSTRAINT identity_connection_activation_attributed
    CHECK (status <> 'active' OR (activated_at IS NOT NULL AND activated_by IS NOT NULL)),
  CONSTRAINT identity_connection_role_map_names_product_roles
    CHECK (jsonb_typeof(role_map) = 'object'
           AND NOT jsonb_path_exists(role_map,
             '$.* ? (!(@ == "viewer" || @ == "analyst" || @ == "content_reader" || @ == "admin"))'))
);

-- Email domains for sign-in discovery. Set by the vendor when it issues the onboarding invite, never
-- claimed by a customer.
CREATE TABLE ops.tenant_email_domain (
  domain      text PRIMARY KEY
                CONSTRAINT tenant_email_domain_is_lower_hostname
                CHECK (domain = lower(domain)
                       AND domain ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$'),
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  text
);

CREATE INDEX tenant_email_domain_by_tenant ON ops.tenant_email_domain (tenant_id);

-- The one-time onboarding link the vendor issues; only its hash is stored. The first admin's
-- sign-in uses it and activates the connection.
CREATE TABLE ops.onboarding_invite (
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

CREATE INDEX onboarding_invite_by_tenant ON ops.onboarding_invite (tenant_id, created_at);

-- Roles the product grants to one person, in addition to those the provider's claim maps to. Keyed
-- by the provider's subject (Entra: `oid`), which a provider never reassigns.
CREATE TABLE ops.role_grant (
  tenant_id      uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  connection_id  uuid NOT NULL,
  subject        text NOT NULL,
  role           text NOT NULL CHECK (role IN ('viewer','analyst','content_reader','admin')),
  granted_by     text NOT NULL,
  granted_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, connection_id, subject, role),
  FOREIGN KEY (tenant_id, connection_id) REFERENCES ops.identity_connection(tenant_id, connection_id)
);

-- The server-side session behind the dashboard's cookie. Only the cookie's sha256 is stored. roles
-- is resolved at sign-in and never empty: a person with no role gets no session.
CREATE TABLE ops.auth_session (
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
  -- The person's canonical ref when SCIM knows them, so deprovisioning ends the session at the next
  -- token refresh.
  user_ref              text,
  idp_refresh_token_enc bytea,
  idp_refreshed_at      timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  last_seen_at          timestamptz NOT NULL DEFAULT now(),
  expires_at            timestamptz NOT NULL,
  revoked_at            timestamptz,
  FOREIGN KEY (tenant_id, connection_id) REFERENCES ops.identity_connection(tenant_id, connection_id),
  CONSTRAINT auth_session_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX auth_session_by_user_ref ON ops.auth_session (tenant_id, user_ref) WHERE revoked_at IS NULL;
CREATE INDEX auth_session_expiry ON ops.auth_session (tenant_id, expires_at);

-- A sign-in in progress: the PKCE verifier, state and nonce control-api generated, for ten minutes.
-- The one table without a tenant, because "Sign in with Microsoft" learns the tenant only from the
-- provider's answer. Row-level security still applies, with a policy for sac_control only.
CREATE TABLE ops.auth_signin (
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

CREATE INDEX auth_signin_expiry ON ops.auth_signin (expires_at);

-- The bearer an identity provider's SCIM client presents (sacscim_ prefix, tenant id in clear,
-- 256-bit random tail), stored as its hash. Revoked, never deleted, so audit entries keep resolving.
CREATE TABLE ops.scim_token (
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

CREATE INDEX scim_token_by_tenant ON ops.scim_token (tenant_id, created_at);

-- A person as the identity provider last provisioned them. The resource is sealed (it holds names
-- and addresses); filters are answered through keyed hashes, so no clear identifier is stored.
CREATE TABLE ops.scim_user (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  scim_id           uuid NOT NULL DEFAULT gen_random_uuid(),
  -- HMAC-SHA256 under the tenant's user-reference key of lower(userName); likewise externalId.
  user_name_hash    bytea NOT NULL CONSTRAINT scim_user_name_hash_is_hmac CHECK (octet_length(user_name_hash) = 32),
  external_id_hash  bytea CONSTRAINT scim_user_external_id_hash_is_hmac CHECK (octet_length(external_id_hash) = 32),
  resource_enc      bytea NOT NULL,
  -- The canonical ref, derived at creation and never changed: a rename becomes an alias, so a
  -- person's history does not split.
  user_ref          text NOT NULL CONSTRAINT scim_user_ref_is_derived CHECK (user_ref ~ '^u_[0-9a-f]{32}$'),
  active            boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, scim_id),
  UNIQUE (tenant_id, user_name_hash),
  -- Two people sharing one canonical ref would merge their histories. A recycled userName derives
  -- its previous owner's ref, so this must fail loudly.
  UNIQUE (tenant_id, user_ref)
);

CREATE INDEX scim_user_by_external_id ON ops.scim_user (tenant_id, external_id_hash)
  WHERE external_id_hash IS NOT NULL;

-- Groups as provisioned. A group name is organisational, so it is stored in clear.
CREATE TABLE ops.scim_group (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  scim_id       uuid NOT NULL DEFAULT gen_random_uuid(),
  display_name  text NOT NULL,
  external_id   text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, scim_id)
);

-- Membership is removed with either side.
CREATE TABLE ops.scim_group_member (
  tenant_id  uuid NOT NULL,
  group_id   uuid NOT NULL,
  user_id    uuid NOT NULL,
  PRIMARY KEY (tenant_id, group_id, user_id),
  FOREIGN KEY (tenant_id, group_id) REFERENCES ops.scim_group(tenant_id, scim_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, user_id) REFERENCES ops.scim_user(tenant_id, scim_id) ON DELETE CASCADE
);

CREATE INDEX scim_group_member_by_user ON ops.scim_group_member (tenant_id, user_id);

-- Device-derived ref -> canonical ref. A device derives a person's ref from whatever it can resolve
-- (a UPN, an Entra object id, an account name), so one person reaches the server under several refs.
-- ingest.record_event resolves through this table, so stored events carry the canonical ref. Rows
-- outlive deprovisioning, so a later event cannot start a second history for the same person.
CREATE TABLE ops.user_ref_alias (
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  alias_ref   text NOT NULL CONSTRAINT user_ref_alias_is_derived CHECK (alias_ref ~ '^u_[0-9a-f]{32}$'),
  user_ref    text NOT NULL CONSTRAINT user_ref_alias_target_is_derived CHECK (user_ref ~ '^u_[0-9a-f]{32}$'),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, alias_ref)
);

-- Subject export and erasure start from the canonical ref and need every alias of it.
CREATE INDEX user_ref_alias_by_canonical ON ops.user_ref_alias (tenant_id, user_ref);

-- The per-tenant key a device package carries (sacdk_ prefix, tenant id in clear), stored as its
-- hash. A first enrolment presents it. One key enrols many devices, so it is counted, revocable and
-- optionally time-bounded rather than consumed; a new key is minted per package download, so
-- revoking one leaked package leaves the others working.
CREATE TABLE ops.deployment_key (
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

CREATE INDEX deployment_key_by_tenant ON ops.deployment_key (tenant_id, created_at);


-- =====================================================================================
-- 5d. Cross-tenant lookups
-- =====================================================================================
-- A few questions must be answered before any tenant is known ("which tenant is this issuer?",
-- "whose session is this cookie?"), and the jobs must list the tenants they iterate over. Those are
-- the only reads that cross tenants, so they are functions rather than grants: SECURITY DEFINER,
-- owned by sac_resolver (section 10), whose only privileges are SELECT on the tables they read and a
-- SELECT-only policy on each (section 9). search_path is pinned and every name is schema-qualified,
-- and EXECUTE is revoked from PUBLIC and granted only to the role that calls each one. Each returns
-- only what may be used (an active connection, an unused invite, a live session, an unrevoked
-- token), so a caller bug cannot revive a revoked credential.

CREATE FUNCTION ops.identity_connection_for_entra(p_entra_tenant text)
RETURNS SETOF ops.identity_connection
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.*
    FROM ops.identity_connection c
   WHERE c.provider = 'entra'
     AND c.entra_tenant_id = lower(p_entra_tenant)
     AND c.status = 'active'
$$;

COMMENT ON FUNCTION ops.identity_connection_for_entra(text) IS
  'The active Entra connection for a token''s tid, or nothing.';

CREATE FUNCTION ops.identity_connection_for_issuer(p_issuer text)
RETURNS SETOF ops.identity_connection
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.*
    FROM ops.identity_connection c
   WHERE c.provider = 'oidc'
     AND c.issuer = p_issuer
     AND c.status = 'active'
$$;

COMMENT ON FUNCTION ops.identity_connection_for_issuer(text) IS
  'The active OIDC connection whose issuer is exactly p_issuer, or nothing.';

-- Any status: onboarding completes a first admin's sign-in through a connection that is still
-- pending, and the caller decides what each status permits.
CREATE FUNCTION ops.identity_connection_by_id(p_connection uuid)
RETURNS SETOF ops.identity_connection
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.*
    FROM ops.identity_connection c
   WHERE c.connection_id = p_connection
$$;

COMMENT ON FUNCTION ops.identity_connection_by_id(uuid) IS
  'One connection by id in any status, for completing a sign-in that began against it.';

CREATE FUNCTION ops.tenant_for_email_domain(p_domain text)
RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT d.tenant_id
    FROM ops.tenant_email_domain d
   WHERE d.domain = lower(p_domain)
$$;

COMMENT ON FUNCTION ops.tenant_for_email_domain(text) IS
  'The tenant a sign-in email domain belongs to, or NULL.';

CREATE FUNCTION ops.onboarding_invite_by_hash(p_hash text)
RETURNS SETOF ops.onboarding_invite
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT i.*
    FROM ops.onboarding_invite i
   WHERE i.token_hash = p_hash
     AND i.used_at IS NULL
     AND i.expires_at > now()
$$;

COMMENT ON FUNCTION ops.onboarding_invite_by_hash(text) IS
  'An unused, unexpired onboarding invite, or nothing. A used invite does not resolve, so a forwarded link cannot onboard a second admin.';

CREATE FUNCTION ops.auth_session_by_hash(p_hash bytea)
RETURNS SETOF ops.auth_session
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT s.*
    FROM ops.auth_session s
   WHERE s.session_hash = p_hash
     AND s.revoked_at IS NULL
     AND s.expires_at > now()
$$;

COMMENT ON FUNCTION ops.auth_session_by_hash(bytea) IS
  'The live session behind a cookie (by the sha256 of its value), or nothing. The idle limit and SCIM deactivation are checked by the caller.';

CREATE FUNCTION ops.tenant_for_scim_token(p_hash text)
RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT t.tenant_id
    FROM ops.scim_token t
   WHERE t.token_hash = p_hash
     AND t.revoked_at IS NULL
$$;

COMMENT ON FUNCTION ops.tenant_for_scim_token(text) IS
  'The tenant an unrevoked SCIM bearer belongs to, or NULL.';

-- Every tenant id, for the scheduled jobs, which run per tenant and cannot list ops.tenant under
-- forced row-level security.
CREATE FUNCTION ops.tenant_ids()
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT t.tenant_id
    FROM ops.tenant t
   ORDER BY t.tenant_id
$$;

COMMENT ON FUNCTION ops.tenant_ids() IS
  'Every tenant id, in order. Only the ids: each job then sets app.tenant_id and reads under that tenant''s row-level security.';


-- =====================================================================================
-- 6. ingest: immutable observations and the logical submission
-- =====================================================================================

-- One row per observation. Two routes observing one submission produce two rows here and one row in
-- ingest.submission. UPDATE is refused outright; DELETE only by the retention path, which sets a
-- session flag (section 8).
CREATE TABLE ingest.observation (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  event_id          uuid NOT NULL,
  device_id         uuid NOT NULL,
  user_ref          text NOT NULL,
  -- The clear account name, present only while the tenant's device_identity is 'clear' and the
  -- device could attribute the observation to a person.
  subject_name      text,
  tool_fingerprint  text NOT NULL,
  direction         text NOT NULL CHECK (direction IN ('egress','ingress','none')),
  kind              text NOT NULL CHECK (kind IN ('prompt','usage_rollup','discovery','agent_activity')),
  -- What the device decided this prompt is, from the shape of the request: text a person typed
  -- ('user'), a request the client made for itself ('client_generated'), or undecided ('unknown').
  -- Present on prompts at M1 and above; NULL otherwise, which reads as 'unknown'.
  prompt_kind       text CHECK (prompt_kind IN ('user','client_generated','unknown')),
  occurred_at       timestamptz NOT NULL,
  received_at       timestamptz NOT NULL,
  monotonic_offset_ms bigint NOT NULL CHECK (monotonic_offset_ms >= 0),
  source            text NOT NULL REFERENCES ref.route_fidelity(source),
  confidence        text CHECK (confidence IN ('high','medium','low','degraded')),
  collection_mode   text NOT NULL CHECK (collection_mode IN ('m0','m1','m2','m3')),
  size_bytes        bigint CHECK (size_bytes >= 0),
  content_digest    text CHECK (content_digest ~ '^sha256:[0-9a-f]{64}$'),
  labels            jsonb,
  classifier_version text,
  content_excerpt   jsonb,
  policy_decision   jsonb,
  window_start      timestamptz,
  window_end        timestamptz,
  submission_count  bigint CHECK (submission_count >= 0),
  bytes_total       bigint CHECK (bytes_total >= 0),
  detection_basis   text CHECK (detection_basis IN ('installed_scan','package_scan','extension_scan','process_event','model_store','port_listen','flow_metadata')),
  dedup_key         text NOT NULL CHECK (dedup_key ~ '^sha256:[0-9a-f]{64}$'),
  schema_version    text NOT NULL,
  ingested_at       timestamptz NOT NULL DEFAULT now(),
  -- Materialised at write time from ops.retention_policy, so a later policy change does not
  -- retroactively apply to data collected under a different one.
  expires_at        timestamptz NOT NULL,
  -- The columns below follow expires_at because migrations append columns: a migrated table and
  -- one built from this file then have the same column order.
  -- What a discovery found on the device.
  discovery_type    text CHECK (discovery_type IN ('app_installed','app_running','cli_installed','ide_extension','local_model','inference_connection')),
  app_version       text,
  publisher         text,
  host_app          text,
  destination_host  text,
  model_names       text[],
  -- One model request or tool call, as the agent's own telemetry reports it.
  activity_type     text CHECK (activity_type IN ('model_request','tool_call')),
  model             text,
  input_tokens      bigint CHECK (input_tokens >= 0),
  output_tokens     bigint CHECK (output_tokens >= 0),
  duration_ms       bigint CHECK (duration_ms >= 0),
  tool_name         text,
  outcome           text CHECK (outcome IN ('success','error','denied')),
  PRIMARY KEY (tenant_id, event_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),

  -- The event envelope contract's kind and mode boundaries, enforced so that a record arriving by
  -- any path, not only the ingest gate, cannot carry a field its kind or mode forbids. Each list
  -- equals the matching branch of contracts/event-envelope.schema.json, minus `attachments`, which
  -- has no column here (attachment names are indexed in ingest.search_text). At M0 the collector
  -- reads no content, so a content-derived field on an M0 record is a defect and is refused.
  CONSTRAINT observation_m0_carries_no_content
    CHECK (collection_mode <> 'm0' OR (
      content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND confidence IS NULL AND content_excerpt IS NULL AND prompt_kind IS NULL)),
  CONSTRAINT observation_m1_plus_carries_labels
    CHECK (kind <> 'prompt' OR collection_mode = 'm0' OR (
      content_digest IS NOT NULL AND labels IS NOT NULL AND classifier_version IS NOT NULL
      AND confidence IS NOT NULL)),
  -- An excerpt is permitted only at M2. The contract also requires one there; that requirement is
  -- enforced at ingest, because an M2 record without an excerpt is less informative, not unsafe.
  CONSTRAINT observation_excerpt_only_at_m2
    CHECK (content_excerpt IS NULL OR (kind = 'prompt' AND collection_mode = 'm2')),
  CONSTRAINT observation_prompt_shape
    CHECK (kind <> 'prompt' OR (
      direction = 'egress' AND size_bytes IS NOT NULL AND policy_decision IS NOT NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL AND detection_basis IS NULL
      AND discovery_type IS NULL AND app_version IS NULL AND publisher IS NULL
      AND host_app IS NULL AND destination_host IS NULL AND model_names IS NULL
      AND activity_type IS NULL AND model IS NULL AND input_tokens IS NULL
      AND output_tokens IS NULL AND duration_ms IS NULL AND tool_name IS NULL AND outcome IS NULL)),
  CONSTRAINT observation_rollup_shape
    CHECK (kind <> 'usage_rollup' OR (
      direction = 'none' AND window_start IS NOT NULL AND window_end IS NOT NULL
      AND submission_count IS NOT NULL AND bytes_total IS NOT NULL
      AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND content_excerpt IS NULL AND policy_decision IS NULL AND size_bytes IS NULL
      AND detection_basis IS NULL AND prompt_kind IS NULL
      AND discovery_type IS NULL AND app_version IS NULL AND publisher IS NULL
      AND host_app IS NULL AND destination_host IS NULL AND model_names IS NULL
      AND activity_type IS NULL AND model IS NULL AND input_tokens IS NULL
      AND output_tokens IS NULL AND duration_ms IS NULL AND tool_name IS NULL AND outcome IS NULL)),
  CONSTRAINT observation_discovery_shape
    CHECK (kind <> 'discovery' OR (
      direction = 'none' AND discovery_type IS NOT NULL AND detection_basis IS NOT NULL
      AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND content_excerpt IS NULL AND confidence IS NULL AND prompt_kind IS NULL
      AND policy_decision IS NULL AND size_bytes IS NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL
      AND activity_type IS NULL AND model IS NULL AND input_tokens IS NULL
      AND output_tokens IS NULL AND duration_ms IS NULL AND tool_name IS NULL AND outcome IS NULL)),
  CONSTRAINT observation_activity_shape
    CHECK (kind <> 'agent_activity' OR (
      direction = 'none' AND activity_type IS NOT NULL
      AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND content_excerpt IS NULL AND confidence IS NULL AND prompt_kind IS NULL
      AND policy_decision IS NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL AND detection_basis IS NULL
      AND discovery_type IS NULL AND app_version IS NULL AND publisher IS NULL
      AND host_app IS NULL AND destination_host IS NULL AND model_names IS NULL))
);

-- The expire job deletes per tenant by expires_at.
CREATE INDEX observation_expiry ON ingest.observation (tenant_id, expires_at);

-- The dedup key for an observation without a content digest (M0, a canvas UI, a WebSocket session,
-- a rollup, a discovery, an agent activity): tenant, device, tool, kind, a 300-second bucket and the
-- payload size. The server derives it, so independently written collectors agree. `kind` is part of
-- it because rollups and discoveries carry no size: without it, a rollup and a discovery of one tool
-- in one bucket would collapse into one record.
CREATE FUNCTION ingest.weak_dedup_key(
  p_tenant uuid, p_device uuid, p_tool text, p_kind text, p_occurred timestamptz, p_size bigint
) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
  SELECT 'sha256:' || encode(sha256(convert_to(concat_ws('|',
           p_tenant::text,
           p_device::text,
           p_tool,
           p_kind,
           (floor(extract(epoch FROM p_occurred) / 300))::bigint::text,
           coalesce(p_size, 0)::text), 'UTF8')), 'hex')
$$;

-- The deduplicated logical submission. Every aggregate and finding is derived from it, so two routes
-- observing one submission count once; observed_routes and winning_source explain each count.
--
-- Two keys with deliberately asymmetric uniqueness:
--   * exact keys (from a content digest) are unique among themselves, so two different digests stay
--     two rows and a canonicalisation divergence stays visible;
--   * the weak key binds only rows with no exact key, so a weak observation cannot pull a confident
--     row into a merge.
-- An exact observation matching a weak-only row adopts it rather than inserting a twin.
CREATE TABLE ingest.submission (
  tenant_id          uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id      uuid NOT NULL,
  dedup_key          text CHECK (dedup_key ~ '^sha256:[0-9a-f]{64}$'),
  dedup_weak_key     text NOT NULL CHECK (dedup_weak_key ~ '^sha256:[0-9a-f]{64}$'),
  kind               text NOT NULL CHECK (kind IN ('prompt','usage_rollup','discovery','agent_activity')),
  -- The winning observation's request kind; NULL for every other kind and for M0 prompts, read as
  -- 'unknown'. A 'client_generated' submission is never indexed for prompt-text search.
  prompt_kind        text CHECK (prompt_kind IS NULL OR (prompt_kind IN ('user','client_generated','unknown') AND kind = 'prompt')),
  device_id          uuid NOT NULL,
  user_ref           text NOT NULL,
  -- The winning observation's account name, so the name and the content describe the same
  -- observation.
  subject_name       text,
  tool_fingerprint   text NOT NULL,
  first_occurred_at  timestamptz NOT NULL,
  last_occurred_at   timestamptz NOT NULL,
  received_at        timestamptz NOT NULL,
  collection_mode    text NOT NULL CHECK (collection_mode IN ('m0','m1','m2','m3')),
  size_bytes         bigint CHECK (size_bytes >= 0),
  content_digest     text CHECK (content_digest ~ '^sha256:[0-9a-f]{64}$'),
  labels             jsonb,
  classifier_version text,
  confidence         text CHECK (confidence IN ('high','medium','low','degraded')),
  policy_action      text CHECK (policy_action IN ('blocked','warned','logged')),
  policy_rule_id     text,
  decided_locally    boolean,
  winning_source     text NOT NULL REFERENCES ref.route_fidelity(source),
  -- The fidelity_rank of winning_source, so the upsert compares without a join. Lower is better.
  winning_fidelity   int NOT NULL,
  observed_routes    text[] NOT NULL,
  observation_count  int NOT NULL DEFAULT 1 CHECK (observation_count >= 1),
  -- 'low' while the row has only a weak key: such rows are counted and reported separately, so an
  -- undercount is visible rather than silent.
  merge_confidence   text NOT NULL DEFAULT 'high' CHECK (merge_confidence IN ('high','low')),
  -- not_captured: no content was taken. local_only: the device holds it. uploaded: stored in
  -- ops.content. shredded: stored and since destroyed.
  content_state      text NOT NULL DEFAULT 'not_captured' CHECK (content_state IN ('not_captured','local_only','uploaded','shredded')),
  shredded_reason    text CHECK (shredded_reason IN ('retention_expired','erasure','tenant_offboarded')),
  expires_at         timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, submission_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),
  CONSTRAINT submission_shredded_requires_reason
    CHECK ((content_state = 'shredded') = (shredded_reason IS NOT NULL)),
  CONSTRAINT submission_no_content_without_capture
    CHECK (content_state <> 'not_captured' OR shredded_reason IS NULL),
  -- An exact key is derived from a content digest, so it never appears without one.
  CONSTRAINT submission_exact_key_implies_digest
    CHECK (dedup_key IS NULL OR content_digest IS NOT NULL)
);

CREATE UNIQUE INDEX submission_exact_key_uniq
  ON ingest.submission (tenant_id, dedup_key)
  WHERE dedup_key IS NOT NULL;

CREATE UNIQUE INDEX submission_weak_key_uniq
  ON ingest.submission (tenant_id, dedup_weak_key)
  WHERE dedup_key IS NULL;

CREATE INDEX submission_expiry ON ingest.submission (tenant_id, expires_at);

-- Rejected envelopes, kept for diagnosis with a short TTL. Content does not leave the device for a
-- rejected record either, so the redacted envelope may carry the fields needed to diagnose a
-- collector defect but never content or anything derived from it.
CREATE TABLE ingest.rejected (
  tenant_id          uuid NOT NULL,
  rejected_id        bigint GENERATED ALWAYS AS IDENTITY,
  device_id          uuid,
  received_at        timestamptz NOT NULL DEFAULT now(),
  -- The per-event reason codes ingest-api quarantines, spelled as device/protocol.ReasonCode
  -- spells them. tenant_mismatch is never stored: an envelope naming another tenant has no honest
  -- tenant to be filed under.
  reason_code        text NOT NULL CHECK (reason_code IN (
                       'schema_violation','unsupported_schema_version','unknown_kind',
                       'mode_violation')),
  detail             jsonb NOT NULL DEFAULT '{}'::jsonb,
  field_presence     jsonb NOT NULL DEFAULT '{}'::jsonb,
  envelope_redacted  jsonb,
  expires_at         timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, rejected_id),
  CONSTRAINT rejected_holds_no_content
    CHECK (envelope_redacted IS NULL OR NOT (
      envelope_redacted ? 'content_excerpt'
      OR envelope_redacted ? 'content_bytes'
      OR envelope_redacted ? 'prompt_text'
      OR envelope_redacted ? 'attachments'
    )),
  CONSTRAINT rejected_no_digest
    CHECK (envelope_redacted IS NULL OR NOT (envelope_redacted ? 'content_digest'))
);

CREATE INDEX rejected_expiry ON ingest.rejected (tenant_id, expires_at);

-- The content search index: one row per prompt body (tenants whose content_search is full_text) and
-- per attachment filename (attachment_names and above). Attachment contents are not indexed.
--
-- The index is derived from plaintext and is readable by whoever can read the table, so only
-- content-vault reads it; query-api asks content-vault for hits. Rows die with their submission
-- (the foreign key cascades) and are swept by the expire job when the content they index expires.
CREATE TABLE ingest.search_text (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id uuid NOT NULL,
  unit_kind     text NOT NULL CHECK (unit_kind IN ('prompt_body','attachment_name')),
  -- 0 for the prompt body; 0..n for attachment filenames.
  unit_index    int NOT NULL DEFAULT 0 CHECK (unit_index >= 0),
  body          text NOT NULL CHECK (length(body) BETWEEN 1 AND 65536),
  -- The two-argument to_tsvector is IMMUTABLE and so legal in a generated column. 'simple' rather
  -- than a language configuration, because prompts are multilingual and stemming the wrong language
  -- is worse than not stemming.
  tsv           tsvector GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, body)) STORED,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, submission_id, unit_kind, unit_index),
  CONSTRAINT search_text_body_required CHECK (unit_kind <> 'prompt_body' OR unit_index = 0),
  FOREIGN KEY (tenant_id, submission_id)
    REFERENCES ingest.submission(tenant_id, submission_id) ON DELETE CASCADE
);

CREATE INDEX search_text_tsv_gin
  ON ingest.search_text USING gin (tenant_id, tsv);

-- Trigram matching for filenames only: full-text search is the tool for prose.
CREATE INDEX search_text_name_trgm
  ON ingest.search_text USING gin (tenant_id, body gin_trgm_ops)
  WHERE unit_kind = 'attachment_name';

CREATE INDEX search_text_expiry
  ON ingest.search_text (tenant_id, expires_at);


-- =====================================================================================
-- 7. mart: derived, rebuildable, no workflow state
-- =====================================================================================

-- Submissions that matched a rule. The natural key makes a rebuild produce identical rows, so it
-- cannot orphan a review in ops.finding_review. The rule's class, severity and title are read from
-- ref.rule at query time (mart.v_finding), so a rule edit shows on every finding that names it.
-- A finding is derived from its submission and is deleted with it.
CREATE TABLE mart.finding (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id    uuid NOT NULL,
  rule_id          text NOT NULL REFERENCES ref.rule(rule_id),
  detected_at      timestamptz NOT NULL,
  decided_locally  boolean NOT NULL,
  collection_mode  text NOT NULL CHECK (collection_mode IN ('m0','m1','m2','m3')),
  PRIMARY KEY (tenant_id, submission_id, rule_id),
  FOREIGN KEY (tenant_id, submission_id)
    REFERENCES ingest.submission(tenant_id, submission_id) ON DELETE CASCADE
);

-- Review state for findings. Kept out of mart, which is rebuildable, so a rebuild cannot destroy an
-- analyst's judgement; keyed like mart.finding. `open` means nobody has reviewed it, which is
-- neither disputed nor confirmed. A review does not outlive the submission it judges: expiry and
-- erasure delete it with the submission.
CREATE TABLE ops.finding_review (
  tenant_id      uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id  uuid NOT NULL,
  rule_id        text NOT NULL REFERENCES ref.rule(rule_id),
  review_state   text NOT NULL DEFAULT 'open' CHECK (review_state IN ('open','disputed','confirmed')),
  reviewed_by    text,
  reviewed_at    timestamptz,
  note           text,
  PRIMARY KEY (tenant_id, submission_id, rule_id),
  FOREIGN KEY (tenant_id, submission_id)
    REFERENCES ingest.submission(tenant_id, submission_id) ON DELETE CASCADE,
  CONSTRAINT finding_review_attributed
    CHECK (review_state = 'open' OR (reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL))
);

-- Aggregates. Each is written with INSERT .. ON CONFLICT DO UPDATE that replaces the bucket, never
-- increments it: devices flush late in bursts, so late events are normal, and a replaced bucket is
-- correct however often it is recomputed.

-- Usage per tool. Sanction state is joined from ops.tool_sanction at read time (mart.v_tool_usage),
-- so re-classifying a tool does not change the meaning of history.
CREATE TABLE mart.agg_tool_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  tool_fingerprint text NOT NULL,
  submissions      bigint NOT NULL CHECK (submissions >= 0),
  users            bigint NOT NULL CHECK (users >= 0),
  bytes_total      bigint NOT NULL CHECK (bytes_total >= 0),
  blocked          bigint NOT NULL DEFAULT 0 CHECK (blocked >= 0),
  warned           bigint NOT NULL DEFAULT 0 CHECK (warned >= 0),
  logged           bigint NOT NULL DEFAULT 0 CHECK (logged >= 0),
  -- A tool known only from a detection or a rollup has no submissions; these keep it in the list.
  detections       bigint NOT NULL DEFAULT 0 CHECK (detections >= 0),
  rollup_events    bigint NOT NULL DEFAULT 0 CHECK (rollup_events >= 0),
  -- Events recorded while the classifier could not run, so an outage reads as a fall in classifier
  -- health rather than a fall in sensitive data.
  degraded_events  bigint NOT NULL DEFAULT 0 CHECK (degraded_events >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, tool_fingerprint)
);

CREATE TABLE mart.agg_tool_user_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  tool_fingerprint text NOT NULL,
  user_ref         text NOT NULL,
  submissions      bigint NOT NULL CHECK (submissions >= 0),
  bytes_total      bigint NOT NULL CHECK (bytes_total >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, tool_fingerprint, user_ref)
);

-- Sensitive data per class. users is carried so the read layer can suppress small cells.
CREATE TABLE mart.agg_class_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  class_code       text NOT NULL REFERENCES ref.data_class(class_code),
  tool_fingerprint text NOT NULL,
  severity         text NOT NULL CHECK (severity IN ('low','medium','high','critical')),
  -- Part of the key, so a change in classifier behaviour shows as a version change rather than a
  -- silent shift in the numbers.
  classifier_version text NOT NULL,
  submissions      bigint NOT NULL CHECK (submissions >= 0),
  users            bigint NOT NULL CHECK (users >= 0),
  max_score        numeric(4,3) CHECK (max_score >= 0 AND max_score <= 1),
  degraded_events  bigint NOT NULL DEFAULT 0 CHECK (degraded_events >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, class_code, tool_fingerprint, severity, classifier_version)
);

-- Usage per team, from ops.user_dim. Empty for a tenant without a directory sync.
CREATE TABLE mart.agg_org_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  department       text NOT NULL,
  population       text,
  tool_fingerprint text NOT NULL,
  submissions      bigint NOT NULL CHECK (submissions >= 0),
  users            bigint NOT NULL CHECK (users >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, department, tool_fingerprint, population)
);

-- A per-person time series, so every read is subject-level and audited. It carries no score or
-- ranking: the product does not do productivity analytics.
CREATE TABLE mart.agg_user_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  user_ref         text NOT NULL,
  submissions      bigint NOT NULL CHECK (submissions >= 0),
  bytes_total      bigint NOT NULL CHECK (bytes_total >= 0),
  tools_used       bigint NOT NULL CHECK (tools_used >= 0),
  block_events     bigint NOT NULL DEFAULT 0 CHECK (block_events >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, user_ref)
);

-- The daily rollup of ops.collector_state. The four state counts stay separate columns.
CREATE TABLE mart.agg_device_period (
  tenant_id       uuid NOT NULL,
  bucket_start    timestamptz NOT NULL,
  bucket_size     text NOT NULL CHECK (bucket_size IN ('hour','day')),
  device_id       uuid NOT NULL,
  collector       text NOT NULL REFERENCES ref.collector(collector_code),
  healthy_days    int NOT NULL DEFAULT 0 CHECK (healthy_days >= 0),
  degraded_days   int NOT NULL DEFAULT 0 CHECK (degraded_days >= 0),
  absent_days     int NOT NULL DEFAULT 0 CHECK (absent_days >= 0),
  tampered_days   int NOT NULL DEFAULT 0 CHECK (tampered_days >= 0),
  spool_dropped   bigint NOT NULL DEFAULT 0 CHECK (spool_dropped >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, device_id, collector)
);

-- Device liveness. A device that has stopped sends nothing, so its silence is turned into an
-- explicit state here. `stale`, `never_reported` and `revoked` are different facts. One row per
-- device: its collectors' states are summed beside it, so a read never fans out per collector.
-- The view carries the most recent user, so a read of it is subject-level and audited.
CREATE VIEW mart.v_device_liveness
WITH (security_invoker = true) AS
SELECT d.tenant_id,
       d.device_id,
       d.hostname,
       d.os,
       d.os_version,
       d.agent_version,
       d.managed_state,
       d.collection_mode,
       d.last_user_ref,
       d.last_subject_name,
       d.enrolled_at,
       d.revoked_at,
       d.last_seen_at,
       CASE
         WHEN d.revoked_at IS NOT NULL THEN 'revoked'
         WHEN d.last_seen_at IS NULL THEN 'never_reported'
         WHEN d.last_seen_at < now() - interval '24 hours' THEN 'stale'
         ELSE 'reporting'
       END AS liveness,
       c.collectors_reporting,
       c.collectors_healthy,
       c.collectors_degraded,
       c.collectors_absent,
       c.collectors_tampered,
       c.collectors,
       c.collector_states,
       c.spool_depth,
       c.spool_dropped_total,
       c.last_report_at
  FROM ops.device d
  LEFT JOIN LATERAL (
    SELECT count(*)::int                                            AS collectors_reporting,
           count(*) FILTER (WHERE cs.state = 'healthy')::int        AS collectors_healthy,
           count(*) FILTER (WHERE cs.state = 'degraded')::int       AS collectors_degraded,
           count(*) FILTER (WHERE cs.state = 'absent')::int         AS collectors_absent,
           count(*) FILTER (WHERE cs.state = 'tampered')::int       AS collectors_tampered,
           coalesce(array_agg(cs.collector ORDER BY cs.collector), ARRAY[]::text[]) AS collectors,
           coalesce(array_agg(DISTINCT cs.state), ARRAY[]::text[])  AS collector_states,
           sum(cs.spool_depth)::bigint                               AS spool_depth,
           sum(cs.spool_dropped_total)::bigint                       AS spool_dropped_total,
           max(cs.last_report_at)                                    AS last_report_at
      FROM ops.collector_state cs
     WHERE cs.tenant_id = d.tenant_id AND cs.device_id = d.device_id) c ON true;

-- Usage with present-tense sanction state: the decision on the tool the fingerprint belongs to.
-- LEFT JOINs, so a fingerprint outside the catalogue, or a tool with no decision, still appears with
-- a NULL state, which the read layer renders as `unknown`.
CREATE VIEW mart.v_tool_usage
WITH (security_invoker = true) AS
SELECT a.tenant_id,
       a.bucket_start,
       a.bucket_size,
       a.tool_fingerprint,
       coalesce(c.display_name, a.tool_fingerprint) AS display_name,
       s.sanctioned_state,
       a.submissions,
       a.users,
       a.bytes_total,
       a.blocked,
       a.warned,
       a.logged
  FROM mart.agg_tool_period a
  LEFT JOIN ref.tool_catalogue c
    ON c.tool_fingerprint = a.tool_fingerprint
  LEFT JOIN ops.tool_sanction s
    ON s.tenant_id = a.tenant_id
   AND s.tool_key = c.app_key;

-- A fingerprint's display name: the shared catalogue's, else "Unrecognised tool". The raw
-- fingerprint is never shown as a name, because it would read like a known tool with a bad label;
-- it travels separately on every row.
CREATE FUNCTION ops.tool_display_name(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce(
    (SELECT c.display_name
       FROM ref.tool_catalogue c
      WHERE c.tool_fingerprint = p_tool_fingerprint),
    'Unrecognised tool')
$$;

-- Findings with the current rule definition and review state. A missing review renders as `open`:
-- nobody has reviewed it.
CREATE VIEW mart.v_finding
WITH (security_invoker = true) AS
SELECT f.tenant_id,
       f.submission_id,
       f.rule_id,
       r.title        AS rule_title,
       r.class_code,
       r.severity,
       f.detected_at,
       f.decided_locally,
       f.collection_mode,
       s.user_ref,
       s.tool_fingerprint,
       s.policy_action,
       coalesce(fr.review_state, 'open') AS review_state,
       fr.reviewed_by,
       fr.reviewed_at
  FROM mart.finding f
  JOIN ref.rule r ON r.rule_id = f.rule_id
  JOIN ingest.submission s
    ON s.tenant_id = f.tenant_id AND s.submission_id = f.submission_id
  LEFT JOIN ops.finding_review fr
    ON fr.tenant_id = f.tenant_id
   AND fr.submission_id = f.submission_id
   AND fr.rule_id = f.rule_id;

-- What an operator needs to see before closing a tenant's gate: how many devices are enrolled and
-- reporting, and how many events sit in device spools that a closed ingest gate would strand. Spool
-- depth is the per-device maximum across collectors, because collectors share one spool. Scoped to
-- one tenant by row-level security, so it informs one decision at a time.
CREATE VIEW mart.v_tenant_suspension_impact
WITH (security_invoker = true) AS
SELECT t.tenant_id,
       t.name,
       t.status,
       t.ingest_enabled,
       t.read_enabled,
       (SELECT count(*) FROM ops.device d
         WHERE d.tenant_id = t.tenant_id AND d.revoked_at IS NULL)        AS devices_enrolled,
       (SELECT count(*) FROM ops.device d
         WHERE d.tenant_id = t.tenant_id
           AND d.last_seen_at > now() - interval '24 hours')              AS devices_reporting,
       (SELECT coalesce(sum(p.depth), 0) FROM (
          SELECT max(cs.spool_depth) AS depth
            FROM ops.collector_state cs
           WHERE cs.tenant_id = t.tenant_id
           GROUP BY cs.device_id) p)                                      AS events_spooled_on_devices,
       (SELECT coalesce(sum(u.dropped), 0) FROM (
          SELECT max(cs.spool_dropped_total) AS dropped
            FROM ops.collector_state cs
           WHERE cs.tenant_id = t.tenant_id
           GROUP BY cs.device_id) u)                                      AS events_already_dropped,
       (SELECT max(d.last_seen_at) FROM ops.device d
         WHERE d.tenant_id = t.tenant_id)                                 AS last_device_seen_at
  FROM ops.tenant t;


-- =====================================================================================
-- 8. Enforcement functions and triggers
-- =====================================================================================

-- A policy bundle may not exceed the tenant's ceiling. Enforced here, so the configuration API is
-- not the only thing between a tenant and a mode it may not use.
CREATE FUNCTION ops.enforce_policy_ceiling() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_ceiling int;
  v_worst   int;
  v_unknown int;
BEGIN
  SELECT ops.mode_rank(t.ceiling_mode) INTO v_ceiling
    FROM ops.tenant t WHERE t.tenant_id = NEW.tenant_id;

  IF v_ceiling IS NULL THEN
    RAISE EXCEPTION 'tenant % has no ceiling mode', NEW.tenant_id;
  END IF;

  -- The scope matrix's leaves are mode strings, at any depth. An unrecognised mode is an error
  -- rather than a skip, so a mode the server cannot read never becomes an unrestricted one.
  SELECT count(*) FILTER (WHERE ops.mode_rank(q.value #>> '{}') IS NULL),
         max(ops.mode_rank(q.value #>> '{}'))
    INTO v_unknown, v_worst
    FROM jsonb_path_query(NEW.scope_matrix, '$.** ? (@.type() == "string")') AS q(value);

  IF v_unknown > 0 THEN
    RAISE EXCEPTION 'policy bundle % for tenant % contains an unrecognised collection mode',
      NEW.bundle_version, NEW.tenant_id;
  END IF;

  -- A bundle that specifies no scope is malformed, not unrestricted.
  IF v_worst IS NULL THEN
    RAISE EXCEPTION 'policy bundle % for tenant % specifies no collection scope',
      NEW.bundle_version, NEW.tenant_id;
  END IF;

  IF v_worst > v_ceiling THEN
    RAISE EXCEPTION 'policy bundle % for tenant % exceeds ceiling: worst scope mode rank % > ceiling rank %',
      NEW.bundle_version, NEW.tenant_id, v_worst, v_ceiling;
  END IF;

  RETURN NEW;
END $$;

CREATE TRIGGER policy_bundle_ceiling
  BEFORE INSERT OR UPDATE ON ops.policy_bundle
  FOR EACH ROW EXECUTE FUNCTION ops.enforce_policy_ceiling();

-- A scope override must name a non-empty tool and a valid mode no wider than the requested mode
-- (which is itself no wider than the ceiling), so composition can never widen collection. The
-- bundle ceiling trigger above is the backstop for the composed matrix; this refuses the input
-- earlier, so the Settings page gets a named error rather than a bundle-mint failure.
CREATE FUNCTION ops.enforce_scope_overrides() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_requested int := ops.mode_rank(coalesce(NEW.collection_mode, NEW.ceiling_mode));
  v_key  text;
  v_mode text;
  v_rank int;
BEGIN
  FOR v_key, v_mode IN
    SELECT e.key, e.value
      FROM jsonb_each_text(NEW.scope_overrides) AS e(key, value)
  LOOP
    v_rank := ops.mode_rank(v_mode);
    IF v_rank IS NULL THEN
      RAISE EXCEPTION 'scope override % has an unrecognised collection mode', v_key;
    END IF;
    IF v_rank > v_requested THEN
      RAISE EXCEPTION 'scope override % (mode %) is wider than the tenant''s requested mode (%)',
        v_key, v_mode, coalesce(NEW.collection_mode, NEW.ceiling_mode);
    END IF;
  END LOOP;
  RETURN NEW;
END $$;

CREATE TRIGGER scope_overrides_within_requested
  BEFORE INSERT OR UPDATE OF collection_mode, ceiling_mode, scope_overrides ON ops.tenant
  FOR EACH ROW EXECUTE FUNCTION ops.enforce_scope_overrides();

-- An enforcement rule's labels are classifier classes: a label outside ref.data_class could never
-- match, so the rule is refused rather than stored silently inert.
CREATE FUNCTION ops.enforce_rule_labels() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_label text;
BEGIN
  -- A labels value that is not an array is left to the match shape CHECK.
  IF jsonb_typeof(NEW.match->'labels') IS DISTINCT FROM 'array' THEN
    RETURN NEW;
  END IF;
  SELECT l INTO v_label
    FROM jsonb_array_elements_text(NEW.match->'labels') AS l
   WHERE NOT EXISTS (SELECT 1 FROM ref.data_class c WHERE c.class_code = l)
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'enforcement rule % names label % outside ref.data_class', NEW.rule_id, v_label
      USING ERRCODE = 'check_violation', CONSTRAINT = 'enforcement_rule_labels_known';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER enforcement_rule_labels_known
  BEFORE INSERT OR UPDATE OF match ON ops.enforcement_rule
  FOR EACH ROW EXECUTE FUNCTION ops.enforce_rule_labels();

-- The audit hash chain: each row hashes its predecessor in the same tenant. The per-tenant advisory
-- lock stops two concurrent writers from reading the same predecessor and forking the chain.
CREATE FUNCTION ops.audit_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_prev text;
  v_body text;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text, 0));

  SELECT a.row_hash INTO v_prev
    FROM ops.audit a
   WHERE a.tenant_id = NEW.tenant_id
   ORDER BY a.audit_seq DESC
   LIMIT 1;

  NEW.prev_hash := v_prev;

  v_body := concat_ws(E'\x1f',
    NEW.tenant_id::text,
    NEW.audit_seq::text,
    to_char(NEW.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'),
    NEW.actor_type,
    NEW.actor_id,
    NEW.action,
    NEW.object_type,
    coalesce(NEW.object_id, ''),
    coalesce(NEW.subject_ref, ''),
    coalesce(NEW.case_reference, ''),
    NEW.detail::text,
    coalesce(v_prev, ''));

  NEW.row_hash := encode(sha256(convert_to(v_body, 'UTF8')), 'hex');
  RETURN NEW;
END $$;

CREATE TRIGGER audit_chain_before_insert
  BEFORE INSERT ON ops.audit
  FOR EACH ROW EXECUTE FUNCTION ops.audit_chain();

-- The chain trigger runs with the inserting role's rights and reads the previous row's hash, so
-- every role that inserts audit rows reads exactly those columns. Row-level security keeps the read
-- to the inserting session's tenant.
GRANT SELECT (tenant_id, audit_seq, row_hash) ON ops.audit TO sac_ingest, sac_control, sac_vault, sac_ops;

-- UPDATE and DELETE are refused for every role, including the owner.
CREATE FUNCTION ops.block_audit_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ops.audit is append-only; % is not permitted', TG_OP;
  RETURN NULL;
END $$;

CREATE TRIGGER audit_append_only
  BEFORE UPDATE OR DELETE ON ops.audit
  FOR EACH STATEMENT EXECUTE FUNCTION ops.block_audit_mutation();

-- Observations are never edited. They are deleted only by the retention and erasure paths, which
-- declare themselves by setting sac.retention_delete = on, so a removal is always attributable.
CREATE FUNCTION ingest.block_observation_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'ingest.observation is immutable; in-place update is not permitted';
  END IF;

  IF coalesce(current_setting('sac.retention_delete', true), 'off') <> 'on' THEN
    RAISE EXCEPTION 'ingest.observation may be deleted only by the retention path (set sac.retention_delete = on)';
  END IF;

  RETURN OLD;
END $$;

CREATE TRIGGER observation_append_only
  BEFORE UPDATE OR DELETE ON ingest.observation
  FOR EACH ROW EXECUTE FUNCTION ingest.block_observation_mutation();

-- A redeemed retrieval grant cannot be updated again. The correct claim statement never reaches a
-- redeemed row (it matches used_at IS NULL); this refuses one that has forgotten the guard. A CHECK
-- cannot do it, because it sees only the new row.
CREATE FUNCTION ops.block_grant_reuse() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.used_at IS NOT NULL THEN
    RAISE EXCEPTION 'ops.retrieval_grant is single-use; grant % was already redeemed at % by %',
      OLD.grant_id, OLD.used_at, OLD.used_by;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER retrieval_grant_single_use
  BEFORE UPDATE ON ops.retrieval_grant
  FOR EACH ROW EXECUTE FUNCTION ops.block_grant_reuse();

-- Retention in days for an event: the tenant's policy for the highest-severity class in its labels
-- and its collection mode, else the longest event policy for the mode, else the 'standard' class
-- default, else 90 days. M0 rows carry no labels and fall through to the mode default.
CREATE FUNCTION ops.event_ttl_days(p_tenant uuid, p_mode text, p_labels jsonb)
RETURNS int
LANGUAGE plpgsql STABLE AS $$
DECLARE
  v_class text;
  v_ttl   int;
BEGIN
  IF p_labels IS NOT NULL
     AND jsonb_typeof(p_labels) = 'array'
     AND jsonb_array_length(p_labels) > 0 THEN
    SELECT l.value->>'class' INTO v_class
      FROM jsonb_array_elements(p_labels) AS l(value)
      JOIN ref.data_class d ON d.class_code = l.value->>'class'
     ORDER BY CASE d.default_severity
                WHEN 'critical' THEN 0
                WHEN 'high'     THEN 1
                WHEN 'medium'   THEN 2
                ELSE 3
              END,
              coalesce((l.value->>'score')::numeric, 0) DESC
     LIMIT 1;
  END IF;

  IF v_class IS NOT NULL THEN
    SELECT rp.ttl_days INTO v_ttl
      FROM ops.retention_policy rp
     WHERE rp.tenant_id = p_tenant
       AND rp.applies_to = 'event'
       AND rp.data_class = v_class
       AND rp.collection_mode = p_mode;
  END IF;

  IF v_ttl IS NULL THEN
    SELECT coalesce(
             (SELECT rp.ttl_days
                FROM ops.retention_policy rp
               WHERE rp.tenant_id = p_tenant
                 AND rp.applies_to = 'event'
                 AND rp.collection_mode = p_mode
               ORDER BY rp.ttl_days DESC
               LIMIT 1),
             (SELECT rc.default_ttl_days
                FROM ref.retention_class rc
               WHERE rc.retention_class = 'standard'),
             90)
      INTO v_ttl;
  END IF;

  RETURN v_ttl;
END $$;

-- The single write path for events. It records the observation idempotently, folds it into the
-- logical submission with the fidelity tie-break, and returns the per-event outcome ('inserted',
-- 'merged' or 'duplicate') with the submission id. It takes the stored envelope as jsonb, so it
-- follows the envelope contract rather than a parameter list. Invoker rights: row-level security
-- applies to it like any other statement.
CREATE FUNCTION ingest.record_event(p_envelope jsonb, p_received_at timestamptz)
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
  v_captured      boolean;
  v_exact         text;
  v_weak          text;
  v_subject_name  text;
  v_user_ref      text;
BEGIN
  SELECT f.fidelity_rank INTO v_fidelity FROM ref.route_fidelity f WHERE f.source = v_source;
  IF v_fidelity IS NULL THEN
    RAISE EXCEPTION 'unknown route %', v_source;
  END IF;

  -- Retention is resolved once and materialised onto both rows.
  v_ttl := ops.event_ttl_days(v_tenant, v_mode, p_envelope->'labels');

  -- The dedup tier is derived, never sent: a prompt with a content digest was read and has an exact
  -- key; anything else is filed under the server-derived weak key.
  v_is_exact := (v_kind = 'prompt' AND p_envelope->>'content_digest' IS NOT NULL);
  -- At M3 the device captures the prompt's content and holds it until an upload is granted, so the
  -- submission's content is local_only from the start. content-vault marks it uploaded; the expire
  -- and erasure paths mark it shredded.
  v_captured := (v_kind = 'prompt' AND v_mode = 'm3');
  v_exact    := p_envelope->>'dedup_key';
  v_weak     := ingest.weak_dedup_key(v_tenant, (p_envelope->>'device_id')::uuid,
                                      p_envelope->>'tool_fingerprint', v_kind, v_occurred,
                                      (p_envelope->>'size_bytes')::bigint);

  -- The clear account name is stored only for a tenant whose device_identity is 'clear'; a device
  -- that still sends one to a 'hashed' tenant has it dropped here.
  SELECT CASE WHEN t.device_identity = 'clear' THEN nullif(p_envelope->>'subject_name', '') END
    INTO v_subject_name
    FROM ops.tenant t WHERE t.tenant_id = v_tenant;

  -- Store the person under their canonical ref. A ref with no alias (no SCIM, or not yet
  -- provisioned) is stored as sent. The dedup keys never include the person, so resolution cannot
  -- change what merges with what.
  v_user_ref := coalesce(
    (SELECT a.user_ref FROM ops.user_ref_alias a
      WHERE a.tenant_id = v_tenant AND a.alias_ref = p_envelope->>'user_ref'),
    p_envelope->>'user_ref');

  -- Step 1: record the observation. The primary key is (tenant_id, event_id), so a replayed batch
  -- inserts nothing and reports duplicates.
  INSERT INTO ingest.observation (
    tenant_id, event_id, device_id, user_ref, subject_name, tool_fingerprint, direction, kind,
    occurred_at, received_at, monotonic_offset_ms, source, confidence, collection_mode,
    size_bytes, content_digest, labels, classifier_version, content_excerpt, policy_decision,
    window_start, window_end, submission_count, bytes_total, detection_basis,
    prompt_kind, dedup_key, schema_version, expires_at,
    discovery_type, app_version, publisher, host_app, destination_host, model_names,
    activity_type, model, input_tokens, output_tokens, duration_ms, tool_name, outcome
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
    p_received_at + make_interval(days => v_ttl),
    p_envelope->>'discovery_type',
    p_envelope->>'app_version',
    p_envelope->>'publisher',
    p_envelope->>'host_app',
    p_envelope->>'destination_host',
    CASE WHEN jsonb_typeof(p_envelope->'model_names') = 'array'
         THEN ARRAY(SELECT jsonb_array_elements_text(p_envelope->'model_names')) END,
    p_envelope->>'activity_type',
    p_envelope->>'model',
    (p_envelope->>'input_tokens')::bigint,
    (p_envelope->>'output_tokens')::bigint,
    (p_envelope->>'duration_ms')::bigint,
    p_envelope->>'tool_name',
    p_envelope->>'outcome'
  )
  ON CONFLICT (tenant_id, event_id) DO NOTHING;

  GET DIAGNOSTICS v_rowcount = ROW_COUNT;

  IF v_rowcount = 0 THEN
    -- Already recorded: report the duplicate against its submission, preferring an exact match.
    SELECT s.submission_id INTO v_sub_id
      FROM ingest.submission s
     WHERE s.tenant_id = v_tenant
       AND ((v_is_exact AND s.dedup_key = v_exact) OR s.dedup_weak_key = v_weak)
     ORDER BY (s.dedup_key IS NULL)
     LIMIT 1;
    RETURN QUERY SELECT 'duplicate'::text, v_sub_id;
    RETURN;
  END IF;

  -- The device's most recent user. Guarded like last_seen_at, so an out-of-order batch cannot move
  -- it backwards.
  UPDATE ops.device
     SET last_user_ref     = v_user_ref,
         last_subject_name = v_subject_name
   WHERE tenant_id = v_tenant
     AND device_id = (p_envelope->>'device_id')::uuid
     AND (last_seen_at IS NULL OR last_seen_at <= p_received_at);

  -- Step 2: fold the observation into the logical submission:
  --   1. an exact match on this observation's key: merge into it;
  --   2. otherwise a weak-only row with the same weak key: adopt it, because this observation
  --      supplies the exact key that row lacks;
  --   3. otherwise a new submission.
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
    -- Lower fidelity rank wins. A better route replaces the content-bearing fields wholesale; an
    -- equal or worse route only adds itself to observed_routes, so the row never blends two
    -- observations. The exception is adoption: a weak-only row takes the exact key and the digest it
    -- was derived from together (submission_exact_key_implies_digest), whatever the rank, and is then
    -- a high-confidence merge.
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
           -- An observation that did not decide a kind does not erase one an earlier route supplied.
           prompt_kind        = CASE WHEN v_fidelity < s.winning_fidelity
                                     THEN nullif(p_envelope->>'prompt_kind', '')
                                     ELSE coalesce(s.prompt_kind, nullif(p_envelope->>'prompt_kind', '')) END,
           size_bytes         = CASE WHEN v_fidelity < s.winning_fidelity THEN (p_envelope->>'size_bytes')::bigint ELSE s.size_bytes END,
           content_digest     = CASE WHEN v_fidelity < s.winning_fidelity
                                     THEN p_envelope->>'content_digest'
                                     ELSE coalesce(s.content_digest,
                                                   CASE WHEN v_is_exact THEN p_envelope->>'content_digest' END) END,
           labels             = CASE WHEN v_fidelity < s.winning_fidelity THEN p_envelope->'labels' ELSE s.labels END,
           -- A route that could not attribute a person does not erase a name an earlier route
           -- supplied. Both observations are of one submission, so of one person.
           subject_name       = CASE WHEN v_fidelity < s.winning_fidelity
                                     THEN v_subject_name
                                     ELSE coalesce(s.subject_name, v_subject_name) END,
           classifier_version = CASE WHEN v_fidelity < s.winning_fidelity THEN p_envelope->>'classifier_version' ELSE s.classifier_version END,
           confidence         = CASE WHEN v_fidelity < s.winning_fidelity THEN p_envelope->>'confidence' ELSE s.confidence END,
           merge_confidence   = CASE WHEN s.dedup_key IS NULL AND NOT v_is_exact THEN 'low'
                                     WHEN s.dedup_key IS NULL AND v_is_exact THEN 'high'
                                     ELSE s.merge_confidence END,
           -- Content is only ever captured, never un-captured: an M3 prompt moves a submission from
           -- not_captured to local_only, and nothing here moves it back.
           content_state      = CASE WHEN s.content_state = 'not_captured' AND v_captured THEN 'local_only'
                                     ELSE s.content_state END,
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
    merge_confidence, content_state, expires_at
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
    CASE WHEN v_captured THEN 'local_only' ELSE 'not_captured' END,
    p_received_at + make_interval(days => v_ttl)
  )
  RETURNING ingest.submission.submission_id INTO v_sub_id;

  RETURN QUERY SELECT 'inserted'::text, v_sub_id;
END $$;


-- =====================================================================================
-- 9. Row-level security
-- =====================================================================================
-- Every tenant-scoped table is ENABLED and FORCED; FORCE subjects the table owner to the policy
-- too. The policies compare with ops.current_tenant(), which is NULL when the session has not set a
-- tenant, and a comparison with NULL excludes the row. The list is explicit so a reviewer can see
-- exactly what is covered.

DO $$
DECLARE
  t text;
  tenant_tables text[] := ARRAY[
    -- ops
    'ops.user_dim', 'ops.device', 'ops.device_credential', 'ops.collector_state',
    'ops.policy_bundle', 'ops.tool_sanction', 'ops.notice_acknowledgement', 'ops.retention_policy',
    'ops.endpoint_setting', 'ops.endpoint_tool_setting', 'ops.enforcement_rule', 'ops.kill_switch',
    'ops.audit', 'ops.grant', 'ops.content', 'ops.retrieval_grant', 'ops.finding_review',
    'ops.erasure_receipt', 'ops.export', 'ops.erasure_request',
    'ops.aggregate_watermark', 'ops.coverage_snapshot',
    'ops.subscription', 'ops.usage_daily',
    'ops.identity_connection', 'ops.tenant_email_domain', 'ops.onboarding_invite',
    'ops.role_grant', 'ops.auth_session', 'ops.scim_token', 'ops.scim_user', 'ops.scim_group',
    'ops.scim_group_member', 'ops.user_ref_alias', 'ops.deployment_key',
    -- ingest
    'ingest.observation', 'ingest.submission', 'ingest.rejected', 'ingest.search_text',
    -- mart
    'mart.finding', 'mart.agg_tool_period', 'mart.agg_tool_user_period',
    'mart.agg_class_period', 'mart.agg_org_period', 'mart.agg_user_period',
    'mart.agg_device_period'
  ];
BEGIN
  FOREACH t IN ARRAY tenant_tables LOOP
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON %s
         USING (tenant_id = ops.current_tenant())
         WITH CHECK (tenant_id = ops.current_tenant())', t);
  END LOOP;
END $$;

-- ops.tenant is the tenant table itself: a session sees exactly its own row.
ALTER TABLE ops.tenant ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.tenant FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ops.tenant
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

-- The cross-tenant lookups (section 5d) read as sac_resolver. These SELECT-only policies are its
-- whole reach.
CREATE POLICY pre_tenant_lookup ON ops.identity_connection FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.tenant_email_domain FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.onboarding_invite   FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.auth_session        FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.scim_token          FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.tenant              FOR SELECT TO sac_resolver USING (true);

-- ops.auth_signin has no tenant. It is forced with a policy for the one role that drives sign-in,
-- so a grant added to any other role by mistake still reads nothing.
ALTER TABLE ops.auth_signin ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.auth_signin FORCE ROW LEVEL SECURITY;
CREATE POLICY control_plane_only ON ops.auth_signin TO sac_control
  USING (true)
  WITH CHECK (true);

-- ref tables are shared, hold no tenant or personal data, and carry no policy.


-- =====================================================================================
-- 10. Grants
-- =====================================================================================
-- The rules the grants implement:
--   * ingest-api writes events and cannot read content.
--   * control-api manages configuration, enrolment, identity and grant decisions, and cannot read
--     content.
--   * content-vault is the only role that reads ops.content and the content search index.
--   * query-api reads events, aggregates and findings, and writes only audit rows, finding reviews
--     and tool sanction decisions.
--   * the scheduled jobs own aggregation and expiry.
--   * no runtime role holds UPDATE or DELETE on ops.audit, or UPDATE on ingest.observation.

-- ingest-api
GRANT SELECT ON ref.data_class, ref.route_fidelity, ref.collector, ref.retention_class TO sac_ingest;
GRANT SELECT ON ops.tenant, ops.device, ops.device_credential, ops.retention_policy TO sac_ingest;
-- Accepting a batch stamps the device's activity in the same transaction. Column-level, so the
-- write path can record activity but cannot change a device's identity or revocation.
GRANT UPDATE (last_seen_at, last_user_ref, last_subject_name) ON ops.device TO sac_ingest;
-- The usage ledger is counted in the transaction that accepts the events, so the counter and the
-- data commit together.
GRANT SELECT, INSERT, UPDATE ON ops.usage_daily TO sac_ingest;
GRANT SELECT, INSERT ON ingest.observation TO sac_ingest;
GRANT SELECT, INSERT, UPDATE ON ingest.submission TO sac_ingest;
GRANT INSERT ON ingest.rejected TO sac_ingest;
GRANT INSERT ON ops.audit TO sac_ingest;
-- record_event runs with the caller's rights and resolves aliases as it writes.
GRANT SELECT ON ops.user_ref_alias TO sac_ingest;
GRANT EXECUTE ON FUNCTION ingest.record_event(jsonb, timestamptz) TO sac_ingest;
GRANT EXECUTE ON FUNCTION ops.current_tenant() TO sac_ingest;

-- control-api: enrolment, policy, health, grant decisions, identity. The submission read is
-- column-level, so it cannot become a read of labels or content.
GRANT SELECT, INSERT, UPDATE ON ops.tenant, ops.user_dim, ops.device, ops.device_credential,
      ops.collector_state, ops.policy_bundle, ops.tool_sanction, ops.notice_acknowledgement,
      ops.retention_policy, ops.grant, ops.coverage_snapshot, ops.finding_review,
      ops.subscription, ops.endpoint_setting, ops.endpoint_tool_setting
  TO sac_control;
-- The enforcement rules are replaced as a whole list: the old rows are deleted and the new inserted
-- in one transaction.
GRANT SELECT, INSERT, DELETE ON ops.enforcement_rule TO sac_control;
-- A kill switch is tripped (inserted), given a new reason (updated) and cleared (deleted).
GRANT SELECT, INSERT, UPDATE, DELETE ON ops.kill_switch TO sac_control;
-- Deciding a content grant reads the tenant's budget; a stored upload adds its bytes to the day's
-- content counter and to nothing else.
GRANT SELECT ON ops.usage_daily TO sac_control;
GRANT INSERT (tenant_id, usage_day, content_bytes_added), UPDATE (content_bytes_added)
  ON ops.usage_daily TO sac_control;
-- A content grant is decided against the device's own observation of the event and the submission
-- it belongs to. Column-level: never labels, excerpts, digests or policy decisions.
GRANT SELECT (tenant_id, event_id, device_id, kind, collection_mode, expires_at, dedup_key)
  ON ingest.observation TO sac_control;
GRANT SELECT (tenant_id, submission_id, device_id, user_ref, tool_fingerprint, collection_mode,
              content_state, expires_at, received_at, dedup_key)
  ON ingest.submission TO sac_control;
GRANT INSERT ON ops.audit TO sac_control;
GRANT SELECT ON ref.data_class, ref.route_fidelity, ref.collector, ref.retention_class, ref.rule
  TO sac_control;
-- The policy bundle's interception hosts come from the catalogue's TLS destinations, and its app
-- catalog from ref.app and ref.app_signal.
GRANT SELECT ON ref.tool_catalogue, ref.app, ref.app_signal TO sac_control;
GRANT EXECUTE ON FUNCTION ops.current_tenant(), ops.mode_rank(text) TO sac_control;
-- Credentials the audit trail may name (invites, SCIM tokens, deployment keys, connections) are
-- revoked or disabled, never deleted. DELETE is granted only where removing the row is the
-- operation: a role taken away, a domain withdrawn, a SCIM delete, and the sweep of expired
-- sessions and sign-in attempts. Aliases are never deleted.
GRANT SELECT, INSERT, UPDATE ON ops.identity_connection, ops.tenant_email_domain,
      ops.onboarding_invite, ops.role_grant, ops.auth_session, ops.auth_signin, ops.scim_token,
      ops.scim_user, ops.scim_group, ops.scim_group_member, ops.user_ref_alias, ops.deployment_key
  TO sac_control;
GRANT DELETE ON ops.tenant_email_domain, ops.role_grant, ops.auth_session, ops.auth_signin,
      ops.scim_user, ops.scim_group, ops.scim_group_member
  TO sac_control;

-- sac_resolver reads only what the cross-tenant lookups answer from.
GRANT SELECT ON ops.identity_connection, ops.tenant_email_domain, ops.onboarding_invite,
      ops.auth_session, ops.scim_token
  TO sac_resolver;
GRANT SELECT (tenant_id) ON ops.tenant TO sac_resolver;

-- Functions are executable by PUBLIC unless revoked, and these cross tenants. The privileges are
-- set while the applying role still owns the functions; a change of owner keeps them.
REVOKE EXECUTE ON FUNCTION ops.identity_connection_for_entra(text), ops.identity_connection_for_issuer(text),
      ops.identity_connection_by_id(uuid), ops.tenant_for_email_domain(text),
      ops.onboarding_invite_by_hash(text), ops.auth_session_by_hash(bytea),
      ops.tenant_for_scim_token(text), ops.tenant_ids()
  FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ops.identity_connection_for_entra(text), ops.identity_connection_for_issuer(text),
      ops.identity_connection_by_id(uuid), ops.tenant_for_email_domain(text),
      ops.onboarding_invite_by_hash(text), ops.auth_session_by_hash(bytea),
      ops.tenant_for_scim_token(text)
  TO sac_control;
GRANT EXECUTE ON FUNCTION ops.tenant_ids() TO sac_ops;

-- Hand the lookups to sac_resolver. Changing a function's owner requires the applying role to be
-- able to SET ROLE to the new owner and the new owner to have CREATE on the schema; both are held
-- only for the hand-over, so sac_resolver keeps no members and no CREATE.
GRANT sac_resolver TO CURRENT_USER;
GRANT CREATE ON SCHEMA ops TO sac_resolver;
ALTER FUNCTION ops.identity_connection_for_entra(text)  OWNER TO sac_resolver;
ALTER FUNCTION ops.identity_connection_for_issuer(text) OWNER TO sac_resolver;
ALTER FUNCTION ops.identity_connection_by_id(uuid)      OWNER TO sac_resolver;
ALTER FUNCTION ops.tenant_for_email_domain(text)        OWNER TO sac_resolver;
ALTER FUNCTION ops.onboarding_invite_by_hash(text)      OWNER TO sac_resolver;
ALTER FUNCTION ops.auth_session_by_hash(bytea)          OWNER TO sac_resolver;
ALTER FUNCTION ops.tenant_for_scim_token(text)          OWNER TO sac_resolver;
ALTER FUNCTION ops.tenant_ids()                         OWNER TO sac_resolver;
REVOKE CREATE ON SCHEMA ops FROM sac_resolver;
REVOKE sac_resolver FROM CURRENT_USER;

-- content-vault: the only role that reads stored content.
GRANT SELECT, INSERT, DELETE ON ops.content TO sac_vault;
-- Claiming an upload grant sets used_at and nothing else.
GRANT SELECT, UPDATE (used_at) ON ops.grant TO sac_vault;
-- Issue, read and redeem retrieval grants. No DELETE: a redeemed grant is the record that content
-- left the vault.
GRANT SELECT, INSERT, UPDATE ON ops.retrieval_grant TO sac_vault;
GRANT SELECT, INSERT, UPDATE, DELETE ON ingest.search_text TO sac_vault;
-- The content retention: the tenant's policy for the highest-severity label class, else the
-- 'content' class default.
GRANT SELECT ON ops.tenant, ops.retention_policy, ops.erasure_receipt TO sac_vault;
GRANT SELECT ON ref.data_class, ref.retention_class TO sac_vault;
-- A filtered content search joins the index to the person, tool, device, mode and receive time of
-- a submission; storing content reads its prompt kind and labels. Column-level, so it cannot become
-- a read of excerpts, policy decisions or dedup keys.
GRANT SELECT (tenant_id, submission_id, device_id, user_ref, tool_fingerprint,
              collection_mode, content_state, received_at, expires_at, prompt_kind, labels)
  ON ingest.submission TO sac_vault;
-- A subject export resolves content by the subject's events too, so an object whose submission was
-- not yet known at upload is still found. Column-level: never excerpts, digests or policy decisions.
GRANT SELECT (tenant_id, event_id, user_ref) ON ingest.observation TO sac_vault;
-- Storing content marks its submission uploaded, in the same transaction.
GRANT UPDATE (content_state) ON ingest.submission TO sac_vault;
GRANT INSERT ON ops.audit TO sac_vault;

-- query-api: read-mostly. No grant on ops.content or ingest.search_text: content and content
-- search are reached only through content-vault.
GRANT SELECT ON ingest.submission, ingest.observation TO sac_query;
GRANT SELECT ON mart.finding, mart.agg_tool_period, mart.agg_tool_user_period,
      mart.agg_class_period, mart.agg_org_period, mart.agg_user_period, mart.agg_device_period,
      mart.v_device_liveness, mart.v_tool_usage, mart.v_finding,
      mart.v_tenant_suspension_impact TO sac_query;
GRANT SELECT ON ops.tenant, ops.user_dim, ops.device, ops.collector_state, ops.tool_sanction,
      ops.grant, ops.erasure_receipt, ops.coverage_snapshot, ops.aggregate_watermark,
      ops.retention_policy, ops.notice_acknowledgement, ops.policy_bundle,
      ops.subscription, ops.usage_daily TO sac_query;
GRANT SELECT ON ref.data_class, ref.rule, ref.route_fidelity, ref.collector TO sac_query;
GRANT SELECT ON ref.tool_catalogue, ref.app, ref.app_signal TO sac_query;
GRANT INSERT, SELECT ON ops.audit TO sac_query;
GRANT SELECT, INSERT, UPDATE ON ops.finding_review TO sac_query;
GRANT EXECUTE ON FUNCTION ops.current_tenant(), ops.tool_display_name(text) TO sac_query;
-- Exports are this service's artifacts: it writes a generated CSV or archive and serves it once.
GRANT SELECT, INSERT ON ops.export TO sac_query;
GRANT UPDATE (used_at) ON ops.export TO sac_query;
-- Recording a subject erasure request; the erase job performs it.
GRANT SELECT, INSERT ON ops.erasure_request TO sac_query;

-- Scheduled jobs: aggregation and expiry.
GRANT SELECT, INSERT, UPDATE, DELETE ON ingest.observation, ingest.submission TO sac_ops;
GRANT SELECT, INSERT, UPDATE, DELETE ON ingest.rejected TO sac_ops;
GRANT SELECT, INSERT, UPDATE, DELETE ON mart.finding, mart.agg_tool_period,
      mart.agg_tool_user_period, mart.agg_class_period, mart.agg_org_period,
      mart.agg_user_period, mart.agg_device_period TO sac_ops;
-- Expiry deletes content and its index entries without being able to read either, and marks the
-- content's submission shredded.
GRANT SELECT (tenant_id, object_id, submission_id, event_id, expires_at), DELETE ON ops.content TO sac_ops;
GRANT SELECT (tenant_id, submission_id, unit_kind, unit_index, expires_at), DELETE
  ON ingest.search_text TO sac_ops;
GRANT SELECT, INSERT, UPDATE ON ops.erasure_receipt, ops.aggregate_watermark,
      ops.coverage_snapshot TO sac_ops;
-- The end-of-day device snapshot into the ledger. The accepted-event and content-byte counters are
-- written forward by the services and never recomputed, and no role may DELETE ledger rows.
GRANT SELECT, UPDATE ON ops.usage_daily TO sac_ops;
GRANT SELECT ON ops.subscription TO sac_ops;
GRANT SELECT ON ops.tenant, ops.user_dim, ops.device, ops.collector_state,
      ops.retention_policy TO sac_ops;
GRANT SELECT ON ref.data_class, ref.rule, ref.route_fidelity,
      ref.collector, ref.retention_class TO sac_ops;
GRANT INSERT ON ops.audit TO sac_ops;
GRANT EXECUTE ON FUNCTION ops.current_tenant() TO sac_ops;
-- The erase job sweeps expired exports (by id and expiry, never the payload) and processes the
-- subject erasure requests query-api records.
GRANT SELECT (tenant_id, export_id, expires_at), DELETE ON ops.export TO sac_ops;
GRANT SELECT, UPDATE ON ops.erasure_request TO sac_ops;


-- =====================================================================================
-- 11. Seed data
-- =====================================================================================

-- The classes the classifier produces.
INSERT INTO ref.data_class (class_code, category, description, default_severity, detector_family) VALUES
  ('payment_card',     'financial',    'Payment card numbers, validated by checksum rather than pattern alone.',        'critical', 'deterministic'),
  ('government_id',    'identity',     'National identifiers and tax numbers, per-jurisdiction rule sets.',            'critical', 'deterministic'),
  ('credential',       'secrets',      'Passwords, API keys, private key headers and tokens.',                         'critical', 'deterministic'),
  ('customer_pii',     'personal',     'Names, addresses, contact details and other customer-identifying text.',       'high',     'statistical'),
  ('source_code',      'intellectual', 'Proprietary source code and internal implementation detail.',                  'high',     'statistical'),
  ('legal_commercial', 'commercial',   'Contracts, negotiations and commercially sensitive language.',                 'medium',   'statistical'),
  ('health',           'special',      'Health information and anything suggesting a medical condition.',              'critical', 'statistical');

-- Route fidelity. Lower rank wins.
INSERT INTO ref.route_fidelity (source, fidelity_rank, yields_content, description) VALUES
  ('tool.hook',         5, true,  'The tool''s own hook, handed the prompt by the tool itself before it is sent. Exactly what the user submitted, with no reconstruction.'),
  ('ext.page_context', 10, true,  'Extension reading the user-authored payload and attachment bytes in page context, before serialisation. It sees what the user composed, not what the transport did with it.'),
  ('tool.otel',        15, true,  'The tool''s own OpenTelemetry export, received on the device. Structured by the tool; prompt text is present only when the tool is configured to export it.'),
  ('cli.shim',         20, true,  'Call-site capture from a managed shell environment. Structured arguments rather than a parsed HTTP body, so no reconstruction is involved.'),
  ('proxy.loopback',   30, true,  'Local inference broker observing plaintext HTTP on the loopback interface. Nothing is decrypted and no framing is guessed; coverage is narrow.'),
  ('ext.web_request',  40, true,  'Extension reading the request body as sent. Accurate for the wire form, which may differ from the composed form in encoding and whitespace.'),
  ('proxy.tls',        50, true,  'Egress proxy terminating TLS and reading the HTTP body. The prompt is reconstructed from a provider-specific serialisation, so encoding differences can defeat canonicalisation.'),
  ('ext.dom',          60, true,  'DOM-derived observation, used where the request is not observable. Best-effort: some browser UIs render outside normal DOM structures.'),
  ('proc.detect',      70, false, 'Process and module observation only. Establishes that a model ran; carries no content.'),
  ('inv.scan',         75, false, 'Inventory of installed applications, CLIs, IDE extensions and local models. Establishes that a tool is present; carries no content.'),
  ('net.flow',         80, false, 'Connection metadata only: the host a process connected to. Establishes that a tool was used; carries no content.');

-- Collection components. modes_supported is what each can achieve, not what it is configured to do.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('capture_extension', 'capture_extension', ARRAY['m0','m1','m2','m3'], 'Policy-installed Chromium extension: browser AI tools, read in page context or from the request.'),
  ('egress_proxy',      'capture_core',      ARRAY['m0','m1','m2','m3'], 'Local TLS-terminating proxy, for clients that honour the system proxy settings.'),
  ('loopback_broker',   'capture_core',      ARRAY['m0','m1','m2','m3'], 'Local inference broker holding the well-known loopback ports of local model runtimes.'),
  ('cli_shim',          'capture_core',      ARRAY['m0','m1','m2','m3'], 'Managed shell profile and environment for proxy and trust: coding agents and SDKs.'),
  ('desktop_proxy',     'capture_core',      ARRAY['m0','m1','m2','m3'], 'Per-user proxy auto-config (PAC) that routes the AI traffic of Windows desktop apps through the local TLS proxy.'),
  ('process_detector',  'capture_core',      ARRAY['m0'],                'Process and loaded-module observation. Detection only: establishes that a model ran, never what was said to it.'),
  ('otel_receiver',     'capture_core',      ARRAY['m0','m1','m2','m3'], 'Loopback OTLP receiver for the telemetry AI tools export about themselves, authenticated by a per-device token.'),
  ('hook_relay',        'capture_core',      ARRAY['m0','m1','m2','m3'], 'Relay for the hooks AI tools run before sending a prompt: decides allow, warn or block on the device and records the prompt.'),
  ('inventory_scanner', 'capture_core',      ARRAY['m0','m1','m2','m3'], 'Scheduled scan of the installed applications (uninstall entries and AppX/MSIX packages), matched against the app catalog. Establishes that a tool is present; reads no content.'),
  ('flow_monitor',      'capture_core',      ARRAY['m0','m1','m2','m3'], 'The operating system''s DNS answers and TCP connect events: which process connected to a catalog inference domain. Connection metadata only; reads no payload.'),
  ('classifier_host',   'classifier_host',   ARRAY['m1','m2','m3'],      'Sandboxed classification host. Not a collection path; its health is reported like a collector''s.'),
  ('user_helper',       'capture_core',      ARRAY[]::text[],            'Helper process in each signed-in user session, which shows that user the agent''s notifications. Not a collection path; it reads nothing.'),
  ('tool_config_claude_code', 'capture_core',  ARRAY[]::text[],            'Writes Claude Code''s machine-wide managed settings so it exports its telemetry to the OTLP receiver. Not a collection path; it reads nothing.'),
  ('tool_config_copilot', 'capture_core',      ARRAY[]::text[],            'Writes VS Code''s machine policies for the Copilot extension and the Copilot CLI''s machine environment variables so both export their telemetry to the OTLP receiver. Not a collection path; it reads nothing.'),
  ('tool_config_cursor', 'capture_core',       ARRAY[]::text[],            'Writes the agent''s hooks into Cursor''s enterprise hooks file so its prompts and MCP calls reach the hook relay. Not a collection path; it reads nothing.'),
  ('tool_config_codex', 'capture_core',        ARRAY[]::text[],            'Writes the agent''s prompt hook into the Codex CLI''s system requirements file so its prompts reach the hook relay. Not a collection path; it reads nothing.');

INSERT INTO ref.retention_class (retention_class, default_ttl_days, description) VALUES
  ('standard',   90,  'Default for event metadata.'),
  ('extended',   365, 'Events under investigation or a longer contractual retention.'),
  ('content',    30,  'Default for stored content at M3. Short, because stored content is the cost that grows with usage.'),
  ('quarantine', 14,  'Rejected envelopes, content-stripped: long enough to diagnose a collector defect.');

-- The rules the endpoint classifier publishes. A finding names one of these (mart.finding.rule_id is
-- a foreign key), so a detection the classifier never published cannot be recorded.
INSERT INTO ref.rule (rule_id, class_code, detector_kind, severity, title, description) VALUES
  ('PAYMENT_CARD_PAN','payment_card','deterministic','critical','Payment card number','A 12-19 digit number with a payment-network prefix whose Luhn check digit holds, not preceded by "test".'),
  ('US_SSN_STRUCTURE','government_id','deterministic','critical','US Social Security number','NNN-NN-NNNN outside the never-issued ranges and published sample numbers.'),
  ('UK_NINO','government_id','deterministic','critical','UK National Insurance number','Two prefix letters, six digits and a suffix A-D, unallocated prefixes excluded.'),
  ('PRIVATE_KEY_BLOCK','credential','deterministic','critical','Private key','A PEM or PGP private key header.'),
  ('AWS_ACCESS_KEY_ID','credential','deterministic','critical','AWS access key ID','An AKIA or ASIA access key identifier.'),
  ('CUSTOMER_PII_LABEL','customer_pii','deterministic','high','Personal data field','A personal-data field label: date of birth, passport number, home address, national insurance or social security number.'),
  ('HEALTH_CLINICAL_VOCABULARY','health','deterministic','critical','Clinical information','Clinical vocabulary: diagnosis, prescription, clinical trial, medical record, ICD-10.'),
  ('LEGAL_CLAUSE_VOCABULARY','legal_commercial','deterministic','medium','Contract language','Contract drafting vocabulary: whereas, hereinafter, witnesseth, indemnify, governing law, force majeure.'),
  ('SOURCE_DECLARATION','source_code','deterministic','high','Source code','A line declaring a function, class, package, import or C include.');

-- The app catalog. Each signal was checked against its app's source_url or another published
-- vendor or package-registry source; a signal that could not be checked is left out. The JetBrains
-- uninstall names are the leading words of an entry named '<product> <version>'. Each app's
-- endpoint fingerprint is a tool_catalogue row, so display names and sanction apply to it.
INSERT INTO ref.app (app_key, display_name, vendor, category, source_url) VALUES
  ('anthropic_api',      'Anthropic API', 'anthropic', 'inference_api',
   'https://code.claude.com/docs/en/network-config'),
  ('aws_bedrock',        'Amazon Bedrock', 'amazon', 'inference_api',
   'https://github.com/boto/botocore/blob/develop/botocore/data/bedrock-runtime/2023-09-30/endpoint-rule-set-1.json'),
  ('azure_ai_foundry',   'Azure AI Foundry', 'microsoft', 'inference_api',
   'https://github.com/MicrosoftDocs/azure-ai-docs/blob/main/articles/foundry/foundry-models/includes/concepts-endpoints-2.md'),
  ('chatgpt_desktop',    'ChatGPT Desktop', 'openai', 'chat_assistant',
   'https://github.com/Homebrew/homebrew-cask/blob/main/Casks/c/chatgpt.rb'),
  ('chatgpt_web',        'ChatGPT (web)', 'openai', 'chat_assistant',
   'https://chatgpt.com'),
  ('claude_code',        'Claude Code', 'anthropic', 'coding_agent',
   'https://code.claude.com/docs/en/setup'),
  ('claude_code_vscode', 'Claude Code for VS Code', 'anthropic', 'ide_assistant',
   'https://code.claude.com/docs/en/vs-code'),
  ('claude_desktop',     'Claude Desktop', 'anthropic', 'chat_assistant',
   'https://code.claude.com/docs/en/desktop'),
  ('codex',              'Codex CLI', 'openai', 'coding_agent',
   'https://github.com/openai/codex'),
  ('cohere_api',         'Cohere API', 'cohere', 'inference_api',
   'https://docs.cohere.com/reference/chat'),
  ('continue',           'Continue', 'continue', 'ide_assistant',
   'https://github.com/continuedev/continue'),
  ('copilot_cli',        'GitHub Copilot CLI', 'github', 'coding_agent',
   'https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/install-copilot-cli'),
  ('cursor',             'Cursor', 'cursor', 'ide',
   'https://github.com/Homebrew/homebrew-cask/blob/main/Casks/c/cursor.rb'),
  ('deepseek_api',       'DeepSeek API', 'deepseek', 'inference_api',
   'https://api-docs.deepseek.com'),
  ('gemini_cli',         'Gemini CLI', 'google', 'coding_agent',
   'https://github.com/google-gemini/gemini-cli'),
  ('gemini_web',         'Gemini (web)', 'google', 'chat_assistant',
   'https://gemini.google.com'),
  ('github_copilot',     'GitHub Copilot', 'github', 'ide_assistant',
   'https://github.com/microsoft/vscode-copilot-chat'),
  ('google_ai_api',      'Google AI API', 'google', 'inference_api',
   'https://github.com/googleapis/python-genai/blob/main/google/genai/_api_client.py'),
  ('groq_api',           'Groq API', 'groq', 'inference_api',
   'https://console.groq.com/docs/api-reference'),
  ('jetbrains',          'JetBrains IDEs', 'jetbrains', 'ide',
   'https://github.com/microsoft/winget-pkgs/tree/master/manifests/j/JetBrains'),
  ('llama_cpp',          'llama.cpp', 'llama.cpp', 'local_runtime',
   'https://github.com/ggml-org/llama.cpp'),
  ('lm_studio',          'LM Studio', 'lmstudio', 'local_runtime',
   'https://github.com/lmstudio-ai/docs'),
  ('mistral_api',        'Mistral API', 'mistral', 'inference_api',
   'https://docs.mistral.ai/api'),
  ('ollama',             'Ollama', 'ollama', 'local_runtime',
   'https://github.com/ollama/ollama/blob/main/docs/faq.mdx'),
  ('openai_api',         'OpenAI API', 'openai', 'inference_api',
   'https://github.com/openai/openai-python'),
  ('openrouter',         'OpenRouter', 'openrouter', 'inference_api',
   'https://openrouter.ai/docs/api-reference/overview'),
  ('perplexity_api',     'Perplexity API', 'perplexity', 'inference_api',
   'https://docs.perplexity.ai/api-reference/chat-completions-post'),
  ('perplexity_web',     'Perplexity (web)', 'perplexity', 'chat_assistant',
   'https://www.perplexity.ai'),
  ('together_ai',        'Together AI', 'together', 'inference_api',
   'https://docs.together.ai/reference/chat-completions-1'),
  ('vllm',               'vLLM', 'vllm', 'local_runtime',
   'https://github.com/vllm-project/vllm'),
  ('vscode',             'Visual Studio Code', 'microsoft', 'ide',
   'https://github.com/microsoft/vscode-docs/blob/main/docs/setup/portable.md'),
  ('windsurf',           'Windsurf', 'codeium', 'ide',
   'https://github.com/microsoft/winget-pkgs/tree/master/manifests/c/Codeium/Windsurf'),
  ('xai_api',            'xAI API', 'xai', 'inference_api',
   'https://docs.x.ai/docs/api-reference');

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('anthropic_api',       'any',       'inference_domain',        'api.anthropic.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.af-south-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-east-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-east-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-northeast-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-northeast-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-northeast-3.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-south-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-south-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-southeast-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-southeast-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-southeast-3.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-southeast-4.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-southeast-5.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-southeast-6.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ap-southeast-7.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ca-central-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.ca-west-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-central-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-central-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-north-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-south-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-south-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-west-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-west-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.eu-west-3.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.il-central-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.me-central-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.me-south-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.mx-central-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.sa-east-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.us-east-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.us-east-2.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.us-gov-east-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.us-gov-west-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.us-west-1.amazonaws.com'),
  ('aws_bedrock',         'any',       'inference_domain',        '.bedrock-runtime.us-west-2.amazonaws.com'),
  ('azure_ai_foundry',    'any',       'inference_domain',        '.models.ai.azure.com'),
  ('azure_ai_foundry',    'any',       'inference_domain',        '.openai.azure.com'),
  ('azure_ai_foundry',    'any',       'inference_domain',        '.services.ai.azure.com'),
  ('chatgpt_desktop',     'any',       'inference_domain',        'chatgpt.com'),
  ('chatgpt_desktop',     'macos',     'macos_bundle_id',         'com.openai.chat'),
  ('chatgpt_desktop',     'macos',     'macos_bundle_id',         'com.openai.codex'),
  ('chatgpt_web',         'any',       'inference_domain',        'chat.openai.com'),
  ('chatgpt_web',         'any',       'inference_domain',        'chatgpt.com'),
  ('claude_code',         'any',       'cli_binary',              'claude'),
  ('claude_code',         'any',       'inference_domain',        'api.anthropic.com'),
  ('claude_code',         'any',       'npm_package',             '@anthropic-ai/claude-code'),
  ('claude_code',         'windows',   'windows_exe',             'claude.exe'),
  ('claude_code_vscode',  'any',       'ide_extension_id',        'anthropic.claude-code'),
  ('claude_desktop',      'any',       'inference_domain',        'claude.ai'),
  ('claude_desktop',      'macos',     'macos_bundle_id',         'com.anthropic.claudefordesktop'),
  ('claude_desktop',      'windows',   'publisher',               'Anthropic, PBC'),
  ('claude_desktop',      'windows',   'windows_appx',            'Claude_pzs8sxrjxfjjc'),
  ('codex',               'any',       'cli_binary',              'codex'),
  ('codex',               'any',       'npm_package',             '@openai/codex'),
  ('codex',               'windows',   'windows_exe',             'codex-aarch64-pc-windows-msvc.exe'),
  ('codex',               'windows',   'windows_exe',             'codex-x86_64-pc-windows-msvc.exe'),
  ('codex',               'windows',   'windows_exe',             'codex.exe'),
  ('cohere_api',          'any',       'inference_domain',        'api.cohere.ai'),
  ('continue',            'any',       'ide_extension_id',        'continue.continue'),
  ('copilot_cli',         'any',       'cli_binary',              'copilot'),
  ('copilot_cli',         'any',       'npm_package',             '@github/copilot'),
  ('cursor',              'macos',     'macos_bundle_id',         'com.todesktop.230313mzl4w4u92'),
  ('cursor',              'windows',   'publisher',               'Anysphere'),
  ('deepseek_api',        'any',       'inference_domain',        'api.deepseek.com'),
  ('gemini_cli',          'any',       'cli_binary',              'gemini'),
  ('gemini_cli',          'any',       'npm_package',             '@google/gemini-cli'),
  ('gemini_web',          'any',       'inference_domain',        'gemini.google.com'),
  ('github_copilot',      'any',       'ide_extension_id',        'github.copilot-chat'),
  ('google_ai_api',       'any',       'inference_domain',        '.aiplatform.googleapis.com'),
  ('google_ai_api',       'any',       'inference_domain',        'generativelanguage.googleapis.com'),
  ('groq_api',            'any',       'inference_domain',        'api.groq.com'),
  ('jetbrains',           'windows',   'publisher',               'JetBrains s.r.o.'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'CLion'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'DataGrip'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'GoLand'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'IntelliJ IDEA'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'JetBrains Rider'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'PhpStorm'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'PyCharm'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'RubyMine'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'RustRover'),
  ('jetbrains',           'windows',   'windows_uninstall_name',  'WebStorm'),
  ('llama_cpp',           'any',       'listen_port',             '8080'),
  ('lm_studio',           'any',       'listen_port',             '1234'),
  ('lm_studio',           'any',       'model_store',             '~/.lmstudio/models'),
  ('mistral_api',         'any',       'inference_domain',        'api.mistral.ai'),
  ('ollama',              'any',       'listen_port',             '11434'),
  ('ollama',              'linux',     'model_store',             '/usr/share/ollama/.ollama/models'),
  ('ollama',              'macos',     'model_store',             '~/.ollama/models'),
  ('ollama',              'windows',   'model_store',             '%USERPROFILE%\.ollama\models'),
  ('ollama',              'windows',   'windows_exe',             'ollama app.exe'),
  ('ollama',              'windows',   'windows_exe',             'ollama.exe'),
  ('openai_api',          'any',       'inference_domain',        'api.openai.com'),
  ('openrouter',          'any',       'inference_domain',        'openrouter.ai'),
  ('perplexity_api',      'any',       'inference_domain',        'api.perplexity.ai'),
  ('perplexity_web',      'any',       'inference_domain',        'www.perplexity.ai'),
  ('together_ai',         'any',       'inference_domain',        'api.together.xyz'),
  ('vllm',                'any',       'listen_port',             '8000'),
  ('vscode',              'macos',     'macos_bundle_id',         'com.microsoft.VSCode'),
  ('vscode',              'windows',   'publisher',               'Microsoft Corporation'),
  ('vscode',              'windows',   'windows_exe',             'Code.exe'),
  ('windsurf',            'windows',   'publisher',               'Codeium'),
  ('xai_api',             'any',       'inference_domain',        'api.x.ai');

-- The shared tool catalogue. Every fingerprint belongs to one tool, its app_key in ref.app, which is
-- what a tenant sanctions and what a rule names; the fingerprints are what devices observe.
--   * `tls` rows: the egress proxy's fingerprint of a destination, "tls_" + the first eight bytes
--     (lowercase hex) of sha256("tls|" + lowercased host + "|" + path without leading or trailing
--     slashes). One tool may have several paths, so several rows. evidence names the host and path.
--   * `extension` rows: the browser extension's "tf2:" + base32(sha256(canonical({v: 2, host}))),
--     one per host the tool is reached at. evidence names the host.
--   * `process` rows: the process detector's "proc_" + image signature.
-- A fingerprint not listed here renders as "Unrecognised tool" and belongs to no tool.
INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key) VALUES
  ('tls_b6681b043244c43f', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/v1/messages"}', 'claude_code'),
  ('tls_5a5a41ed0bf50d9d', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/v1/messages/count_tokens"}', 'claude_code'),
  ('tls_69f6ab029df3c019', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/event_logging/v2/batch"}', 'claude_code'),
  ('tls_31299ef7601928f7', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/event_logging/batch"}', 'claude_code'),
  ('tls_cb53d2b2add3d450', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/claude_cli_profile"}', 'claude_code'),
  ('tls_f32477ff734d70d1', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/chat/completions"}', 'openai_api'),
  ('tls_4a602150609f427e', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/responses"}', 'openai_api'),
  ('tls_15ab95c6f0615e12', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/models"}', 'openai_api'),
  ('tls_8a9512eae8499418', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/completions"}', 'openai_api'),
  ('tls_11574658dafb8805', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chatgpt.com","path":"/backend-api/conversation"}', 'chatgpt_web'),
  ('tls_fd863543bed5e1fd', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chatgpt.com","path":"/backend-api/chat/completions"}', 'chatgpt_web'),
  ('tls_2af2dd0ea445e033', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chat.openai.com","path":"/backend-api/conversation"}', 'chatgpt_web'),
  ('tls_f412811be7ac6539', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/chat/completions"}', 'github_copilot'),
  ('tls_336b980c5f15d4f0', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/v1/chat/completions"}', 'github_copilot'),
  ('tls_ba5b03450233e3fc', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/models"}', 'github_copilot'),
  ('tls_3a0703d7a0dc3a68', 'GitHub Copilot', 'github', 'tls', '{"host":"copilot.microsoft.com","path":"/chat/completions"}', 'github_copilot'),
  ('tls_7659f7a5e64cd885', 'Gemini (web)', 'google', 'tls', '{"host":"gemini.google.com","path":"/"}', 'gemini_web'),
  ('tls_0320fa08a4f64851', 'Gemini API', 'google', 'tls', '{"host":"generativelanguage.googleapis.com","path":"/v1beta/models"}', 'google_ai_api'),
  ('tls_a40a04ef6e27de9f', 'Perplexity (web)', 'perplexity', 'tls', '{"host":"www.perplexity.ai","path":"/"}', 'perplexity_web'),
  ('tls_aefb9cd055abfffb', 'Perplexity API', 'perplexity', 'tls', '{"host":"api.perplexity.ai","path":"/chat/completions"}', 'perplexity_api'),
  ('tls_9230903190dcf0dc', 'Cursor', 'cursor', 'tls', '{"host":"api2.cursor.sh","path":"/"}', 'cursor'),
  ('tls_14a9086e088f0d82', 'Cursor', 'cursor', 'tls', '{"host":"api.cursor.com","path":"/"}', 'cursor'),
  ('tls_84160351478923e0', 'Mistral API', 'mistral', 'tls', '{"host":"api.mistral.ai","path":"/v1/chat/completions"}', 'mistral_api'),
  ('tls_41b7a7dd22bb3bae', 'Groq API', 'groq', 'tls', '{"host":"api.groq.com","path":"/openai/v1/chat/completions"}', 'groq_api'),
  ('tls_823b80bdeef13f38', 'OpenRouter', 'openrouter', 'tls', '{"host":"openrouter.ai","path":"/api/v1/chat/completions"}', 'openrouter'),
  ('tls_ce5fbce2b73c1868', 'DeepSeek API', 'deepseek', 'tls', '{"host":"api.deepseek.com","path":"/chat/completions"}', 'deepseek_api'),
  ('tls_d829c7e9598888f9', 'xAI API', 'xai', 'tls', '{"host":"api.x.ai","path":"/v1/chat/completions"}', 'xai_api'),
  ('tls_544b80dcc1446f8b', 'Together AI', 'together', 'tls', '{"host":"api.together.xyz","path":"/v1/chat/completions"}', 'together_ai'),
  ('tls_4fb4f5cb98d7a1c3', 'Cohere API', 'cohere', 'tls', '{"host":"api.cohere.ai","path":"/v1/chat"}', 'cohere_api'),
  ('tf2:de4s6yclxfhnumvdm3ubzgd5jveiwkzapelxdcj2qdzd72qrridq', 'ChatGPT (web)', 'openai', 'extension', '{"host":"chatgpt.com"}', 'chatgpt_web'),
  ('tf2:mespeqbrgr4vb7rqmmrubxuglidtpu3e5xpjd7dwqkm2evicbwla', 'ChatGPT (web)', 'openai', 'extension', '{"host":"chat.openai.com"}', 'chatgpt_web'),
  ('tf2:6lwajg7v6mwycbn43eszknx6p3sqfegfsk6tj2p4yqovdd32mnfq', 'Gemini (web)', 'google', 'extension', '{"host":"gemini.google.com"}', 'gemini_web'),
  ('tf2:vjlb4hsqpngtmkhd4gwtgmcorqo26q2ezy7vp4erdqysbawzdx2q', 'Perplexity (web)', 'perplexity', 'extension', '{"host":"www.perplexity.ai"}', 'perplexity_web'),
  ('proc_ollama',          'Ollama (local)',    'ollama',    'process', '{"image_signature":"ollama"}', 'ollama'),
  ('proc_lmstudio',        'LM Studio (local)', 'lmstudio',  'process', '{"image_signature":"lm studio"}', 'lm_studio'),
  ('proc_llama',           'llama.cpp (local)', 'llama.cpp', 'process', '{"image_signature":"llama.cpp"}', 'llama_cpp'),
  ('proc_vllm',            'vLLM (local)',      'vllm',      'process', '{"image_signature":"vllm"}', 'vllm');

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key)
SELECT 'app:' || app_key, display_name, vendor, 'endpoint', '{}', app_key FROM ref.app;


-- =====================================================================================
-- 12. Notes
-- =====================================================================================
--
--   * No partitioning. Tenant is the leading column of every key and index, so introducing range
--     partitioning on receive time later is a migration, not an application change.
--   * Attachment contents are not indexed for search; only attachment filenames are.
--   * The aggregate upserts live in the jobs. This file provides the primary keys they conflict on.
--   * Every audit row belongs to a tenant, so a tenant's audit export is complete and holds nothing
--     it cannot interpret. Operations that concern no tenant (migrations, infrastructure changes)
--     are recorded in the platform log.
