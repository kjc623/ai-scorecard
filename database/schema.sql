-- =====================================================================================
-- Shadow AI Capture — PostgreSQL 16 schema
-- =====================================================================================
-- Product: records what employees send to generative AI tools from managed devices.
-- Derived from: docs/00-architecture.md (the master design) and the engineering brief.
-- Every object below traces to a numbered constraint (C1-C36), environment fact (E1-E25)
-- or decision (D1-D8) in the master document. Where it does not, the comment says so.
--
-- Design positions expressed here, so a reader does not have to hold the whole doc set:
--
--   * One database, four schemas:
--       ref     shared reference data, not tenant-scoped
--       ops     tenant configuration, device lifecycle, and evidence (audit, receipts)
--       ingest  immutable observations and the deduplicated logical submission
--       mart    derived aggregates and findings -- rebuildable from ingest at any time
--
--   * NOTHING in mart holds human workflow state. review_state lives in ops.finding_review,
--     keyed by the same natural key as mart.finding, so that dropping and rebuilding mart
--     cannot destroy an analyst's judgement. This is why the two are separate schemas.
--
--   * ingest.observation and ingest.submission are two tables, not one, because brief R9
--     requires double-counting to be impossible AND every count to be explainable. Two
--     routes observing one submission are two observations and one submission. Aggregates
--     are computed from submission; investigations can see both.
--
--   * No partitioning at v1 volume (master doc D2 / ADR 0009). Brief C32 says tenant should
--     be leading in "every index and every partition"; tenant is the leading column of every
--     primary key and every index below, and there is nothing to partition -- 4.4M events per
--     year per tenant is roughly 18M rows over four years. The single deviation from a
--     literal reading of C32 is recorded in ADR 0009, with the trigger for revisiting it.
--
--   * No extensions are required. sha256() and gen_random_uuid() are built into PostgreSQL 11+
--     and 13+ respectively, so the pgcrypto and pg_partman questions in master doc Q12 do not
--     block anything in this file.
--
--   * Every tenant-scoped table has ROW LEVEL SECURITY ENABLED AND FORCED, with a policy
--     written by the loop near the end of this file. Brief C32 requires cross-tenant reads to
--     be structurally impossible rather than merely unauthorised, so the policy compares the
--     row against a session setting, and a session that has not set a tenant reads zero rows.
--     The table list is explicit; the loop only removes repetition.
-- =====================================================================================


-- =====================================================================================
-- 1. Schemas
-- =====================================================================================

CREATE SCHEMA ref;
CREATE SCHEMA ops;
CREATE SCHEMA ingest;
CREATE SCHEMA mart;

-- Two extensions, both on the Azure Database for PostgreSQL Flexible Server allow-list and both
-- required only by the content search index (ADR 0014):
--
--   pg_trgm    substring and fuzzy matching over attachment filenames
--   btree_gin  lets a GIN index lead with tenant_id, so brief C32's "tenant is the leading
--              dimension of every index" has no exception for the search indexes
--
-- Everything else in this file runs on a stock instance: sha256() and gen_random_uuid() are
-- built in. Confirm both are permitted in the target region before provisioning -- master doc Q12.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS btree_gin;

COMMENT ON SCHEMA ref    IS 'Shared reference data. Not tenant-scoped, not encrypted, readable by every runtime role.';
COMMENT ON SCHEMA ops    IS 'Tenant configuration, device lifecycle, and evidence: audit, receipts, coverage.';
COMMENT ON SCHEMA ingest IS 'Immutable observations and deduplicated logical submissions. Append-only except for whole-record retention expiry.';
COMMENT ON SCHEMA mart   IS 'Derived aggregates and findings. Droppable and rebuildable from ingest. Holds no workflow state.';


-- =====================================================================================
-- 2. Roles
-- =====================================================================================
-- One role per runtime component, so that "which component can read this" is a grant and
-- not a convention. Master doc §4.3 states the intended matrix; these grants implement it.

CREATE ROLE sac_owner     NOLOGIN;   -- owns every object; never used at runtime
CREATE ROLE sac_migrator  NOLOGIN BYPASSRLS;  -- DDL only. BYPASSRLS is deliberate and is
                                              -- the reason no runtime role is ever a member.
CREATE ROLE sac_ingest    NOLOGIN;   -- ingest-api
CREATE ROLE sac_control   NOLOGIN;   -- control-api
CREATE ROLE sac_vault     NOLOGIN;   -- content-vault, internal ingress only
CREATE ROLE sac_query     NOLOGIN;   -- query-api
CREATE ROLE sac_ops       NOLOGIN;   -- aggregator and reconciler jobs
-- Not a component: the owner of the pre-tenant lookup functions in section 5c, and nothing else. It
-- never logs in and has no members, so the cross-tenant read it holds is reachable only through
-- those function bodies.
CREATE ROLE sac_resolver  NOLOGIN;

GRANT USAGE ON SCHEMA ref    TO sac_ingest, sac_control, sac_vault, sac_query, sac_ops;
GRANT USAGE ON SCHEMA ops    TO sac_ingest, sac_control, sac_vault, sac_query, sac_ops;
GRANT USAGE ON SCHEMA ingest TO sac_ingest, sac_control, sac_vault, sac_query, sac_ops;
GRANT USAGE ON SCHEMA mart   TO sac_query, sac_ops, sac_control;


-- =====================================================================================
-- 3. Helper functions
-- =====================================================================================

-- The current tenant, taken from the session. Never from a request body: the application
-- layer sets this from the authenticated principal before any query runs. If it is unset
-- the result is NULL, and every row-level policy comparison against NULL yields NULL, which
-- filters the row out. That is the fail-closed behaviour brief C32 asks for.
CREATE FUNCTION ops.current_tenant() RETURNS uuid
LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;

COMMENT ON FUNCTION ops.current_tenant() IS
  'Tenant from the session setting app.tenant_id. NULL when unset, which makes every RLS policy comparison NULL and therefore excludes every row.';

-- Collection modes are ordered, and "the most restrictive applicable mode wins" (C1, C3)
-- is a comparison on that order. Encoding it once here means the ceiling trigger and the
-- application resolve modes the same way.
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
  'Numeric order of collection modes. NULL for an unrecognised mode, which the callers treat as invalid rather than as permissive.';


-- =====================================================================================
-- 4. ref — shared reference data
-- =====================================================================================

-- The classification catalogue. Brief §6 requires "a label set with confidences", never a
-- boolean, so a class is an identified thing with a stable code rather than a free string.
CREATE TABLE ref.data_class (
  class_code        text PRIMARY KEY,
  category          text NOT NULL,
  description       text NOT NULL,
  default_severity  text NOT NULL CHECK (default_severity IN ('low','medium','high','critical')),
  detector_family   text NOT NULL CHECK (detector_family IN ('deterministic','statistical','both')),
  created_at        timestamptz NOT NULL DEFAULT now()
);

-- Brief §6: "New rules and models must be promotable without a software release, evaluated
-- in a non-enforcing mode before they take effect, and reversible instantly." That is a
-- state machine on a release artefact, which is this table. `shadow` is the non-enforcing
-- state; `rolled_back` is reachable from `enforcing` at any time and is what the server-side
-- kill switch sets.
CREATE TABLE ref.classifier_release (
  release_version   text PRIMARY KEY,
  ruleset_version   text NOT NULL,
  model_version     text,
  artifact_digest   text NOT NULL,
  state             text NOT NULL CHECK (state IN ('shadow','enforcing','rolled_back','retired')),
  promoted_at       timestamptz,
  retired_at        timestamptz,
  notes             text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT classifier_enforcing_requires_promotion
    CHECK (state <> 'enforcing' OR promoted_at IS NOT NULL),
  -- The digest identifies the release artefact that was signed and promoted, so it must be the
  -- same shape everywhere it appears. The producer is device/classifier-host/release, which builds
  -- every digest as "sha256:" + hex.EncodeToString(sum) -- lowercase by construction -- and the
  -- seed row below plus every fixture already use that shape.
  --
  -- The loader and the store refuse the same spelling, in both directions:
  -- release.verifyDigest and model.Load compare exactly, so an uppercase digest in a manifest is
  -- refused before this constraint ever sees it, and refused here if it does arrive. They were
  -- briefly out of step -- the loader accepted the uppercase form while this column would have
  -- rejected it -- and that is exactly why the same pattern is enforced on both sides rather than
  -- only where the value is produced. Two components, each individually correct, disagreeing at
  -- the seam, is the defect class this constraint and those two comparisons close together.
  CONSTRAINT classifier_release_digest_is_sha256
    CHECK (artifact_digest ~ '^sha256:[0-9a-f]{64}$')
);

-- Rule metadata for display and for findings. Rules are published as signed data alongside a
-- release, not compiled into a binary; this table is the server-side mirror so that a finding
-- can be joined to a human-readable rule without shipping the rule body to the query layer.
CREATE TABLE ref.rule (
  rule_id           text PRIMARY KEY,
  class_code        text NOT NULL REFERENCES ref.data_class(class_code),
  detector_kind     text NOT NULL CHECK (detector_kind IN ('deterministic','statistical')),
  severity          text NOT NULL CHECK (severity IN ('low','medium','high','critical')),
  title             text NOT NULL,
  description       text NOT NULL,
  introduced_in     text REFERENCES ref.classifier_release(release_version),
  retired_in        text REFERENCES ref.classifier_release(release_version)
);

-- The seed tool catalogue: the shared, non-tenant-specific mapping from a behaviour-derived
-- `tool_fingerprint` to a human-readable name. Brief §2 and C8 make discovery behavioural, so the
-- endpoint never emits a brand: it emits a fingerprint derived from what it observed (a TLS
-- host+path hash, a process image signature, or an extension name). This table is what turns that
-- opaque value back into "Claude Code" on the read side.
--
-- It is deliberately global reference data, not a column on ops.tool: ops.tool is per-tenant
-- sanction state, and a name is not a decision. A tenant may override the name through
-- ops.tool.display_name; an operator may add a fingerprint here without touching any tenant.
-- `signal_kind` and `evidence` record how the fingerprint was derived so the catalogue can be
-- regenerated rather than guessed at, and so a reviewer can see which host/path a hash stands for.
--
-- The TLS rows are the sha256 of `tls|<lowercased host>|<path with leading/trailing slashes
-- trimmed>`, first eight bytes in lowercase hex, exactly as
-- endpoint/capture-core/proxy/tlsproxy/provider.go derives it. A fingerprint the catalogue does
-- not hold is not an error and is not inferred: the read renders it as "Unrecognised tool" with
-- the raw fingerprint beside it.
CREATE TABLE ref.tool_catalogue (
  tool_fingerprint  text PRIMARY KEY,
  display_name      text NOT NULL,
  vendor            text,
  signal_kind       text NOT NULL CHECK (signal_kind IN ('tls','process','extension','other')),
  evidence          jsonb NOT NULL DEFAULT '{}'::jsonb
);

-- The fidelity ranking that decides which of two observations of the same submission wins
-- (brief §4.1 and R9; master doc §5.3). LOWER RANK IS BETTER. The ranking is about the
-- quality of the observed content, not the coverage of the route: a route that sees fewer
-- tools but sees them perfectly ranks above one that sees more tools through a reconstruction.
CREATE TABLE ref.route_fidelity (
  source            text PRIMARY KEY,
  fidelity_rank     int NOT NULL UNIQUE,
  yields_content    boolean NOT NULL,
  description       text NOT NULL
);

-- The collection components, so that ops.collector_state cannot invent a name for a path
-- that does not exist and thereby create a coverage row nobody can interpret.
CREATE TABLE ref.collector (
  collector_code    text PRIMARY KEY,
  component         text NOT NULL CHECK (component IN ('capture_extension','capture_core','classifier_host')),
  modes_supported   text[] NOT NULL,
  description       text NOT NULL
);

-- Retention classes referenced by stored content. Brief §3.2 requires a per-tenant retention
-- policy; this table names the classes that policy selects between.
CREATE TABLE ref.retention_class (
  retention_class   text PRIMARY KEY,
  -- Zero is meaningful and is not a bug: it means "never expires by default", which is what a
  -- legal-hold class needs. Expiry is suspended by ops.hold rather than by a long TTL, because
  -- a hold has to be releasable and visible (brief C35), which a TTL cannot express.
  default_ttl_days  int NOT NULL CHECK (default_ttl_days >= 0),
  description       text NOT NULL
);


-- =====================================================================================
-- 5. ops — tenants, devices, configuration, evidence
-- =====================================================================================

-- Brief C3: "the mode in force is applied before a policy bundle is signed, so a device can
-- never exceed its tenant's ceiling." ceiling_mode is that ceiling. key_custody selects the
-- key model from master doc D6; the three values are not interchangeable and the content
-- vault branches on them.
CREATE TABLE ops.tenant (
  tenant_id                    uuid PRIMARY KEY,
  name                         text NOT NULL,
  status                       text NOT NULL CHECK (status IN ('active','suspended','offboarding','closed')),
  residency_region             text NOT NULL,
  key_custody                  text NOT NULL CHECK (key_custody IN ('vendor','customer_managed','customer_held')),
  kek_id                       text,
  ceiling_mode                 text NOT NULL CHECK (ceiling_mode IN ('m0','m1','m2','m3')),
  -- Content search is a required product capability. Brief §3.5 is explicit that customer-held
  -- keys and server-side full-text search cannot both exist -- so rather than refuse one of
  -- them globally, the choice is made per tenant and the incompatibility is enforced below.
  -- Customer requirements supersede the earlier "no content search" position: see ADR 0014,
  -- which supersedes ADR 0008.
  --
  --   disabled          structured filtering only: tool, user, date, class, rule, severity
  --   attachment_names  adds substring and fuzzy search over attachment FILENAMES. A filename
  --                     is metadata that crosses at M1; no content leaves the device for it, so
  --                     this tier is compatible with every custody mode, including customer_held
  --   full_text         adds full-text search over PROMPT TEXT with highlighted snippets.
  --                     Requires vendor-readable content, because an index over plaintext is
  --                     itself plaintext-derived. Forbidden with customer_held
  content_search               text NOT NULL DEFAULT 'disabled'
                                 CHECK (content_search IN ('disabled','attachment_names','full_text')),
  content_budget_bytes_per_day bigint NOT NULL DEFAULT 0 CHECK (content_budget_bytes_per_day >= 0),
  -- The device-identity setting (backlog/04-device-identity). 'clear' means the device sends, and
  -- the read returns, the machine's hostname and the submitting account name; 'hashed' means the
  -- device sends only the hostname hash and no account name. ON by default, at the owner's
  -- direction: this deliberately reverses the earlier "pseudonymous on the wire" position, and the
  -- reasoning, the opt-out and the ADR that supersedes docs/04 A8 and docs/06 A6 are recorded in
  -- backlog/04-device-identity/DECISIONS.md and docs/adr.
  device_identity              text NOT NULL DEFAULT 'clear'
                                 CHECK (device_identity IN ('clear','hashed')),
  directory_source             text,
  -- The tenant's user-reference key: 32 random bytes, sealed under the tenant's directory key, from
  -- which every device and the SCIM endpoint derive the same pseudonymous user_ref for one person
  -- (endpoint/protocol.DeriveUserRef). NULL until control-api first needs it. Losing it cannot be
  -- repaired by re-provisioning, because every enrolled device holds a copy and derives with it.
  user_ref_key_enc             bytea,
  -- Whether enrolment also requires the device to be one Intune manages (contract §5). 'intune' is
  -- meaningful only while the tenant has an active Entra connection to ask Graph through; control-api
  -- refuses the setting otherwise, because a cross-table rule here would also have to fire when the
  -- connection is later disabled, and refusing enrolment then is the safe direction anyway.
  device_verification          text NOT NULL DEFAULT 'none'
                                 CONSTRAINT tenant_device_verification_known
                                 CHECK (device_verification IN ('none','intune')),
  -- The two enforcement gates, deliberately separate from the commercial lifecycle in `status`.
  --
  -- The product owner's rule is that nothing is automated: a human decides. That makes the
  -- SPLIT the important part rather than the enforcement, because stopping ingest and stopping
  -- reads have wildly different consequences here. Brief §1: there is no vendor-side API, so if
  -- the system is not observing, the data does not exist anywhere -- a suspension that stops
  -- ingest destroys history that can never be re-collected, while one that stops only reads is
  -- fully reversible. Collapsing both into a single `suspended` value would bury that choice
  -- inside a status change, so the flags are the enforcement and `status` is the label.
  ingest_enabled               boolean NOT NULL DEFAULT true,
  read_enabled                 boolean NOT NULL DEFAULT true,
  status_reason                text,
  status_changed_by            text,
  status_changed_at            timestamptz,
  created_at                   timestamptz NOT NULL DEFAULT now(),
  updated_at                   timestamptz NOT NULL DEFAULT now(),
  -- A key is needed only at M3. Brief §1.1 puts the storage and lifecycle boundary between M2
  -- and M3: M1 and M2 emit derived data and need no content store at all, so requiring a key
  -- below M3 would force key management on tenants that have nothing to encrypt.
  CONSTRAINT tenant_kek_required_for_content_modes
    CHECK (ceiling_mode <> 'm3' OR kek_id IS NOT NULL),
  -- Brief §3.5's mutual exclusivity, as a database invariant rather than a policy note. A
  -- tenant that requires the vendor to be cryptographically unable to read content cannot ask
  -- the vendor to index that content. Making the pair unrepresentable means no configuration
  -- path, no migration and no operator can produce a combination that cannot work.
  CONSTRAINT tenant_full_text_search_requires_vendor_readable_content
    CHECK (content_search <> 'full_text' OR key_custody <> 'customer_held'),
  -- A search tier is a capability over data the tenant must actually be able to collect.
  -- A filename crosses at M1 and prompt text only at M3 (brief §1.1), so a tenant whose ceiling
  -- cannot reach those modes would hold a search capability over nothing.
  --
  -- Enforced here rather than at policy signing, which is where docs/06 recorded it as an
  -- unresolved assumption (A11). A check the configuration API is trusted to perform is the kind
  -- that a migration, a support script or a direct UPDATE quietly bypasses; this one cannot be.
  CONSTRAINT tenant_search_tier_requires_collection_mode
    CHECK (content_search = 'disabled'
           OR (content_search = 'attachment_names' AND ceiling_mode <> 'm0')
           OR (content_search = 'full_text' AND ceiling_mode = 'm3')),
  -- A closed tenant is closed: neither gate may be open.
  CONSTRAINT tenant_closed_implies_gates_shut
    CHECK (status <> 'closed' OR (NOT ingest_enabled AND NOT read_enabled)),
  -- Closing a gate requires a recorded actor, time and reason.
  --
  -- This is what "a human decides" has to mean in the schema. With no automation, the audit
  -- trail is the only thing between a destructive action and an unexplained one -- and one of
  -- these two gates destroys data that cannot be re-collected, so an unattributed closure is
  -- the failure mode that matters most. An open tenant needs no justification; a closed gate
  -- cannot be recorded without one.
  CONSTRAINT tenant_gate_closure_attributed
    CHECK ((ingest_enabled AND read_enabled)
           OR (status_reason IS NOT NULL
               AND status_changed_by IS NOT NULL
               AND status_changed_at IS NOT NULL))
);

COMMENT ON COLUMN ops.tenant.content_search IS
  'Ceiling for content search. The signed policy bundle narrows it per scope (tool, data class, population), so search is never enabled globally across everything and brief C5''s "must not be possible to configure an upload-everything state" keeps its meaning. attachment_names is compatible with customer-held keys; full_text is not, and the check constraint makes that combination impossible to store.';

COMMENT ON COLUMN ops.tenant.kek_id IS
  'Key Vault or Managed HSM key identifier wrapping this tenant''s object keys. NULL is permitted only while the ceiling is below M3, because there is nothing to wrap until content may be stored.';

COMMENT ON COLUMN ops.tenant.content_budget_bytes_per_day IS
  'Per-tenant ceiling on content bytes accepted per day. This is the mechanism that bounds the only unbounded cost in the system (brief §3.1, §4.4 "over budget").';

-- The organisational dimension. Brief §3.6 asks "how much is usage growing, per team" and
-- "who is using them", neither of which is observable on the endpoint, so it is synchronised
-- from the customer's directory. This is an inbound data flow the brief does not describe;
-- it is open question Q2 in the master doc.
CREATE TABLE ops.user_dim (
  tenant_id             uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  user_ref              text NOT NULL,
  directory_object_id_enc bytea,
  department            text,
  population            text,
  manager_ref           text,
  -- The directory's own display name for the person. Like the department it is a directory attribute
  -- needed by the read, not the mapping to a person (that stays directory_object_id_enc). It is
  -- written only while the tenant's device_identity is 'clear', so a tenant that opted out of clear
  -- identities stores no directory name either (ADR 0021; the read then falls back to the account
  -- name the device reports, then to user_ref).
  display_name          text,
  status                text NOT NULL DEFAULT 'unknown' CHECK (status IN ('active','inactive','unknown')),
  synced_at             timestamptz,
  PRIMARY KEY (tenant_id, user_ref)
);

COMMENT ON COLUMN ops.user_dim.directory_object_id_enc IS
  'The directory identifier, encrypted. This is the only column that maps a pseudonymous user_ref to a real person, and it exists solely so that subject export and erasure can resolve one. Absent it, the wire format is pseudonymous end to end.';

COMMENT ON COLUMN ops.user_dim.display_name IS
  'The directory''s display name for the user_ref, shown beside the account name the device reports. It is not the mapping to a real person: that is directory_object_id_enc, which is encrypted and never exported. Written only while the tenant''s device_identity is ''clear''.';

CREATE TABLE ops.device (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  device_id        uuid NOT NULL,
  -- The clear machine name, present only while the tenant's device_identity is 'clear'; when it is
  -- 'hashed' the device sends hostname_hash instead and this stays NULL. Both columns exist so the
  -- setting is a real per-tenant choice rather than a one-way door (backlog/04-device-identity).
  hostname         text,
  hostname_hash    text,
  -- The stable, privacy-preserving idempotency key for enrolment (brief C11, master doc A17).
  -- A hash rather than the raw hardware identifier, so re-enrolment can recognise a returning
  -- device without the store holding an identifier that is personal data in its own right.
  -- NULL is permitted: a device may enrol without supplying one.
  hardware_identity_hash text,
  os               text NOT NULL CHECK (os IN ('windows','macos','linux')),
  os_version       text,
  -- Reported by the device at enrolment and on each heartbeat. agent_version is the reporting build;
  -- collection_mode is the effective base mode the device resolved from the signed bundle for its
  -- own scope (see DECISIONS.md D4). Both are current-state facts, overwritten on each report.
  agent_version    text,
  mdm_id           text,
  -- The Intune managed-device id Graph confirmed at enrolment, for a tenant whose
  -- device_verification is 'intune'. Separate from mdm_id, which is whatever the device reports
  -- about itself: this one was verified against the customer's Intune, so it is the identity a
  -- re-imaged machine is recognised by (contract §5).
  intune_device_id text,
  managed_state    text NOT NULL DEFAULT 'unknown' CHECK (managed_state IN ('managed','unmanaged','unknown')),
  -- Reported by the device, because there is no MDM resolver in this build. The meaning of each value
  -- and the mapping to a managed/unmanaged/unknown vocabulary live in docs/06.
  collection_mode  text CHECK (collection_mode IN ('m0','m1','m2','m3')),
  -- The person most recently active on the device, maintained monotonically by the ingest acceptance
  -- path as last_seen_at is. last_user_ref is the pseudonymous key; last_subject_name is the clear
  -- account name from the same submission, present only while device_identity is 'clear'. Keeping
  -- these on the device row lets the Devices read return a user without a per-read subject join.
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

COMMENT ON COLUMN ops.device.hostname IS
  'The clear machine name, when the tenant''s device_identity is ''clear''. NULL when the setting is ''hashed'', in which case hostname_hash carries the device-supplied hash and no clear name is stored. The reversal of the earlier hash-only design is recorded in backlog/04-device-identity/DECISIONS.md and the accompanying ADR.';

COMMENT ON COLUMN ops.device.collection_mode IS
  'The effective base collection mode the device resolved from the signed bundle for its own scope: the device override when the bundle names this device, otherwise the tenant default. Per-tool, per-population and per-class modes can be narrower and are not reflected here; this is the blanket coverage the device applies where no narrower scope does (DECISIONS.md D4).';

COMMENT ON COLUMN ops.device.last_subject_name IS
  'The clear account name from the device''s most recent submission, present only while device_identity is ''clear''. It exists so the Devices list can name the user as-of the last submission without a subject join on every read; it is the reason the Devices read is subject-level and audited (docs/04 section 5.2).';

-- Brief C11: re-enrolment after a re-image is idempotent and returns the existing identity, so a
-- hardware identity may appear at most once per tenant. A plain UNIQUE constraint is the wrong
-- tool: PostgreSQL treats every NULL as distinct, and the column is legitimately NULL for devices
-- that do not supply an identity, so a UNIQUE constraint would enforce nothing for those rows
-- while still forbidding the duplicates that matter. The partial index enforces the uniqueness
-- only where a value is present, which is the property C11 actually states.
CREATE UNIQUE INDEX device_hardware_identity_uniq
  ON ops.device (tenant_id, hardware_identity_hash)
  WHERE hardware_identity_hash IS NOT NULL;

COMMENT ON COLUMN ops.device.hardware_identity_hash IS
  'Per-device enrolment idempotency key (brief C11, A17): a hash of the hardware identity, so re-enrolment after a re-image returns the existing device_id rather than creating a second row. Uniqueness is per tenant and is enforced by the device_hardware_identity_uniq partial index, not by a UNIQUE constraint, because the column is nullable and PostgreSQL permits unlimited NULLs in a UNIQUE constraint, which would silently weaken the guard.';

-- One Intune device is one product device: a re-imaged machine that Intune still knows returns its
-- existing device_id instead of enrolling a second time. Partial for the same reason as the
-- hardware identity above: devices enrolled without Intune verification carry NULL, and a UNIQUE
-- constraint would enforce nothing for them while reading as if it did.
CREATE UNIQUE INDEX device_intune_device_uniq
  ON ops.device (tenant_id, intune_device_id)
  WHERE intune_device_id IS NOT NULL;

-- Brief §3.6 question 7 and docs/04 §3.11: the silent-device list and the coverage block both order
-- or filter on last_seen_at, and both run tenant-scoped. Tenant-leading, so the policy predicate is
-- served by the index rather than applied after a scan (C32).
CREATE INDEX device_by_last_seen ON ops.device (tenant_id, last_seen_at);

-- Brief C11: enrolment is one-shot and mutually authenticated, re-enrolment after re-imaging
-- is idempotent and returns the existing identity. Brief §4.2: a revoked device is rejected
-- and marked accordingly. ADR 0020 §4: the credential may be an X.509 certificate or an
-- RFC 9449 DPoP key; public_key_thumbprint is the single transport binding for both, so a
-- stolen credential alone is not enough to impersonate a device.
CREATE TABLE ops.device_credential (
  tenant_id            uuid NOT NULL,
  credential_id        uuid NOT NULL,
  device_id            uuid NOT NULL,
  -- ADR 0020 §4: the credential is either an X.509 client certificate or an RFC 9449 DPoP key.
  -- The mode is a property of the credential, not of the deployment, because one gateway serves
  -- a mixed fleet. Defaults to x509 so the existing certificate path is unchanged.
  credential_type      text NOT NULL DEFAULT 'x509' CHECK (credential_type IN ('x509','dpop')),
  -- The single transport-binding value for BOTH modes (ADR 0020 §4): SHA-256 over the certificate
  -- SPKI for x509, SHA-256 over the RFC 7638 JWK thumbprint for dpop. One column, not one per
  -- mode, so the authenticator compares the same field regardless of what the wire presented and
  -- the two modes cannot drift to different binding values.
  public_key_thumbprint text NOT NULL,
  -- The registered public key of a DPoP credential, as an RFC 7517 JWK. NULL for x509, where the
  -- certificate carries the key.
  public_key_jwk       jsonb,
  issued_at            timestamptz NOT NULL DEFAULT now(),
  expires_at           timestamptz NOT NULL,
  revoked_at           timestamptz,
  revoked_reason       text,
  last_used_at         timestamptz,
  PRIMARY KEY (tenant_id, credential_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),
  CONSTRAINT credential_expiry_after_issue CHECK (expires_at > issued_at),
  -- A DPoP credential IS the key: a row that says dpop and carries no JWK could never be
  -- verified. An x509 credential need not carry one, because the certificate is the material.
  CONSTRAINT credential_dpop_requires_jwk
    CHECK (credential_type <> 'dpop' OR public_key_jwk IS NOT NULL)
);

COMMENT ON COLUMN ops.device_credential.credential_type IS
  'Which authenticator mode this credential belongs to (ADR 0020 §4): x509 for an X.509 client certificate verified against the configured trust bundle, dpop for an RFC 9449 device-held key. The mode is a property of the credential so a single gateway can serve a mixed fleet.';

COMMENT ON COLUMN ops.device_credential.public_key_jwk IS
  'The registered public key of a DPoP credential, as an RFC 7517 JWK. NULL for x509, where the certificate carries the key; the credential_dpop_requires_jwk CHECK makes a dpop row without one unrepresentable.';

COMMENT ON COLUMN ops.device_credential.public_key_thumbprint IS
  'The single transport-binding value for both credential modes (ADR 0020 §4): SHA-256 over the certificate SPKI for x509, SHA-256 over the RFC 7638 JWK thumbprint for dpop. Deliberately one column rather than one per mode, so the binding the authenticator checks cannot differ by mode or drift between two columns.';

-- RFC 9449 DPoP replay detection (ADR 0020 §4). A DPoP proof carries a jti the origin must not
-- accept twice within the proof's lifetime; this table is that one-shot memory. It is a BOUNDED
-- REPLAY WINDOW, NOT AN AUDIT LOG: a row exists only to refuse a replay, it is deleted once
-- expires_at passes, it holds no actor, device or key reference, and nothing is derived from it.
-- Absence of a row is therefore not evidence that a jti was never presented -- the window is
-- deliberately short and the store is deliberately forgetful.
CREATE TABLE ops.dpop_replay (
  tenant_id    uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  -- The RFC 9449 jti claim, tenant-scoped: the same opaque string from two tenants is not a
  -- collision. The pair is the key, so the uniqueness replay detection needs is structural.
  jti          text NOT NULL,
  seen_at      timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, jti),
  -- The window is bounded in the future by construction rather than by trust in the caller.
  CONSTRAINT dpop_replay_expiry_after_seen CHECK (expires_at > seen_at)
);

COMMENT ON TABLE ops.dpop_replay IS
  'Bounded RFC 9449 jti replay window, not an audit log. A row is inserted with ON CONFLICT DO NOTHING so a replayed jti is a no-op rather than an error, and it is deleted once expires_at passes. It carries no actor, device or key reference and nothing derives from it, so it is safe to forget and is not a record of who presented what.';

COMMENT ON COLUMN ops.dpop_replay.jti IS
  'The RFC 9449 jti claim. Opaque to the store; held only so a second presentation within the proof lifetime can be refused.';

COMMENT ON COLUMN ops.dpop_replay.expires_at IS
  'When this replay window closes. The row is swept afterwards, so the same jti may legitimately recur once this time has passed; that bounded forgetfulness is a property of the design, not a gap.';

-- The sweep is tenant-scoped because row-level security is forced here, so tenant leads the
-- index for the same reason it leads every other key (brief C32).
CREATE INDEX dpop_replay_expiry
  ON ops.dpop_replay (tenant_id, expires_at);

-- The bootstrap credential of §5.1 / ADR 0020 decision 3: a short-lived, single-use, tenant-scoped
-- enrolment token. The MDM delivers the plaintext token in the enrolment profile (A5); the server
-- stores only its SHA-256 hash, so a database read is not a credential. `used_at` closes a token
-- after one device uses it; `revoked_at` is the operator's escape hatch. `hardware_identity_hash`
-- is an optional binding: when present, only a device presenting that identity may redeem the
-- token, which stops a token leaked in one profile from enrolling a different machine.
--
-- Empty tenant_id is impossible (NOT NULL) and the primary key is tenant-leading, because row-level
-- security is forced here and the context the token bootstraps -- the tenant -- must come from the
-- token, never from the request body (docs/02 §12).
CREATE TABLE ops.enrolment_token (
  tenant_id              uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  -- SHA-256 over the plaintext token, in the one spelling the repository uses for digests.
  token_hash             text NOT NULL,
  hardware_identity_hash text,
  issued_at              timestamptz NOT NULL DEFAULT now(),
  expires_at             timestamptz NOT NULL,
  used_at                timestamptz,
  revoked_at             timestamptz,
  PRIMARY KEY (tenant_id, token_hash),
  CONSTRAINT enrolment_token_expiry_after_issue CHECK (expires_at > issued_at),
  CONSTRAINT enrolment_token_hash_is_sha256 CHECK (token_hash ~ '^sha256:[0-9a-f]{64}$')
);

COMMENT ON TABLE ops.enrolment_token IS
  'The §5.1 bootstrap credential (ADR 0020 decision 3): a short-lived, single-use, tenant-scoped enrolment token, stored only as its SHA-256 hash. The plaintext is delivered by the MDM enrolment profile (A5) and never stored. tenant_id is taken from the token and never from the request body (docs/02 §12); a token that has been used or revoked is refused rather than reused.';

COMMENT ON COLUMN ops.enrolment_token.token_hash IS
  'SHA-256 of the plaintext enrolment token, spelled sha256:<64 lowercase hex>. The hash is the stored value; the plaintext is never written, so a database compromise does not yield an enrolment capability.';

COMMENT ON COLUMN ops.enrolment_token.hardware_identity_hash IS
  'Optional binding to one device: when set, only a request carrying this hardware identity may redeem the token, so a token leaked from one enrolment profile cannot be used to enrol a different machine. NULL leaves the token unbound, which is required for a device that cannot supply an identity.';

-- Brief C23 and C25: per-collector health, version, last successful capture, permission state;
-- and no path may fail into a state that reports success. The four state values are never
-- merged (brief §3.2) -- `absent` and `degraded` are different facts, and neither is `tampered`.
--
-- This is an upsert-by-key operational table rather than an event stream: master doc D5.
-- At hourly cadence across 5,000 devices, health-as-events would be roughly 44M rows a year,
-- ten times the prompt events the product exists to collect (brief §3.1 warns about exactly
-- this). The daily rollup that does enter the analytical store is mart.agg_device_period.
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
  spool_dropped_total  bigint NOT NULL DEFAULT 0 CHECK (spool_dropped_total >= 0),
  error_code           text,
  detail               jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (tenant_id, device_id, collector),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id)
);

COMMENT ON COLUMN ops.collector_state.spool_dropped_total IS
  'Monotonic count of events dropped because the local spool was full. Brief C22 requires the counter to be reported, so that an undercount is visible to the operator. Silent data loss is the failure this column exists to prevent.';

-- docs/04 §3.11: the current-degradation read (`ops.collector_state (tenant_id, state)`) is how the
-- Devices and Degraded-collection screens find every collector reporting a state, without reading
-- every device's rows.
CREATE INDEX collector_state_by_state ON ops.collector_state (tenant_id, state);

-- The signed policy bundle. Brief C10 requires signature verification failure to retain the
-- previous bundle and never fall back to unsigned or empty; bundle_version is what makes
-- "previous" meaningful, and the device keeps the last version it verified.
CREATE TABLE ops.policy_bundle (
  tenant_id          uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  bundle_version     bigint NOT NULL,
  scope_matrix       jsonb NOT NULL,
  destination_allowlist jsonb NOT NULL DEFAULT '[]'::jsonb,
  classifier_release text NOT NULL REFERENCES ref.classifier_release(release_version),
  retention_class    text NOT NULL REFERENCES ref.retention_class(retention_class),
  spool_bounds       jsonb NOT NULL DEFAULT '{}'::jsonb,
  feature_state      jsonb NOT NULL DEFAULT '{}'::jsonb,
  signature_kid      text NOT NULL,
  signed_digest      text NOT NULL,
  -- The exact bytes GET /v1/policy serves: the Ed25519 envelope endpoint/capture-core/policy
  -- verifies ({key_id, algorithm, payload, signature}). Stored rather than re-signed per request so
  -- every device that fetches one version receives byte-identical policy, and so the ETag a device
  -- caches names those bytes. NULL on a row written before control-api served the bundle.
  signed_envelope    bytea,
  effective_from     timestamptz NOT NULL DEFAULT now(),
  created_by         text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, bundle_version),
  -- The digest names the bundle in the one spelling the repository uses for digests. It was excused
  -- from that rule only while nothing wrote this table (database/tools/check-schema.mjs); control-api
  -- now does, so the exemption expired with this change rather than outliving its reason.
  CONSTRAINT policy_bundle_digest_is_sha256
    CHECK (signed_digest ~ '^sha256:[0-9a-f]{64}$'),
  -- When the served bytes are stored, the digest is theirs. The device has no digest field to compare
  -- (it verifies the signature), so this is what keeps the ETag and the audit trail's bundle identity
  -- from describing bytes other than the ones that were served.
  CONSTRAINT policy_bundle_digest_names_envelope
    CHECK (signed_envelope IS NULL
           OR signed_digest = 'sha256:' || encode(sha256(signed_envelope), 'hex'))
);

COMMENT ON COLUMN ops.policy_bundle.scope_matrix IS
  'Collection mode per scope: per tool, per data class, per user population. Brief C1 makes the mode a per-scope setting, not a global flag. The ceiling trigger below refuses any bundle in which a scope exceeds the tenant ceiling.';

COMMENT ON COLUMN ops.policy_bundle.classifier_release IS
  'The classifier release this bundle activates. Pointing at a release in state shadow means labels are recorded but nothing is blocked, which is how a new release is evaluated in a non-enforcing mode before it takes effect (brief §6).';

-- Per-tenant sanctioned state. Brief C8: the same tool is approved by one tenant and
-- prohibited by another, and `unknown` must never be conflated with `prohibited` -- which is
-- why the default is `unknown` and why it is a first-class value rather than a null.
CREATE TABLE ops.tool (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  tool_fingerprint  text NOT NULL,
  display_name      text,
  sanctioned_state  text NOT NULL DEFAULT 'unknown' CHECK (sanctioned_state IN ('sanctioned','unsanctioned','unknown')),
  first_seen_at     timestamptz NOT NULL DEFAULT now(),
  last_seen_at      timestamptz,
  evidence          jsonb NOT NULL DEFAULT '{}'::jsonb,
  decided_by        text,
  decided_at        timestamptz,
  PRIMARY KEY (tenant_id, tool_fingerprint),
  CONSTRAINT tool_decision_attributed
    CHECK (sanctioned_state = 'unknown' OR (decided_by IS NOT NULL AND decided_at IS NOT NULL))
);

COMMENT ON TABLE ops.tool IS
  'One row per behaviour-derived tool fingerprint per tenant. Deliberately not a global catalogue: brief §2 requires discovery to classify behaviour rather than match a curated brand list, and brief C8 requires the allow/deny judgement to be per-customer state.';

-- Brief §3.2: "Notice acknowledgement | tenant + user + version". The deployment workstream
-- supplies the notice text; this records the fact, so that enrolment can require a version
-- before content-reading modes are enabled.
CREATE TABLE ops.notice_acknowledgement (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  user_ref         text NOT NULL,
  notice_version   text NOT NULL,
  acknowledged_at  timestamptz NOT NULL,
  source           text NOT NULL CHECK (source IN ('enrolment','self_service','imported')),
  recorded_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_ref, notice_version)
);

-- Brief §3.2: "Retention policy | tenant | TTL per data class and collection mode". Events and
-- content have different lifecycles (C2: M2 to M3 is a storage and lifecycle boundary), so the
-- policy is selected per kind of thing as well as per class and mode.
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

-- Brief C35: holds suspend expiry for a named scope only, with a visible scope and an expiry
-- of their own. expires_at is NOT NULL precisely so that a hold cannot become permanent by
-- omission.
CREATE TABLE ops.hold (
  tenant_id       uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  hold_id         uuid NOT NULL,
  scope           jsonb NOT NULL,
  reason          text NOT NULL,
  case_reference  text,
  created_by      text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz NOT NULL,
  released_at     timestamptz,
  released_by     text,
  PRIMARY KEY (tenant_id, hold_id),
  CONSTRAINT hold_expires_in_future CHECK (expires_at > created_at)
);

COMMENT ON COLUMN ops.hold.scope IS
  'The named scope the hold covers: tool, user, class, or a time range. Brief C35 requires the scope to be visible, so it is structured data rather than a free-text note.';

-- The append-only audit log. Brief §3.6 requires every read of subject-level data to write an
-- entry AS IT IS SERVED; brief §3.2 requires an entry for every mode change, content grant,
-- content reveal, export and configuration change; brief §4.4 requires the entry to be
-- written BEFORE content is returned.
--
-- Append-only is enforced three ways, because one is not enough for a table whose integrity is
-- the product's answer to "what has been accessed, and by whom":
--   1. No runtime role is granted UPDATE or DELETE (see the grants section).
--   2. A statement-level trigger raises on UPDATE or DELETE for every role, including the owner.
--   3. Each row carries a hash chain over its predecessor, per tenant, so silent removal of a
--      middle row is detectable even by someone with sufficient privilege to do it.
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

COMMENT ON COLUMN ops.audit.detail IS
  'Action-specific payload. For a policy action this carries one of blocked / warned / logged, which brief §3.2 lists as states that must never be merged.';

COMMENT ON COLUMN ops.audit.row_hash IS
  'sha256 over the previous row hash and this row''s content for the same tenant. Computed by the ops.audit_chain() trigger. Periodic anchoring of the chain head to write-once storage is what turns this from a self-consistency check into tamper evidence.';

-- The content grant. Brief C14: content never arrives unsolicited -- the device requests a
-- grant, the backend decides, and a denial carries a reason. The four denial reasons are the
-- ones the brief enumerates; the constraint makes a denial without a reason unrepresentable,
-- so "we refused and we do not know why" cannot be recorded.
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
  case_reference    text,
  requested_by_ref  text,
  approved_by       text,
  object_id         uuid,
  upload_expires_at timestamptz,
  PRIMARY KEY (tenant_id, grant_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),
  CONSTRAINT grant_denial_requires_reason
    CHECK ((decision = 'denied') = (denial_reason IS NOT NULL)),
  CONSTRAINT grant_upload_window_bounded
    CHECK (upload_expires_at IS NULL OR upload_expires_at > requested_at)
);

COMMENT ON TABLE ops.grant IS
  'One row per content grant request. Single-use and per-event by construction: there is no bulk grant, which is why brief C5 ("must not be possible to configure the system into an upload-everything state") holds structurally rather than by a configuration check.';

-- The redemption record for a per-event content grant: the single-use half of ops.grant.
-- ADR 0006 and docs/02 §11 bind retrieval to a recorded per-event grant, and brief C5's "must not
-- be possible to configure an upload-everything state" means a grant that could be redeemed twice
-- would be a bulk-retrieval path by repetition. So the single-use property is enforced here rather
-- than trusted to the service:
--
--   * `used_at IS NULL` in the claim's WHERE clause is the race. Two concurrent redemptions cannot
--     both match the row, so the second updates nothing and is refused.
--   * retrieval_grant_claim_is_whole makes a half-claimed row unrepresentable: a claimed grant
--     always names when it was redeemed and by whom, so "redeemed" is never a partial state.
--   * the retrieval_grant_single_use trigger refuses any UPDATE of a row that is already claimed,
--     so an implementation that forgets the WHERE clause fails loudly instead of quietly
--     overwriting the first redemption. A CHECK cannot express this -- a CHECK sees only the new
--     row and cannot tell a first claim from a second -- which is why it is a trigger.
--
-- The column list is the contract used by services/content-vault (SQLPutRetrievalGrant,
-- SQLRetrievalGrant, SQLClaimRetrievalGrant). db/tools/check-schema.mjs asserts that every column
-- those statements name still exists here, so the two cannot drift apart.
CREATE TABLE ops.retrieval_grant (
  tenant_id       uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  grant_id        uuid NOT NULL,
  event_id        uuid NOT NULL,
  object_id       uuid NOT NULL,
  submission_id   uuid NOT NULL,
  principal       text NOT NULL,
  case_reference  text NOT NULL,
  second_approver text NOT NULL,
  issued_at       timestamptz NOT NULL,
  expires_at      timestamptz NOT NULL,
  used_at         timestamptz,
  used_by         text,
  raw_digest      text NOT NULL,
  PRIMARY KEY (tenant_id, grant_id),
  -- Two-person control: the approver cannot be the requester, the same rule the control-api
  -- enforces on a sanction decision, expressed where it cannot be skipped.
  CONSTRAINT retrieval_grant_second_approver_distinct CHECK (second_approver <> principal),
  CONSTRAINT retrieval_grant_window_bounded CHECK (expires_at > issued_at),
  -- The analyst-verifiable digest of exactly the bytes the retrieval returns: content-vault copies
  -- it verbatim from ops.content_object.ciphertext_sha256, which already carries this same
  -- pattern. Constrained here so the value a grant promises and the value the vault can prove
  -- cannot drift apart in shape, and so a grant cannot carry a digest that is not one.
  CONSTRAINT retrieval_grant_raw_digest_is_sha256
    CHECK (raw_digest ~ '^sha256:[0-9a-f]{64}$'),
  -- A claimed grant carries both halves of the claim, or neither.
  CONSTRAINT retrieval_grant_claim_is_whole CHECK ((used_at IS NULL) = (used_by IS NULL))
);

COMMENT ON TABLE ops.retrieval_grant IS
  'One row per single-use content retrieval grant. A redemption is a conditional UPDATE matching used_at IS NULL, so a second redemption affects zero rows and is refused; the retrieval_grant_single_use trigger additionally refuses any update of an already-claimed row. Content is served only against a grant recorded here.';

COMMENT ON COLUMN ops.retrieval_grant.used_at IS
  'When the grant was redeemed. NULL until then, and the claim statement matches on `used_at IS NULL`, which is what makes a second redemption lose the race rather than overwrite the first.';

-- Stored content objects: ciphertext in Blob Storage, wrapped data key here. Master doc D7 --
-- this table is reachable only by the content-vault role, which is the only component holding
-- Key Vault unwrap rights and which has internal-only ingress. query-api is deliberately NOT
-- granted SELECT here, so the query layer cannot see wrapped keys even in principle.
CREATE TABLE ops.content_object (
  tenant_id             uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  object_id             uuid NOT NULL,
  submission_id         uuid,
  event_id              uuid,
  blob_path             text NOT NULL,
  ciphertext_sha256     text NOT NULL CHECK (ciphertext_sha256 ~ '^sha256:[0-9a-f]{64}$'),
  plaintext_size_bytes  bigint NOT NULL CHECK (plaintext_size_bytes >= 0),
  wrapped_dek           bytea NOT NULL,
  kek_id                text NOT NULL,
  kek_version           text NOT NULL,
  retention_class       text NOT NULL REFERENCES ref.retention_class(retention_class),
  -- The device's request kind (task 08), carried onto the object so a reindex can honour it
  -- without a grant on ingest.submission. NULL when the device did not decide, which reads as
  -- 'unknown'. A 'client_generated' object has no prompt text to find and is never indexed.
  prompt_kind           text CHECK (prompt_kind IN ('user','client_generated','unknown')),
  state                 text NOT NULL CHECK (state IN ('uploaded','shredded')),
  shredded_reason       text CHECK (shredded_reason IN ('retention_expired','erasure','hold_released','tenant_offboarded')),
  created_at            timestamptz NOT NULL DEFAULT now(),
  expires_at            timestamptz,
  shredded_at           timestamptz,
  PRIMARY KEY (tenant_id, object_id),
  CONSTRAINT content_shred_requires_reason CHECK ((state = 'shredded') = (shredded_reason IS NOT NULL)),
  CONSTRAINT content_shred_requires_timestamp CHECK ((state = 'shredded') = (shredded_at IS NOT NULL))
);

COMMENT ON COLUMN ops.content_object.wrapped_dek IS
  'The per-object data key, wrapped by the tenant key. Destroying the content means deleting this row and the blob; destroying the tenant key destroys every object of that tenant at once. That is the mechanism behind brief C15.';

COMMENT ON COLUMN ops.content_object.prompt_kind IS
  'The device''s request kind for the submission this object holds (task 08): user | client_generated | unknown. It lives here, on the vault''s own table, so a reindex honours the kind without the vault needing a grant on ingest.submission. A client_generated object is stored and retrievable but never written to ingest.search_text.';

-- Review workflow for findings. Separate from mart.finding on purpose: mart is derived and
-- rebuildable, so workflow state stored there would be destroyed by a rebuild.
CREATE TABLE ops.finding_review (
  tenant_id      uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id  uuid NOT NULL,
  rule_id        text NOT NULL REFERENCES ref.rule(rule_id),
  review_state   text NOT NULL DEFAULT 'open' CHECK (review_state IN ('open','disputed','confirmed')),
  reviewed_by    text,
  reviewed_at    timestamptz,
  note           text,
  PRIMARY KEY (tenant_id, submission_id, rule_id),
  CONSTRAINT finding_review_attributed
    CHECK (review_state = 'open' OR (reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL))
);

COMMENT ON COLUMN ops.finding_review.review_state IS
  'Brief §3.2 lists disputed and confirmed as states that must never be merged. `open` is added because a finding that nobody has looked at is a third fact, and defaulting it to either of the other two would assert something untrue.';

-- Brief C34 and §3.4: subject-based erasure must produce a verifiable record of what was
-- removed, when, and by which mechanism. "A claim of deletion without a receipt is not usable
-- by the customer." This is that receipt.
CREATE TABLE ops.erasure_receipt (
  tenant_id       uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  receipt_id      uuid NOT NULL,
  scope_kind      text NOT NULL CHECK (scope_kind IN ('subject','tenant','retention','hold_release')),
  subject_ref     text,
  requested_by    text NOT NULL,
  requested_at    timestamptz NOT NULL,
  completed_at    timestamptz,
  mechanisms      text[] NOT NULL DEFAULT '{}',
  removed_counts  jsonb NOT NULL DEFAULT '{}'::jsonb,
  remaining_counts jsonb NOT NULL DEFAULT '{}'::jsonb,
  receipt_hash    text,
  PRIMARY KEY (tenant_id, receipt_id),
  CONSTRAINT erasure_subject_requires_ref CHECK (scope_kind <> 'subject' OR subject_ref IS NOT NULL)
);

COMMENT ON COLUMN ops.erasure_receipt.remaining_counts IS
  'What deliberately survived, and why -- for example rows under an active hold (brief C35). A receipt that claims total removal while a hold preserved something is worse than no receipt.';

COMMENT ON COLUMN ops.erasure_receipt.mechanisms IS
  'Which independent deletion paths ran. Brief C34 requires at least two mechanisms for time-based expiry, reconciled against each other; the same discipline is applied to erasure, including the blob lifecycle rule that removes ciphertext independently of the database row.';

-- The reconciliation of those independent mechanisms. Brief C34: "Do not trust a single
-- deletion path." Drift is recorded rather than auto-corrected, so that a systematic fault
-- becomes visible instead of being papered over nightly.
CREATE TABLE ops.reconciliation_run (
  tenant_id       uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  run_id          uuid NOT NULL,
  started_at      timestamptz NOT NULL,
  finished_at     timestamptz,
  checks          jsonb NOT NULL DEFAULT '{}'::jsonb,
  drift_found     boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, run_id)
);

-- Dashboard freshness. Brief C27 forbids presenting a number without the ability to say how
-- current it is; this is where that comes from.
CREATE TABLE ops.aggregate_watermark (
  tenant_id            uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  aggregate_name       text NOT NULL,
  bucket_size          text NOT NULL CHECK (bucket_size IN ('hour','day')),
  last_complete_bucket timestamptz NOT NULL,
  last_run_at          timestamptz NOT NULL,
  last_run_rows        bigint,
  PRIMARY KEY (tenant_id, aggregate_name, bucket_size)
);

-- Brief R11: "Make coverage state an output of the collection path itself." One row per device
-- per collector per day, recording what was expected and what was observed. This is how the
-- 70-85% managed-coverage reality (brief §5.5) is represented rather than rounded away, and how
-- a pinned or misconfigured client becomes a named gap instead of a silent omission.
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

COMMENT ON COLUMN ops.coverage_snapshot.gap_reason IS
  'Why a collector that was expected did not report. Constrained to a closed vocabulary: "we do not know why we are blind here" must be recorded as `unknown`, not left blank, because blank and unknown look identical on a dashboard and mean different things.';

-- docs/04 §3.11: the gap list reads only the rows where a collector was expected and did not report,
-- so the partial index is the one that serves it. The primary key already serves a per-day read.
CREATE INDEX coverage_snapshot_unobserved
  ON ops.coverage_snapshot (tenant_id, snapshot_day) WHERE NOT observed;


-- =====================================================================================
-- 5b. Commercial state: what is owed, and on what basis
-- =====================================================================================
-- The product is operated by the vendor as a multi-tenant service and sold monthly. Two things
-- the runtime needs that no other table carries: the commercial arrangement, so billable usage
-- has a basis and a period; and a usage ledger that does not move when a data subject is
-- erased.

CREATE TABLE ops.subscription (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  subscription_id   uuid NOT NULL,
  plan_code         text NOT NULL,
  -- The basis the product owner chose: a flat per-tenant charge plus a per-enrolled-device
  -- charge. Held as data rather than assumed in code, because changing the basis later has to
  -- be a row change and not a release.
  billing_basis     text[] NOT NULL DEFAULT ARRAY['tenant','device']
                      CHECK (billing_basis <@ ARRAY['tenant','device','events','content_bytes']
                             AND array_length(billing_basis, 1) >= 1),
  -- Day of month the billing period starts. Constrained to 28 so no period is short-changed by
  -- February.
  period_anchor_day smallint NOT NULL DEFAULT 1 CHECK (period_anchor_day BETWEEN 1 AND 28),
  started_on        date NOT NULL,
  ended_on          date,
  -- Set once a period has been invoiced. Usage on or before this date is frozen: a month that
  -- has been billed must not change because a device was removed afterwards.
  billed_through    date,
  notes             text,
  updated_by        text,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, subscription_id),
  CONSTRAINT subscription_period_ordered CHECK (ended_on IS NULL OR ended_on >= started_on)
);

COMMENT ON TABLE ops.subscription IS
  'The commercial arrangement. Deliberately holds no price: the system produces billable usage, and pricing lives in the billing system. What it must know is the basis and the period, and what it must be able to freeze is the point through which a period has been invoiced.';

-- The usage ledger. One row per tenant per day, counts only: no subject reference and no device
-- identifier, which is exactly what lets it survive an erasure.
--
-- **This is the one table in the design that must never be recomputed from the event tables**,
-- and the reason is a genuine conflict between two requirements. Brief §3.4 requires subject
-- erasure to remove data, and docs/03-data-platform.md §7 makes that the argument for aggregates
-- that *replace* their bucket rather than increment it. But a bill must not fall when a subject
-- is erased: the tenant consumed the service. A usage figure derived from `ingest.submission`
-- would silently understate the invoice after every erasure, and nobody would notice until a
-- customer asked why their usage graph had a step in it.
--
-- So the ledger is written forward as events are accepted and never derived backwards. The
-- counters are monotonic and unattributable: `events_accepted` says how many, never which.
CREATE TABLE ops.usage_daily (
  tenant_id           uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  usage_day           date NOT NULL,
  -- Accepted, non-duplicate events, counted at the moment of acceptance by ingest-api in the
  -- same transaction that stores them. A retried batch reports duplicates and adds nothing; a
  -- subject erasure removes rows and subtracts nothing.
  events_accepted     bigint NOT NULL DEFAULT 0 CHECK (events_accepted >= 0),
  -- Content bytes accepted at M3. Recorded even though the chosen basis does not use it,
  -- because content is the only unbounded cost in the system (brief §3.1) and this is how you
  -- find out that a flat price is no longer profitable for a particular tenant.
  content_bytes_added bigint NOT NULL DEFAULT 0 CHECK (content_bytes_added >= 0),
  -- End-of-day snapshot from ops.device. Enrolled is a different fact from reporting: a device
  -- that has gone quiet is still enrolled, still covered, and still billable.
  devices_enrolled    int CHECK (devices_enrolled >= 0),
  devices_reporting   int CHECK (devices_reporting >= 0),
  snapshot_at         timestamptz,
  PRIMARY KEY (tenant_id, usage_day)
);

COMMENT ON TABLE ops.usage_daily IS
  'The billable usage ledger: one row per tenant per day, counts only. Holds no subject reference and no device identifier, so it is not personal data and a subject erasure neither touches it nor changes what it says. Never recomputed from ingest -- see the note in the table definition for why that is a requirement rather than a preference.';

COMMENT ON COLUMN ops.usage_daily.devices_enrolled IS
  'Snapshot at end of day. Recorded daily rather than read live from ops.device, because "how many devices in March" is a question about March and a device removed in April must not change the answer.';


-- =====================================================================================
-- 5c. Identity, provisioning and deployment (backlog/11-sign-in-and-roles)
-- =====================================================================================
-- control-api is the product's identity service: the relying party for every customer identity
-- provider, the keeper of the server-side session, the SCIM endpoint and the source of each
-- tenant's device package. The tables below are its state.
--
-- Two rules shape all of them:
--
--   * The product tenant is decided by OUR mapping -- an Entra `tid`, an OIDC `iss` or an email
--     domain, each unique across the whole database -- and never by a claim the customer's provider
--     chooses. A token that names a tenant is only as trustworthy as whoever can configure the
--     provider, and that is the customer.
--   * A handful of lookups happen before any tenant is known: "which tenant is this issuer?", "whose
--     session is this cookie?". Those are the only reads that cross tenants, so they are not grants
--     on the tables. They are the SECURITY DEFINER functions after the tables, each answering one
--     exact-key question, owned by a role that can do nothing else (sac_resolver). Every other read
--     and write here runs under the same forced row-level security as the rest of the file.
--
-- The global lookup keys (entra_tenant_id, issuer, domain, the token and session hashes) do not
-- lead with tenant_id, because finding the tenant is what they are for. Every per-tenant listing has
-- a tenant-leading index of its own (brief C32).
--
-- Credentials are stored as hashes (sha256:<hex>, the enrolment-token spelling) or sealed
-- (`*_enc`: AES-256-GCM under the tenant's key derived from SAC_DIRECTORY_KEY, the scheme of
-- control/control-api/internal/directory.Cipher). A database read is never a credential.

-- One identity provider connection per row. A tenant normally has one; the unique lookup keys are
-- what make a second tenant claiming the same Entra tenant or issuer unrepresentable.
CREATE TABLE ops.identity_connection (
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

-- The one-time onboarding link the vendor issues. The token carries the tenant id in clear (as an
-- enrolment token does) and only its hash is stored. Single use: used_at and used_by are set
-- together when the first admin's sign-in activates the connection.
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

-- Roles granted to one person by the product rather than by the provider's claim: the first admin
-- at onboarding, and any grant an admin makes. The effective roles are the role_map of the claim
-- united with these rows. Keyed by the provider's subject (Entra: `oid`), which, unlike an email
-- address, a provider never reassigns to someone else.
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

-- The server-side session behind the dashboard's cookie. The cookie value is never stored, only its
-- sha256, so a database read cannot be replayed as a session. roles is resolved at sign-in and is
-- never empty: a person with no role is refused, not given a session with nothing in it.
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

CREATE INDEX auth_session_by_user_ref ON ops.auth_session (tenant_id, user_ref) WHERE revoked_at IS NULL;
CREATE INDEX auth_session_expiry ON ops.auth_session (tenant_id, expires_at);

-- A sign-in attempt between /auth/begin and /auth/complete: the PKCE verifier, state and nonce
-- control-api generated, for ten minutes. It is the one table here with no tenant, because an
-- attempt that starts with "Sign in with Microsoft" does not know its tenant until the provider
-- answers. Row-level security still applies: only sac_control has a policy, so no other role can
-- see an attempt even if a grant were added by mistake.
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

-- The bearer an identity provider's SCIM client presents. Same spelling as an enrolment token
-- (sacscim_ prefix, tenant id in clear, 256-bit tail), stored as its hash; revoked, never deleted,
-- so the audit trail's token ids keep resolving.
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

-- A person as the identity provider last provisioned them. The resource itself is sealed: it holds
-- names and addresses, and GET must return what was sent. Filters (`userName eq "…"`) are answered
-- through keyed hashes, so a lookup never needs the plaintext and the table holds no clear identifier.
CREATE TABLE ops.scim_user (
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

CREATE INDEX scim_user_by_external_id ON ops.scim_user (tenant_id, external_id_hash)
  WHERE external_id_hash IS NOT NULL;

-- Groups as provisioned. A group name is organisational, not personal, so it is stored in clear.
CREATE TABLE ops.scim_group (
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
-- (a UPN, an Entra object id, an account name), and a UPN changes on a rename, so one person reaches
-- the server under several refs. ingest.record_event resolves through this table at write time, so
-- every stored event already carries the canonical ref and no read has to join it. Rows are kept
-- after the person is deprovisioned: an alias is what stops a later event from starting a second
-- history for the same person.
CREATE TABLE ops.user_ref_alias (
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  alias_ref   text NOT NULL CONSTRAINT user_ref_alias_is_derived CHECK (alias_ref ~ '^u_[0-9a-f]{32}$'),
  user_ref    text NOT NULL CONSTRAINT user_ref_alias_target_is_derived CHECK (user_ref ~ '^u_[0-9a-f]{32}$'),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, alias_ref)
);

-- Subject export and erasure start from the canonical ref and need every alias of it.
CREATE INDEX user_ref_alias_by_canonical ON ops.user_ref_alias (tenant_id, user_ref);

-- A per-tenant key a device package carries (sacdk_ prefix, tenant id in clear), replacing the
-- single-use enrolment token for fleet deployment: one key enrols many devices, so it is counted,
-- revocable and optionally time-bounded rather than consumed. A new key is minted per package
-- download, so revoking one leaked package leaves the others working.
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

COMMENT ON TABLE ops.deployment_key IS
  'A per-tenant deployment key, stored only as its sha256. Many devices enrol with one key, so it is counted (enrolment_count, last_used_at) and revoked rather than consumed; for a tenant whose device_verification is intune, the key alone is not enough and Graph must confirm the device is managed.';

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
  'Pre-tenant: the ACTIVE Entra connection for a token''s tid, or nothing. A pending or disabled connection signs nobody in.';

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
  'Pre-tenant: the ACTIVE OIDC connection whose issuer is exactly p_issuer, or nothing. No normalisation: the issuer is compared as the provider signs it.';

-- Any status, deliberately: onboarding signs the first admin in through a connection that is still
-- pending, and the attempt row names the connection it began with.
CREATE FUNCTION ops.identity_connection_by_id(p_connection uuid)
RETURNS SETOF ops.identity_connection
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.*
    FROM ops.identity_connection c
   WHERE c.connection_id = p_connection
$$;

COMMENT ON FUNCTION ops.identity_connection_by_id(uuid) IS
  'Pre-tenant: one connection by id in any status, for completing a sign-in that began against it (onboarding completes through a pending one). The caller decides what each status permits.';

CREATE FUNCTION ops.tenant_for_email_domain(p_domain text)
RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT d.tenant_id
    FROM ops.tenant_email_domain d
   WHERE d.domain = lower(p_domain)
$$;

COMMENT ON FUNCTION ops.tenant_for_email_domain(text) IS
  'Pre-tenant: the tenant a sign-in email domain belongs to, or NULL. Only the tenant id: which connection signs the person in is then read under that tenant''s row-level security.';

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
  'Pre-tenant: an onboarding invite that is still unused and unexpired, or nothing. A used invite does not resolve, so a forwarded link cannot onboard a second admin.';

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
  'Pre-tenant: the live session behind a cookie (by the sha256 of its value), or nothing once it is revoked or past expires_at. The idle limit and SCIM deactivation are checked by the caller, which then holds the tenant.';

CREATE FUNCTION ops.tenant_for_scim_token(p_hash text)
RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT t.tenant_id
    FROM ops.scim_token t
   WHERE t.token_hash = p_hash
     AND t.revoked_at IS NULL
$$;

COMMENT ON FUNCTION ops.tenant_for_scim_token(text) IS
  'Pre-tenant: the tenant an UNREVOKED SCIM bearer belongs to, or NULL. A revoked token resolves to nothing, so provisioning stops the moment an admin revokes it.';


-- =====================================================================================
-- 6. ingest — immutable observations and the logical submission
-- =====================================================================================

-- One row per observation. Two routes observing one submission produce two rows here and one
-- row in ingest.submission. Append-only: UPDATE is blocked outright; DELETE is possible only
-- through the retention path, which sets a session flag, so that whole-record expiry is
-- distinguishable from an accidental or malicious removal.
CREATE TABLE ingest.observation (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  event_id          uuid NOT NULL,
  device_id         uuid NOT NULL,
  user_ref          text NOT NULL,
  -- The clear account name at submission time, present only while the tenant's device_identity is
  -- 'clear' and only when the device could attribute the observation to a person. NULL is a real
  -- answer (a background process, a headless host, a hashed tenant) and the read falls back to
  -- user_ref; it is never a placeholder for "unknown person".
  subject_name      text,
  tool_fingerprint  text NOT NULL,
  direction         text NOT NULL CHECK (direction IN ('egress','ingress','none')),
  kind              text NOT NULL CHECK (kind IN ('prompt','usage_rollup','model_detection')),
  -- The device's decision about what kind of prompt this is (task 08): text a person typed
  -- ('user'), a request the client made for itself ('client_generated'), or a device that could
  -- not decide ('unknown'). It is request-shape metadata, not content-derived, so it is present
  -- on a prompt at M1 and above; M0 read no body to decide from and the field is NULL there, and
  -- it is NULL for the rollup and detection kinds. NULL is read as 'unknown' downstream.
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
  detection_basis   text CHECK (detection_basis IN ('process_scan','endpoint_security','etw','module_signature')),
  dedup_key         text NOT NULL CHECK (dedup_key ~ '^sha256:[0-9a-f]{64}$'),
  schema_version    text NOT NULL,
  ingested_at       timestamptz NOT NULL DEFAULT now(),
  expires_at        timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, event_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),

  -- The contract's mode and kind boundaries, restated where they can actually be enforced.
  -- Brief §1.1 makes M0 a permission boundary: at M0 the collector must not have read content at
  -- all, so a content-derived field on an M0 record is evidence of a defect and is rejected
  -- rather than ignored.
  --
  -- Each forbid-list below is meant to be EQUAL to the `not.anyOf` of the matching branch in
  -- contracts/event-envelope.schema.json. Three of them had drifted and constrained only some of
  -- the fields the contract forbids, so a record arriving by any path other than the ingest gate
  -- -- a migration, a restore, a support query, a future service -- was accepted while carrying
  -- a field its kind is defined not to have. The database is the last line that does not depend
  -- on application code being correct, so a half-enforced boundary is worse than none: it reads
  -- as a guarantee. db/tools/check-schema.mjs now compares these lists against the contract
  -- mechanically, so the next drift fails a check instead of waiting to be discovered.
  --
  -- `attachments` is the one contract field with no column here: attachment descriptors are
  -- envelope-level, and their store-side home is ingest.search_text, written by content-vault and
  -- keyed by submission. A CHECK on this table cannot see another table, so that field is
  -- enforced on the write path instead, and the checker is told to expect its absence here.
  -- M0's closed list is device, user, tool, timestamp, size and destination, so the request kind
  -- is not on it: a metadata-only device read no body from which to decide one. It is therefore
  -- in the M0 forbid-list, and db/tools/check-schema.mjs keeps that list equal to the contract's
  -- prompt:m0 branch.
  CONSTRAINT observation_m0_carries_no_content
    CHECK (collection_mode <> 'm0' OR (
      content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND confidence IS NULL AND content_excerpt IS NULL AND prompt_kind IS NULL)),
  CONSTRAINT observation_m1_plus_carries_labels
    CHECK (kind <> 'prompt' OR collection_mode = 'm0' OR (
      content_digest IS NOT NULL AND labels IS NOT NULL AND classifier_version IS NOT NULL
      AND confidence IS NOT NULL)),
  -- This constraint PERMITS an excerpt at M2; it does not REQUIRE one, while the contract does
  -- (contracts/event-envelope.schema.json, the M2 branch: then.required: ["content_excerpt"]).
  -- That gap is deliberate and recorded here so it stays a decision rather than a standing
  -- question:
  --
  --   * The contract's requirement is a statement about the WIRE -- an M2 record must carry an
  --     excerpt. It is enforced at ingest, where a violation becomes a mode_violation and a
  --     quarantine row, which is the diagnosability docs/02 section 7 exists for.
  --   * The store's job is to make the dangerous states unreachable. "M2 without an excerpt" is
  --     not dangerous: it is a less informative record, not one that leaks content or
  --     double-counts. Every other constraint in this block tightens a prohibition, and adding a
  --     requirement here would run the other way, making a previously-valid row invalid -- a
  --     migration question, not a constraint question.
  --   * It would also have to answer "what about an M2 record whose excerpt was minimised away to
  --     nothing", and the honest answer is that the write path decides, not a CHECK.
  --
  -- db/tools/check-schema.mjs asserts this difference and reports it as excused-by-decision, the
  -- same treatment attachments gets, so the exemption cannot silently become something else.
  CONSTRAINT observation_excerpt_only_at_m2
    CHECK (content_excerpt IS NULL OR (kind = 'prompt' AND collection_mode = 'm2')),
  CONSTRAINT observation_prompt_shape
    CHECK (kind <> 'prompt' OR (
      direction = 'egress' AND size_bytes IS NOT NULL AND policy_decision IS NOT NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL AND detection_basis IS NULL)),
  CONSTRAINT observation_rollup_shape
    CHECK (kind <> 'usage_rollup' OR (
      direction = 'none' AND window_start IS NOT NULL AND window_end IS NOT NULL
      AND submission_count IS NOT NULL AND bytes_total IS NOT NULL
      AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND content_excerpt IS NULL AND policy_decision IS NULL AND size_bytes IS NULL
      AND detection_basis IS NULL AND prompt_kind IS NULL)),
  CONSTRAINT observation_detection_shape
    CHECK (kind <> 'model_detection' OR (
      direction = 'none' AND detection_basis IS NOT NULL
      AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND content_excerpt IS NULL AND policy_decision IS NULL AND size_bytes IS NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL AND prompt_kind IS NULL))
);

COMMENT ON TABLE ingest.observation IS
  'The raw, immutable observation record. Retention is enforced by whole-row expiry, never by in-place editing: brief C34 requires two independent mechanisms and the reconciler is one of them.';

COMMENT ON COLUMN ingest.observation.expires_at IS
  'Computed at write time from ops.retention_policy, matched on the highest-severity class present in `labels` and falling back to the mode default. Materialised rather than computed at read time so that a later policy change does not silently retro-apply to data that was collected under a different promise.';

COMMENT ON COLUMN ingest.observation.prompt_kind IS
  'The device''s request kind (task 08): ''user'' for text a person authored, ''client_generated'' for a request the client made for itself (titling, summarisation, telemetry, an injected message), ''unknown'' when the device could not decide. Decided on the device from request shape, never from meaning; defaults to ''user'' when unsure so nothing a person typed is hidden. Present only for kind=prompt at M1 and above: M0 read no body and the field is not in its closed list. NULL reads as ''unknown''.';

-- A server-derived weak dedup key, used when a route could not compute a canonical content
-- digest. It is derived on the server rather than trusted from the device so that two
-- collectors written independently produce identical values for the same observation.
--
-- Brief §4.1 requires `dedup_key` to be derived from tenant, device, tool, a normalised
-- content digest and a time bucket. At M0 the collector is forbidden from reading content, so
-- there is no digest to normalise -- the brief's own formula cannot be evaluated. Rather than
-- let that case silently double-count, the observation is filed under this weaker key, which
-- is the same formula with the digest term replaced by the payload size.
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

COMMENT ON FUNCTION ingest.weak_dedup_key(uuid, uuid, text, text, timestamptz, bigint) IS
  'Server-derived dedup key for observations that carry no canonical content digest: the brief §4.1 formula with the digest term replaced by the payload size, bucketed at 300 seconds. `kind` is part of the key because rollups and detections carry no size at all: without it, a usage_rollup and a model_detection for the same tool in the same bucket would collapse into one record, which would be a fabrication rather than a merge.';

-- The deduplicated logical fact. This is what every aggregate and every finding is derived
-- from, so a customer-facing count can never be inflated by two routes observing one
-- submission (brief R9). It is also what keeps the count explainable: observed_routes records
-- every route that saw it, and winning_source records which one supplied the content.
--
-- Two keys, not one, and the reason matters. A submission observed by two routes that both
-- read content merges on the exact `dedup_key`. A submission observed by routes that could
-- not read content (M0, a canvas UI, an established WebSocket) has no exact key and merges on
-- `dedup_weak_key` instead. The two uniqueness rules are deliberately asymmetric:
--
--   * exact keys are unique among themselves, so two confident but *different* digests stay
--     two rows -- canonicalisation divergence is a defect that must be visible, not a
--     condition the database silently papers over;
--   * the weak key binds only rows that have no exact key, so a weak observation cannot pull
--     a confident one into a merge it does not belong in.
--
-- An exact observation that matches an existing weak-only row *adopts* it rather than
-- inserting a twin, which is what stops the M0-observed-by-one-route and
-- content-observed-by-another case from counting twice.
CREATE TABLE ingest.submission (
  tenant_id          uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id      uuid NOT NULL,
  dedup_key          text CHECK (dedup_key ~ '^sha256:[0-9a-f]{64}$'),
  dedup_weak_key     text NOT NULL CHECK (dedup_weak_key ~ '^sha256:[0-9a-f]{64}$'),
  -- Carried on the submission rather than left to a semi-join on ingest.observation, because
  -- every aggregation run needs the prompt / rollup / detection split and paying for it once
  -- here is cheaper than paying for it on every rollup. A submission is one kind: two
  -- observations of one submission observed the same thing.
  kind               text NOT NULL CHECK (kind IN ('prompt','usage_rollup','model_detection')),
  -- The winning observation's request kind (task 08): 'user', 'client_generated' or 'unknown'.
  -- NULL for the rollup and detection kinds and for a prompt folded from an M0 or pre-task-08
  -- observation, which the read layer treats as 'unknown'. A client_generated submission is never
  -- indexed for prompt-text search, so the kind gates the content index as well as the list filter.
  prompt_kind        text CHECK (prompt_kind IS NULL OR (prompt_kind IN ('user','client_generated','unknown') AND kind = 'prompt')),
  device_id          uuid NOT NULL,
  user_ref           text NOT NULL,
  -- The clear account name at submission time; see ingest.observation.subject_name. A submission
  -- merged from several routes keeps the name of the winning (highest-fidelity) observation, which
  -- is the same route whose content-bearing fields win, so the name and the content always describe
  -- the same observation.
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
  winning_fidelity   int NOT NULL,
  observed_routes    text[] NOT NULL,
  observation_count  int NOT NULL DEFAULT 1 CHECK (observation_count >= 1),
  merge_confidence   text NOT NULL DEFAULT 'high' CHECK (merge_confidence IN ('high','low')),
  content_state      text NOT NULL DEFAULT 'not_captured' CHECK (content_state IN ('not_captured','local_only','uploaded','shredded')),
  shredded_reason    text CHECK (shredded_reason IN ('retention_expired','erasure','hold_released','tenant_offboarded')),
  expires_at         timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, submission_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),

  -- Brief §3.2: local_only, uploaded and shredded are states that must never be merged.
  -- not_captured is added because at M0 and M1 no content was ever taken, which is a
  -- different fact from content that was taken and has since been destroyed.
  CONSTRAINT submission_shredded_requires_reason
    CHECK ((content_state = 'shredded') = (shredded_reason IS NOT NULL)),
  CONSTRAINT submission_no_content_without_capture
    CHECK (content_state <> 'not_captured' OR shredded_reason IS NULL),
  -- An exact key implies the content that produced it was read, and the mode that permits
  -- reading it. This is the invariant doc 02 proves: dedup_key determines dedup_weak_key.
  CONSTRAINT submission_exact_key_implies_digest
    CHECK (dedup_key IS NULL OR content_digest IS NOT NULL)
);

CREATE UNIQUE INDEX submission_exact_key_uniq
  ON ingest.submission (tenant_id, dedup_key)
  WHERE dedup_key IS NOT NULL;

CREATE UNIQUE INDEX submission_weak_key_uniq
  ON ingest.submission (tenant_id, dedup_weak_key)
  WHERE dedup_key IS NULL;

COMMENT ON COLUMN ingest.submission.winning_fidelity IS
  'The fidelity_rank of winning_source, denormalised so the upsert can compare without a join. Lower is better. When a higher-fidelity route reports the same submission later, the row is upgraded in place and observed_routes gains the new route; when a lower-fidelity route reports it, only observed_routes changes. That is brief §4.1''s "the higher-fidelity one wins and the record must carry which route produced it".';

COMMENT ON COLUMN ingest.submission.merge_confidence IS
  'Low when the dedup key was derived from weaker material than a canonical content digest -- mode M0, a canvas UI, or a WebSocket session where only the handshake was visible. A low-confidence row is never merged with an unrelated event and is never discarded; it is counted and reported separately, so a customer sees "N observations we cannot merge" instead of a number that is quietly wrong.';

COMMENT ON COLUMN ingest.submission.expires_at IS
  'The row-level half of brief C34''s two independent expiry mechanisms. The other is the blob lifecycle rule for content objects, and the reconciler compares them.';

COMMENT ON COLUMN ingest.submission.prompt_kind IS
  'The winning observation''s request kind (task 08), carried on the submission so the event list can filter without a semi-join on ingest.observation. NULL for the rollup and detection kinds and for a prompt folded from an M0 or pre-task-08 observation; the read layer treats NULL as ''unknown''. content-vault does not build a search index for a ''client_generated'' submission, and Search hides them by default.';

-- Quarantine for rejected envelopes. Brief §4.3 requires per-reason rejection detail "so an
-- agent defect is diagnosable from the server side without access to the device" -- but the
-- product's premise is that content does not leave the device, so a quarantine table that
-- faithfully stored the offending envelope would recreate the exact problem the architecture
-- exists to avoid. This table therefore keeps the diagnosis and discards the content.
CREATE TABLE ingest.rejected (
  tenant_id          uuid NOT NULL,
  rejected_id        bigint GENERATED ALWAYS AS IDENTITY,
  device_id          uuid,
  received_at        timestamptz NOT NULL DEFAULT now(),
  reason_code        text NOT NULL CHECK (reason_code IN (
                       -- The §7 wire vocabulary, spelled exactly as docs/02 §7 and
                       -- device/protocol.ReasonCode spell it. Three of these were previously
                       -- spelled differently here for the same facts (device_revoked,
                       -- batch_oversize, schema_version_unsupported); §7 is the closed contract,
                       -- so the quarantine surface follows it rather than translating to it.
                       'schema_violation','unsupported_schema_version','unknown_kind',
                       'unknown_tenant','revoked_device','region_mismatch','mode_violation',
                       'oversize',
                       -- Storage-only facts. They have no wire code because the record was held
                       -- at a layer that has no per-event outcome to report: an unparseable
                       -- envelope, a server-side fault, and a device-computed dedup key that
                       -- disagrees with the server's derivation.
                       'malformed_json','dedup_key_mismatch','internal_error')),
  detail             jsonb NOT NULL DEFAULT '{}'::jsonb,
  field_presence     jsonb NOT NULL DEFAULT '{}'::jsonb,
  envelope_redacted  jsonb,
  expires_at         timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, rejected_id),
  -- Mechanical guarantee that quarantine is not a back door for content. The redacted envelope
  -- may carry the fields needed to diagnose a collector defect; it may not carry content or
  -- anything derived from it.
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

COMMENT ON TABLE ingest.rejected IS
  'Rejected envelopes, content-stripped, with a short TTL. reason_code carries the docs/02 §7 wire vocabulary, spelled exactly as device/protocol.ReasonCode spells it, plus three storage-only codes that have no per-event wire outcome: malformed_json, dedup_key_mismatch and internal_error. Two wire codes deliberately never appear here -- tenant_mismatch, because tenant_id is NOT NULL and RLS-scoped so a cross-tenant body has no honest tenant to file it under, and duplicate_batch, which is batch-level by construction and has no per-event envelope to quarantine. services/ingest-api/internal/store.QuarantineReason is the single place that translation lives, and db/tools/check-schema.mjs asserts every code it can emit is accepted by this CHECK. See docs/03-data-platform.md for the retention window.';

-- The content search index. One row per searchable unit: the prompt body, and one row per
-- attachment filename.
--
-- ADR 0014. Content search is a required product capability, so this table exists; the earlier
-- position (ADR 0008, "no server-side content search in any key mode") is superseded. Three
-- properties of the design are load-bearing and each is enforced somewhere the reader can check:
--
--   1. Only `content-vault` can read it. It is not granted to sac_query, so the component that
--      answers analyst questions cannot touch an index of plaintext prompt content. Search runs
--      inside the vault and returns hits and snippets. This preserves the invariant that exactly
--      one component can read content.
--   2. Only tenants whose key custody makes it possible have one at all. The check constraint on
--      ops.tenant refuses `full_text` together with `customer_held`.
--   3. It dies with the row it describes. The foreign key cascades, so a subject erasure or a
--      retention expiry that removes a submission removes its index entries in the same
--      transaction, without a second deletion path that could be forgotten or drift.
--
-- What the index costs, stated plainly: `tsv` is derived from plaintext and is therefore itself
-- readable by anyone who can read the table. A search index over content is a second copy of
-- that content's terms. There is no cryptographic construction that gives search without
-- leakage, and this design does not pretend otherwise.
--
-- One asymmetry worth knowing before relying on the cascade below: `search_text` dies with its
-- submission, but `ingest.observation` does NOT, because it has no foreign key to the submission
-- -- the observation is written first and is immutable, so the link cannot be set at insert time.
-- Observations and submissions are therefore deleted by the same predicate rather than by
-- referential action, and the reconciler carries a check for observations whose submission has
-- gone. That check is what stands in for the constraint; see docs/03-data-platform.md §6.1 and
-- open question Q14.
CREATE TABLE ingest.search_text (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id uuid NOT NULL,
  unit_kind     text NOT NULL CHECK (unit_kind IN ('prompt_body','attachment_name')),
  -- Ordinal within the submission: 0 for the prompt body, 0..n for each attachment filename.
  unit_index    int NOT NULL DEFAULT 0 CHECK (unit_index >= 0),
  body          text NOT NULL CHECK (length(body) BETWEEN 1 AND 65536),
  -- The two-argument form of to_tsvector is IMMUTABLE and therefore legal in a generated column;
  -- the one-argument form is only stable, because it depends on default_text_search_config and
  -- would make the stored vector depend on session state. 'simple' rather than a language
  -- config because the product is multi-lingual and stemming the wrong language is worse than
  -- not stemming at all. Language-specific configurability is a later refinement.
  tsv           tsvector GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, body)) STORED,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, submission_id, unit_kind, unit_index),
  CONSTRAINT search_text_body_required CHECK (unit_kind <> 'prompt_body' OR unit_index = 0),
  FOREIGN KEY (tenant_id, submission_id)
    REFERENCES ingest.submission(tenant_id, submission_id) ON DELETE CASCADE
);

COMMENT ON TABLE ingest.search_text IS
  'Full-text and filename index over content, written by content-vault when content is stored and read only by content-vault when an analyst searches. Prompt bodies are present only for tenants whose content_search is full_text; attachment filenames are present for attachment_names and above. Attachment CONTENTS are deliberately not indexed in v1: that is the 50-500 GB/year tier from brief §3.1 and a separate decision.';

COMMENT ON COLUMN ingest.search_text.expires_at IS
  'The index entry expires with the content it indexes, and is swept by the same retention path. An index that outlived its content would be a search result that cannot be opened, and a subject erasure that left terms behind.';

-- Both indexes lead with tenant_id, per brief C32. That needs btree_gin, because a GIN index
-- over tsvector cannot otherwise carry a uuid column. Worth the extra extension: without it the
-- index would be tenant-blind and C32's "tenant is the leading dimension of every index" would
-- have an exception that no reviewer could see.
CREATE INDEX search_text_tsv_gin
  ON ingest.search_text USING gin (tenant_id, tsv);

-- Substring and fuzzy matching for filenames. Partial, because a trigram index over every prompt
-- body would be large for no benefit: full-text search is the right tool for prose, and trigrams
-- are the right tool for "find me the file called something like Q3-contract".
CREATE INDEX search_text_name_trgm
  ON ingest.search_text USING gin (tenant_id, body gin_trgm_ops)
  WHERE unit_kind = 'attachment_name';

CREATE INDEX search_text_expiry
  ON ingest.search_text (tenant_id, expires_at);


-- =====================================================================================
-- 7. mart — derived, rebuildable, no workflow state
-- =====================================================================================

-- Findings: submissions that matched a rule. The key is the natural key rather than a
-- surrogate id, so that rebuilding mart from ingest produces identical rows and therefore
-- cannot orphan a review decision held in ops.finding_review.
--
-- A finding records the match, not the rule's attributes. class_code, severity and title are
-- present-tense configuration read from ref.rule at query time (mart.v_finding below), so a
-- customer editing a rule sees the change on every finding that names it. That follows the product
-- decision recorded in backlog/03-findings/DECISIONS.md: the customer authors the rules and wants
-- their current definition to govern. It is the opposite of mart.agg_tool_period's treatment of
-- sanctioned state, deliberately: findings are a match log read by an analyst, aggregates are a
-- reconstructed history.
CREATE TABLE mart.finding (
  tenant_id        uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  submission_id    uuid NOT NULL,
  rule_id          text NOT NULL REFERENCES ref.rule(rule_id),
  detected_at      timestamptz NOT NULL,
  decided_locally  boolean NOT NULL,
  collection_mode  text NOT NULL CHECK (collection_mode IN ('m0','m1','m2','m3')),
  PRIMARY KEY (tenant_id, submission_id, rule_id),
  FOREIGN KEY (tenant_id, submission_id) REFERENCES ingest.submission(tenant_id, submission_id)
);

COMMENT ON TABLE mart.finding IS
  'Derived. Review state is NOT here -- it is in ops.finding_review, keyed by the same natural key, so that DROP and rebuild of this schema cannot destroy an analyst''s judgement. class_code, severity and title come from the current ref.rule row at read time, not from a snapshot.';

-- Aggregates. Every one of these is written with INSERT .. ON CONFLICT DO UPDATE that REPLACES
-- the bucket, never increments it: brief C28, "aggregates are upserts, never increments",
-- because devices go offline and flush in bursts, so late-arriving events are the normal case
-- rather than the exception. Those upserts conflict on the primary keys defined below.

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
  -- Detection-only evidence (usage mode I) and rolled-up observation. Both are needed for
  -- brief §3.6 question 1: a tool that is known only because an on-device model ran has no
  -- submissions at all, so without these columns it would be absent from "which AI tools are
  -- in use" -- a silently incomplete answer, which is exactly what C25 forbids.
  detections       bigint NOT NULL DEFAULT 0 CHECK (detections >= 0),
  rollup_events    bigint NOT NULL DEFAULT 0 CHECK (rollup_events >= 0),
  -- Brief C21 and §6: when the classifier cannot run, the event is still recorded but its
  -- labels are missing. Without this measure a classifier outage renders on the dashboard as
  -- a fall in sensitive data rather than as a fall in classifier health.
  degraded_events  bigint NOT NULL DEFAULT 0 CHECK (degraded_events >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, tool_fingerprint)
);

COMMENT ON TABLE mart.agg_tool_period IS
  'Brief §3.6 question 1 and part of question 2. Sanctioned state is deliberately NOT denormalised here: it is present-tense tenant configuration rather than a property of the event, so it is joined from ops.tool at read time and its history is read from ops.audit. Storing it here would make historical aggregates silently change meaning when someone re-classifies a tool.';

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

CREATE TABLE mart.agg_class_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  class_code       text NOT NULL REFERENCES ref.data_class(class_code),
  tool_fingerprint text NOT NULL,
  severity         text NOT NULL CHECK (severity IN ('low','medium','high','critical')),
  -- In the key, not a measure. Brief §6 requires a change in classifier behaviour to be
  -- visible as a version change rather than as a mysterious shift in the numbers; if the
  -- version were only a measure, two classifier versions sharing a bucket would be added
  -- together and the shift would be invisible by construction.
  classifier_version text NOT NULL,
  submissions      bigint NOT NULL CHECK (submissions >= 0),
  users            bigint NOT NULL CHECK (users >= 0),
  max_score        numeric(4,3) CHECK (max_score >= 0 AND max_score <= 1),
  degraded_events  bigint NOT NULL DEFAULT 0 CHECK (degraded_events >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, class_code, tool_fingerprint, severity, classifier_version)
);

COMMENT ON TABLE mart.agg_class_period IS
  'Brief §3.6 question 4. users is carried so the read layer can suppress small cells: a per-team, per-day count for a team of two identifies individuals, so it is subject-level data for audit purposes and must be suppressible.';

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

COMMENT ON TABLE mart.agg_org_period IS
  'Brief §3.6 question 3, "how much is usage growing, per team". Depends entirely on ops.user_dim, which is synchronised from the customer''s directory -- an inbound data flow the brief does not describe and which is open question Q2. Until a tenant has a directory sync, this table is empty for them and question 3 is answered by user and tool instead.';

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

COMMENT ON TABLE mart.agg_user_period IS
  'Brief §3.6 question 6, "has a given person''s usage changed, or spiked". A per-subject time series, so every read of it is subject-level data and writes an audit entry as it is served (brief §3.6). Deliberately carries no score, ranking or efficiency measure: brief §1.2 forbids productivity analytics, and this table is the one that could most easily become one.';

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

COMMENT ON TABLE mart.agg_device_period IS
  'Brief §3.6 question 7. The daily rollup of the health upserts in ops.collector_state (master doc D5). The four state counts are separate columns rather than one because brief §3.2 forbids merging them.';

-- Device liveness. Brief §3.6 question 7 asks "the state of every device's collection,
-- including devices not reporting", and brief C24 requires tamper and absence to be reported
-- rather than inferred. A device that has simply stopped is not represented by any event, so
-- silence has to be turned into a row by something other than the device.
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
       (SELECT count(*) FROM ops.collector_state cs
         WHERE cs.tenant_id = d.tenant_id AND cs.device_id = d.device_id) AS collectors_reporting
  FROM ops.device d;

COMMENT ON VIEW mart.v_device_liveness IS
  'Turns an absence of events into an explicit state. `stale` and `never_reported` are different facts and are not merged, and neither is `revoked`: a device that was deliberately removed is not a device that has gone quiet. It also carries the device identity fields the Devices page shows (hostname, agent_version, collection_mode, managed_state) and the most-recent user (last_user_ref, last_subject_name), which makes a read of it subject-level: docs/04 §5.2 audits any query that returns a subject reference, so this view''s reads are audited as served.';

-- Sanctioned state joined to aggregates, because the state is present-tense configuration
-- rather than a property of the event (see the note on mart.agg_tool_period).
CREATE VIEW mart.v_tool_usage
WITH (security_invoker = true) AS
SELECT a.tenant_id,
       a.bucket_start,
       a.bucket_size,
       a.tool_fingerprint,
       coalesce(t.display_name, a.tool_fingerprint) AS display_name,
       t.sanctioned_state,
       a.submissions,
       a.users,
       a.bytes_total,
       a.blocked,
       a.warned,
       a.logged
  FROM mart.agg_tool_period a
  LEFT JOIN ops.tool t
    ON t.tenant_id = a.tenant_id
   AND t.tool_fingerprint = a.tool_fingerprint;

COMMENT ON VIEW mart.v_tool_usage IS
  'Brief §3.6 questions 1 and 2. LEFT JOIN, not INNER: a tool with no ops.tool row must still appear, reporting sanctioned_state as NULL, so that the read layer can render it as `unknown` rather than dropping it. Brief C8 forbids conflating unknown with prohibited, and silently omitting the row would be a third kind of wrong.';

-- The one place a fingerprint becomes a name. Resolution is present-tense, at read time, exactly
-- as sanctioned state is (ops.tool is configuration; ref.tool_catalogue is the shared seed), so a
-- catalogue addition or a tenant override shows on the next read without rewriting any aggregate.
--
-- The order of the coalesce is the product's: a tenant's own label wins, then the shared
-- catalogue, then the explicit "Unrecognised tool". It never falls back to the raw fingerprint as
-- the *name*, because a fingerprint shown in a name's place reads like a tool we know and have
-- merely labelled badly (brief §2, C8). The raw value still travels on every row as `tool`.
CREATE FUNCTION ops.tool_display_name(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce(
    (SELECT t.display_name
       FROM ops.tool t
      WHERE t.tenant_id = ops.current_tenant()
        AND t.tool_fingerprint = p_tool_fingerprint
        AND t.display_name IS NOT NULL),
    (SELECT c.display_name
       FROM ref.tool_catalogue c
      WHERE c.tool_fingerprint = p_tool_fingerprint),
    'Unrecognised tool')
$$;

COMMENT ON FUNCTION ops.tool_display_name(text) IS
  'Fingerprint -> display name: tenant override, else shared catalogue, else "Unrecognised tool". STABLE, tenant-scoped through ops.tool RLS; the raw fingerprint is never substituted for the name.';

-- Findings joined to the current rule definition and to review state (defaulting to open).
--
-- class_code and severity are read from ref.rule, not from mart.finding: they are present-tense
-- configuration. A rule edit therefore shows on every finding that names it. That is the owner's
-- decision (backlog/03-findings/DECISIONS.md); the title was always read from ref.rule this way.
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

COMMENT ON VIEW mart.v_finding IS
  'Brief §3.6 question 5: filtered list with severity and review state. class_code, severity and title are present-tense (ref.rule), so a rule edit shows on every finding that names it; the review coalesce to open is a rendering of "nobody has reviewed this", not an assertion that it was reviewed and found unremarkable.';

-- What a human needs to see before deciding to close a gate.
--
-- The product owner's rule is that suspension is not automated: a person decides. That moves the
-- risk out of the system and into the process, so the system's obligation becomes making the
-- decision informed rather than merely recorded. This view is that obligation. An operator about
-- to set `ingest_enabled = false` should be able to see that N devices are currently holding M
-- events in their spools, because those events will be dropped oldest-first and cannot be
-- re-collected afterwards (brief §1: nothing else observes them).
--
-- Deliberately tenant-scoped by row-level security rather than being a cross-tenant operator
-- console. Looking at one tenant at a time before acting on it is the safer shape: a bulk view
-- of every tenant's exposure invites a bulk action, and only one of the two gates is reversible.
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

COMMENT ON VIEW mart.v_tenant_suspension_impact IS
  'Decision support for the human who closes a gate. events_spooled_on_devices is the estimate of what a closure of ingest_enabled would strand, and events_already_dropped is the visible undercount brief C22 requires. Spool depth is taken as the per-device maximum across collectors because the spool is shared, so summing every collector row would multiply it.';


-- =====================================================================================
-- 8. Enforcement functions and triggers
-- =====================================================================================

-- Brief C3: "the mode in force is applied before a policy bundle is signed, so a device can
-- never exceed its tenant's ceiling." The ceiling is ops.tenant.ceiling_mode; the bundle
-- carries a scope matrix. This trigger makes the ceiling a database invariant rather than a
-- validation the configuration API is trusted to perform.
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

  -- The scope matrix is a JSON object whose leaves are mode strings. jsonb_path_query pulls
  -- every string leaf at any depth. An unrecognised mode yields NULL from mode_rank and is a
  -- hard error rather than a skip: a mode the server cannot read must never become an
  -- unrestricted one, which is the failure this constraint exists to prevent.
  SELECT count(*) FILTER (WHERE ops.mode_rank(q.value #>> '{}') IS NULL),
         max(ops.mode_rank(q.value #>> '{}'))
    INTO v_unknown, v_worst
    FROM jsonb_path_query(NEW.scope_matrix, '$.** ? (@.type() == "string")') AS q(value);

  IF v_unknown > 0 THEN
    RAISE EXCEPTION 'policy bundle % for tenant % contains an unrecognised collection mode',
      NEW.bundle_version, NEW.tenant_id;
  END IF;

  -- No scope at all is also refused. A bundle that specifies nothing cannot be read as
  -- "unrestricted"; it is a malformed bundle, and defaulting it either way would be a guess.
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

COMMENT ON FUNCTION ops.enforce_policy_ceiling() IS
  'Refuses a signed policy bundle in which any scope exceeds the tenant ceiling, and refuses one containing an unrecognised mode. Implements brief C3 at the storage layer, so the configuration API cannot be the only thing standing between a tenant and a mode it is not allowed to use.';

CREATE TRIGGER policy_bundle_ceiling
  BEFORE INSERT OR UPDATE ON ops.policy_bundle
  FOR EACH ROW EXECUTE FUNCTION ops.enforce_policy_ceiling();

-- The audit hash chain. Per tenant, each row hashes its predecessor; the chain head is
-- periodically anchored to write-once storage, which is what makes removal of a middle row
-- detectable rather than merely discouraged.
--
-- The advisory lock is taken per tenant for the duration of the transaction so that two
-- concurrent writers cannot both read the same predecessor and fork the chain.
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

-- The trigger runs with the inserting role's rights and reads the previous row's hash, so every
-- role that may INSERT an audit row needs to read exactly the three columns the chain reads. With
-- INSERT alone the insert fails ("permission denied for table audit"), which only stayed hidden
-- while the lab's services connected as a superuser. RLS still confines the read to the inserting
-- session's own tenant, which is the tenant the row must carry anyway.
GRANT SELECT (tenant_id, audit_seq, row_hash) ON ops.audit TO sac_ingest, sac_control, sac_vault, sac_ops;

-- Append-only enforcement. UPDATE is blocked for every role including the owner, because
-- nothing in this system legitimately rewrites history. DELETE is blocked too, except when
-- the reconciler has explicitly declared that it is performing retention expiry -- so that
-- "the row is gone" is always attributable either to a retention run or to a privileged
-- intervention that had to set a flag to do it.
CREATE FUNCTION ops.block_audit_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ops.audit is append-only; % is not permitted', TG_OP;
  RETURN NULL;
END $$;

CREATE TRIGGER audit_append_only
  BEFORE UPDATE OR DELETE ON ops.audit
  FOR EACH STATEMENT EXECUTE FUNCTION ops.block_audit_mutation();

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

-- Single-use enforcement for a content retrieval grant. The claim path is a conditional UPDATE
-- matching `used_at IS NULL`, and a row that does not match is never updated -- so this trigger
-- does not fire on the correct statement and does not change its behaviour or its "zero rows
-- means refused" result. It fires on the statement that has forgotten the guard, which is the
-- only way a grant could be redeemed twice, and turns that from a silent overwrite of the first
-- redemption into an error.
--
-- A CHECK cannot do this: a CHECK is evaluated against the new row alone and cannot distinguish a
-- first claim from a second. This is the same shape as the observation and audit append-only
-- guards, for the same reason -- the store enforces the property rather than the caller.
CREATE FUNCTION ops.block_grant_reuse() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.used_at IS NOT NULL THEN
    RAISE EXCEPTION 'ops.retrieval_grant is single-use; grant % was already redeemed at % by % (content is served once per recorded grant)',
      OLD.grant_id, OLD.used_at, OLD.used_by;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER retrieval_grant_single_use
  BEFORE UPDATE ON ops.retrieval_grant
  FOR EACH ROW EXECUTE FUNCTION ops.block_grant_reuse();

-- Effective retention for an event, resolved at write time rather than at read time. The lookup
-- uses the highest-severity class present in the label set, because a row carrying a payment
-- card must not expire on the schedule of a row carrying nothing. M0 rows carry no labels by
-- construction, so they fall through to the tenant's mode default.
--
-- ASSUMPTION: when no policy row matches, the 'standard' retention class default applies. The
-- brief does not state a default anywhere; Q6 in the master doc owns the number, and this is
-- the mechanism that makes the number a policy setting rather than a code change.
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

COMMENT ON FUNCTION ops.event_ttl_days(uuid, text, jsonb) IS
  'Retention in days for an event, from ops.retention_policy matched on the highest-severity label class and the collection mode, falling back to the tenant mode default and then to the standard class. Materialised onto the row at write time so that a later policy change does not retro-apply.';

-- Idempotent event recording: the whole of brief C12 and R9 in one place, so that the
-- transport layer cannot implement the tie-break differently from the storage layer.
--
-- Input is the stored envelope as jsonb, which keeps this function in step with
-- contracts/event-envelope.schema.json rather than with a parameter list that drifts.
-- Returns 'inserted' | 'duplicate' | 'upgraded' | 'recorded', plus the submission id, so the
-- batch response can report per-event outcomes without a second query.
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


-- =====================================================================================
-- 9. Row-level security
-- =====================================================================================
-- Brief C32: "Isolation enforced at the storage layer, not only in application code. A
-- cross-tenant read must be structurally impossible, not merely unauthorised."
--
-- Every tenant-scoped table is ENABLED and FORCED. FORCE matters because it subjects the table
-- owner to the policy too, which closes the gap where a view or a maintenance path running as
-- the owner would otherwise see everything. The policies compare against ops.current_tenant(),
-- which returns NULL when the session has not set a tenant; a comparison against NULL is NULL,
-- which excludes the row. Fail-closed, not fail-open. The table list is explicit so that a
-- reviewer can see exactly what is covered.

DO $$
DECLARE
  t text;
  tenant_tables text[] := ARRAY[
    -- ops
    'ops.user_dim', 'ops.device', 'ops.device_credential', 'ops.enrolment_token',
    'ops.dpop_replay',
    'ops.collector_state',
    'ops.policy_bundle', 'ops.tool', 'ops.notice_acknowledgement', 'ops.retention_policy',
    'ops.hold', 'ops.audit', 'ops.grant', 'ops.retrieval_grant', 'ops.content_object',
    'ops.finding_review',
    'ops.erasure_receipt', 'ops.reconciliation_run', 'ops.aggregate_watermark',
    'ops.coverage_snapshot', 'ops.subscription', 'ops.usage_daily',
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

-- ops.tenant is keyed by tenant_id and is the tenant table itself, so its policy is the
-- degenerate case: a session may see exactly its own row.
ALTER TABLE ops.tenant ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.tenant FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ops.tenant
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

-- The pre-tenant lookups of section 5c read as sac_resolver, which is subject to the policies above
-- like any role. These SELECT-only policies are its whole reach: the five tables those functions
-- answer from, never a write, and nothing a runtime role can use, because none is a member of it.
CREATE POLICY pre_tenant_lookup ON ops.identity_connection FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.tenant_email_domain FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.onboarding_invite   FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.auth_session        FOR SELECT TO sac_resolver USING (true);
CREATE POLICY pre_tenant_lookup ON ops.scim_token          FOR SELECT TO sac_resolver USING (true);

-- ops.auth_signin has no tenant: an attempt begun with "Sign in with Microsoft" learns its tenant
-- only from the provider's answer. It is still forced, with a policy for the one role that drives
-- the sign-in, so a grant added to any other role by mistake still reads nothing.
ALTER TABLE ops.auth_signin ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.auth_signin FORCE ROW LEVEL SECURITY;
CREATE POLICY control_plane_only ON ops.auth_signin TO sac_control
  USING (true)
  WITH CHECK (true);

-- Reference tables are shared across tenants, are not tenant-scoped, and hold no personal
-- data, so they carry no policy. Anything tenant-specific derived from them lives in ops.


-- =====================================================================================
-- 10. Grants
-- =====================================================================================
-- The matrix in master doc §4.3 expressed as privileges. The operative rules:
--   * ingest-api writes events and cannot read content or keys.
--   * control-api manages configuration and grant decisions and cannot read content or keys.
--   * content-vault is the only role that can touch ops.content_object.
--   * query-api can read events, aggregates and findings, cannot see wrapped keys at all, and
--     cannot write anything except its own audit trail, a finding review and a tool sanction
--     decision (each an audited configuration write beside the read that shows it).
--   * the scheduled jobs own retention, aggregation and reconciliation.
--   * no runtime role has UPDATE or DELETE on ops.audit or UPDATE on ingest.observation.

-- ingest-api
GRANT SELECT ON ref.data_class, ref.route_fidelity, ref.collector, ref.classifier_release, ref.retention_class TO sac_ingest;
GRANT SELECT ON ops.tenant, ops.device, ops.device_credential, ops.retention_policy TO sac_ingest;
-- Accepting a batch is the device's activity, so ingest-api stamps last_seen_at in the same
-- transaction that accepts it (docs/04 §3.7, docs/01-collectors.md §14.5). The grant is
-- column-level and one-way: an activity stamp must not become a way for the write path to change a
-- device's identity, os or revocation. last_user_ref and last_subject_name are the same fact for the
-- person (the most recent submission's user), so the Devices read can name the user without a
-- subject join; they are still identity columns in the sense that matters -- the write path can set
-- these three timestamps-of-activity fields and nothing else about the device.
GRANT UPDATE (last_seen_at, last_user_ref, last_subject_name) ON ops.device TO sac_ingest;
-- The usage ledger is incremented by the component that accepts the events, in the same
-- transaction, because a billing counter that could commit without its data -- or data without
-- its counter -- would be wrong in a way nobody notices until an invoice is disputed.
GRANT SELECT, INSERT, UPDATE ON ops.usage_daily TO sac_ingest;
GRANT SELECT, INSERT ON ingest.observation TO sac_ingest;
GRANT SELECT, INSERT, UPDATE ON ingest.submission TO sac_ingest;
GRANT INSERT ON ingest.rejected TO sac_ingest;
-- The DPoP replay window: ingest-api records a jti with ON CONFLICT DO NOTHING and sweeps
-- expired rows, so it needs INSERT, SELECT and DELETE. There is no UPDATE: a seen jti is never
-- rewritten, and no other role is granted the table at all.
GRANT SELECT, INSERT, DELETE ON ops.dpop_replay TO sac_ingest;
GRANT INSERT ON ops.audit TO sac_ingest;
-- record_event resolves a device-derived user_ref to the person's canonical one as it writes
-- (section 5c). It runs with the caller's rights, so the read is a grant, and RLS keeps it to the
-- batch's own tenant. Read-only: the aliases are control-api's, written from SCIM.
GRANT SELECT ON ops.user_ref_alias TO sac_ingest;
GRANT EXECUTE ON FUNCTION ingest.record_event(jsonb, timestamptz) TO sac_ingest;
GRANT EXECUTE ON FUNCTION ops.current_tenant() TO sac_ingest;

-- control-api: enrolment, policy, health, grants. Deliberately no access to ops.content_object
-- and no access to ingest.observation content columns beyond the metadata it needs to decide a
-- grant. Column-level grants are used rather than a table grant, so "control-api can read the
-- event" cannot quietly become "control-api can read the labels".
GRANT SELECT, INSERT, UPDATE ON ops.tenant, ops.user_dim, ops.device, ops.device_credential,
      ops.enrolment_token,
      ops.collector_state, ops.policy_bundle, ops.tool, ops.notice_acknowledgement,
      ops.retention_policy, ops.hold, ops.grant, ops.coverage_snapshot, ops.finding_review,
      ops.subscription
  TO sac_control;
GRANT SELECT ON ops.usage_daily TO sac_control;
GRANT SELECT (tenant_id, submission_id, device_id, user_ref, tool_fingerprint, collection_mode,
              content_state, expires_at, received_at)
  ON ingest.submission TO sac_control;
GRANT SELECT, INSERT ON ops.erasure_receipt TO sac_control;
GRANT INSERT ON ops.audit TO sac_control;
GRANT SELECT ON ref.data_class, ref.route_fidelity, ref.collector, ref.classifier_release,
      ref.retention_class, ref.rule TO sac_control;
-- The policy bundle's interception hosts come from the catalogue's TLS destinations (policyserve).
GRANT SELECT ON ref.tool_catalogue TO sac_control;
GRANT EXECUTE ON FUNCTION ops.current_tenant(), ops.mode_rank(text) TO sac_control;
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

-- content-vault: the only role that can read wrapped keys.
GRANT SELECT, INSERT, UPDATE ON ops.content_object TO sac_vault;
-- The redemption path: put a grant, read one, claim one. INSERT and UPDATE are what
-- SQLPutRetrievalGrant and SQLClaimRetrievalGrant need, and UPDATE is deliberately paired with the
-- retrieval_grant_single_use trigger rather than with a DELETE: a redemption row is the record
-- that content left the vault, so no role is granted DELETE on it. A grant that expires is a row
-- with an expires_at in the past, not a row that disappears.
GRANT SELECT, INSERT, UPDATE ON ops.retrieval_grant TO sac_vault;
-- And the only role that can read or write the content search index. `query-api` is deliberately
-- NOT granted here: it calls content-vault, which runs the search and returns hits and snippets.
-- The alternative -- letting the query tier read an index of plaintext prompt content -- would
-- quietly retire the invariant that exactly one component can read content (ADR 0014).
GRANT SELECT, INSERT, UPDATE, DELETE ON ingest.search_text TO sac_vault;
GRANT SELECT ON ops.tenant, ops.retention_policy, ops.hold, ops.grant, ops.erasure_receipt TO sac_vault;
-- The vault composes a filtered content search by joining its index to these columns: the person,
-- tool, device, mode and receive time a search may narrow by (docs/04 §15.3). It is a column grant,
-- not a table grant, so this cannot quietly become access to labels, policy or content state. It is
-- the only metadata the vault reads from a submission; query-api still holds the read of the index.
GRANT SELECT (tenant_id, submission_id, device_id, user_ref, tool_fingerprint,
              collection_mode, content_state, received_at, expires_at)
  ON ingest.submission TO sac_vault;
GRANT INSERT ON ops.audit TO sac_vault;
GRANT SELECT, UPDATE ON ops.grant TO sac_vault;

-- query-api: read-mostly, no keys, no writes except audit and review.
GRANT SELECT ON ingest.submission, ingest.observation TO sac_query;
GRANT SELECT ON mart.finding, mart.agg_tool_period, mart.agg_tool_user_period,
      mart.agg_class_period, mart.agg_org_period, mart.agg_user_period, mart.agg_device_period,
      mart.v_device_liveness, mart.v_tool_usage, mart.v_finding,
      mart.v_tenant_suspension_impact TO sac_query;
GRANT SELECT ON ops.tenant, ops.user_dim, ops.device, ops.collector_state, ops.tool,
      ops.hold, ops.grant, ops.erasure_receipt, ops.coverage_snapshot, ops.aggregate_watermark,
      ops.retention_policy, ops.notice_acknowledgement, ops.policy_bundle,
      -- Needed by the degraded-collection screen: brief C34 requires drift between the two
      -- expiry mechanisms to be reconciled, and a reconciliation the dashboard cannot read is
      -- a reconciliation nobody acts on.
      ops.reconciliation_run,
      -- The operator's suspension decision support: what is owed, and what a gate closure
      -- would strand. Read-only: query-api can inform the decision but never take it.
      ops.subscription, ops.usage_daily TO sac_query;
GRANT SELECT ON ref.data_class, ref.rule, ref.classifier_release, ref.route_fidelity, ref.collector TO sac_query;
GRANT SELECT ON ref.tool_catalogue TO sac_query;
GRANT INSERT, SELECT ON ops.audit TO sac_query;
GRANT SELECT, INSERT, UPDATE ON ops.finding_review TO sac_query;
-- The tool sanction decision (POST /v1/tool-sanction). It is a configuration write that belongs
-- beside the read that shows it, exactly as a finding review does; the row and its audit entry
-- commit together. Still no content, still no keys, and no grant on any other write surface.
GRANT INSERT, UPDATE ON ops.tool TO sac_query;
GRANT EXECUTE ON FUNCTION ops.current_tenant(), ops.tool_display_name(text) TO sac_query;

-- sac_query reads observations for drill-down. It is NOT granted SELECT on ops.content_object,
-- so the component that answers analyst queries cannot see a wrapped data key even in
-- principle. Content is reached only through content-vault, which is not reachable from the
-- network.

-- Scheduled jobs: retention, aggregation, reconciliation, coverage.
GRANT SELECT, INSERT, UPDATE, DELETE ON ingest.observation, ingest.submission TO sac_ops;
GRANT SELECT, INSERT, UPDATE, DELETE ON ingest.rejected TO sac_ops;
GRANT SELECT, INSERT, UPDATE, DELETE ON mart.finding, mart.agg_tool_period,
      mart.agg_tool_user_period, mart.agg_class_period, mart.agg_org_period,
      mart.agg_user_period, mart.agg_device_period TO sac_ops;
GRANT SELECT, INSERT, UPDATE ON ops.content_object, ops.grant TO sac_ops;
GRANT SELECT, INSERT, UPDATE ON ops.reconciliation_run, ops.erasure_receipt,
      ops.aggregate_watermark, ops.coverage_snapshot TO sac_ops;
-- The reconciler takes the end-of-day device snapshot into the ledger. It may UPDATE the two
-- snapshot columns and nothing else: the accepted-event and content-byte counters are written
-- forward by ingest-api and must never be recomputed from ingest, for the reason given on the
-- table. UPDATE rather than DELETE, and no role is granted DELETE at all.
GRANT SELECT, UPDATE ON ops.usage_daily TO sac_ops;
GRANT SELECT ON ops.subscription TO sac_ops;
GRANT SELECT ON ops.tenant, ops.user_dim, ops.device, ops.collector_state, ops.hold,
      ops.retention_policy, ops.tool TO sac_ops;
GRANT SELECT ON ref.data_class, ref.rule, ref.classifier_release, ref.route_fidelity,
      ref.collector, ref.retention_class TO sac_ops;
GRANT INSERT ON ops.audit TO sac_ops;
GRANT EXECUTE ON FUNCTION ops.current_tenant() TO sac_ops;

-- Nothing is granted on ops.audit for UPDATE or DELETE to any role, by design. Combined with
-- the audit_append_only trigger, removing an audit row requires deliberately disabling a
-- trigger as a superuser, which is a privileged intervention rather than an API call.

-- Future objects created by the migration role inherit nothing automatically; default
-- privileges are revoked so that a new table is invisible until it is granted on purpose.
ALTER DEFAULT PRIVILEGES FOR ROLE sac_migrator IN SCHEMA ops, ingest, mart, ref REVOKE ALL ON TABLES FROM PUBLIC;


-- =====================================================================================
-- 11. Seed data
-- =====================================================================================

-- The seven classes the classifier produces, from docs/01-collectors.md §9.
INSERT INTO ref.data_class (class_code, category, description, default_severity, detector_family) VALUES
  ('payment_card',     'financial',    'Payment card numbers, validated by checksum rather than pattern alone.',        'critical', 'deterministic'),
  ('government_id',    'identity',     'National identifiers and tax numbers, per-jurisdiction rule sets.',            'critical', 'deterministic'),
  ('credential',       'secrets',      'Passwords, API keys, private key headers and tokens.',                         'critical', 'deterministic'),
  ('customer_pii',     'personal',     'Names, addresses, contact details and other customer-identifying text.',       'high',     'statistical'),
  ('source_code',      'intellectual', 'Proprietary source code and internal implementation detail.',                  'high',     'statistical'),
  ('legal_commercial', 'commercial',   'Contracts, negotiations and commercially sensitive language.',                 'medium',   'statistical'),
  ('health',           'special',      'Health information and anything suggesting a medical condition.',              'critical', 'statistical');

-- Route fidelity. Lower rank wins. Ranking is about the quality of what the route observed,
-- not how many tools it covers.
INSERT INTO ref.route_fidelity (source, fidelity_rank, yields_content, description) VALUES
  ('ext.page_context', 10, true,  'Extension reading the user-authored payload and attachment bytes in page context, before serialisation. The canonical source: it sees what the user composed, not what the transport did with it.'),
  ('cli.shim',         20, true,  'Call-site capture from a managed shell environment. Structured arguments rather than a parsed HTTP body, so no reconstruction is involved.'),
  ('proxy.loopback',   30, true,  'Local inference broker observing plaintext HTTP on the loopback interface. Nothing is decrypted and no framing is guessed, so fidelity is high; coverage is narrow.'),
  ('ext.web_request',  40, true,  'Extension reading the request body as sent. Accurate for the wire form, which may differ from the composed form in encoding and whitespace.'),
  ('proxy.tls',        50, true,  'Egress proxy terminating TLS and reading the HTTP body. Requires reconstructing the prompt from a provider-specific serialisation, so encoding differences can defeat canonicalisation.'),
  ('ext.dom',          60, true,  'DOM-derived observation, used where the request is not observable. Browser-agent UIs may render outside normal DOM structures, so this is best-effort.'),
  ('proc.detect',      70, false, 'Process and module observation only. Establishes that a model ran; by construction carries no content (usage mode I).');

-- Collection components. `modes_supported` records what each can achieve rather than what it
-- is configured to do, so coverage expectations are not derived from configuration.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('capture_extension', 'capture_extension', ARRAY['m0','m1','m2','m3'], 'Policy-installed Chromium extension. Usage modes A, B and best-effort C.'),
  ('egress_proxy',      'capture_core',      ARRAY['m0','m1','m2','m3'], 'Local TLS-terminating proxy. Usage modes D, E, G and H where the client honours system proxy settings.'),
  ('loopback_broker',   'capture_core',      ARRAY['m0','m1','m2','m3'], 'Local inference broker holding the well-known loopback ports. Usage mode F.'),
  ('cli_shim',          'capture_core',      ARRAY['m0','m1','m2','m3'], 'Managed shell profile and environment for proxy and trust. Coding agents and SDKs, usage modes E and G.'),
  ('process_detector',  'capture_core',      ARRAY['m0'],                'Process and loaded-module observation. Usage mode I, detection only: establishes that a model ran, never what was said to it.'),
  ('classifier_host',   'classifier_host',   ARRAY['m1','m2','m3'],      'Sandboxed classification host. Not a collection path; reported separately because brief C23 and C25 apply to it too.');

INSERT INTO ref.retention_class (retention_class, default_ttl_days, description) VALUES
  ('standard',  90,  'Default for event metadata.'),
  ('extended',  365, 'For events under an investigation or a longer contractual retention.'),
  ('content',   30,  'Default for stored content objects at M3. Short by default: the attachment tier is the only unbounded cost in the system (brief §3.1).'),
  ('quarantine', 14, 'Rejected envelopes, content-stripped. Long enough to diagnose a collector defect, short enough not to accumulate.'),
  ('legal_hold', 0,  'Placeholder class for rows whose expiry is suspended by ops.hold. Never selected for deletion; present so that a held row has an explicit class rather than a null.');

INSERT INTO ref.classifier_release (release_version, ruleset_version, model_version, artifact_digest, state, notes) VALUES
  ('2026.01.0-shadow', 'rules-2026.01.0', 'model-2026.01.0', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'shadow',
   'Seed row. Every new classifier release starts in shadow: labels are recorded but nothing is blocked, which is how brief §6''s "evaluated in a non-enforcing mode before they take effect" is satisfied. Replace with the real first release before any tenant is onboarded.');

-- Rule metadata for the rules the device classifier publishes today (endpoint/classifier-host's
-- dev ruleset and the lab device's rules file). Only a label naming one of these raises a finding:
-- mart.finding.rule_id is a foreign key here, and inventing a rule the classifier never published
-- would assert a detection that did not happen. The wording and severities match the dashboard's
-- own sample vocabulary (query/dashboard/src/explore-stub.js), so the seeded catalogue and the
-- preview agree. A tenant-specific ruleset is a later task (the policy-bundle writer).
INSERT INTO ref.rule (rule_id, class_code, detector_kind, severity, title, description, introduced_in) VALUES
  ('PCI_PAN_PATTERN',       'payment_card',     'deterministic', 'critical', 'Payment card number in prompt',   'A card-number-shaped run of digits whose Luhn checksum holds and which is not preceded by "test".',       '2026.01.0-shadow'),
  ('PAYMENT_CARD_PAN',      'payment_card',     'deterministic', 'critical', 'Payment card number in prompt',   'Card-number pattern validated by Luhn, as published by the endpoint classifier. Alias of the rule above under its wire id.', '2026.01.0-shadow'),
  ('SECRET_API_KEY',        'credential',       'deterministic', 'critical', 'API key or access token',         'A credential-shaped string: private-key headers and provider key prefixes.',                             '2026.01.0-shadow'),
  ('GOV_ID_NUMBER',         'government_id',    'deterministic', 'high',     'Government identifier',           'A national identifier or tax number matching a jurisdiction rule set.',                                   '2026.01.0-shadow'),
  ('PII_CUSTOMER_RECORD',   'customer_pii',     'deterministic', 'high',     'Customer personal data',          'Names, addresses or contact details identifying a customer.',                                             '2026.01.0-shadow'),
  ('SRC_INTERNAL_REPO',     'source_code',      'deterministic', 'high',     'Proprietary source code',         'Source declarations or repository detail indicating proprietary implementation.',                          '2026.01.0-shadow'),
  ('PHI_CLINICAL_TERM',     'health',           'deterministic', 'high',     'Health information',              'Clinical vocabulary and anything suggesting a medical condition.',                                        '2026.01.0-shadow'),
  ('LEGAL_CONTRACT_TERMS',  'legal_commercial', 'deterministic', 'medium',   'Contract or commercial terms',    'Contractual or commercially sensitive language.',                                                         '2026.01.0-shadow');

-- The seed tool catalogue (ref.tool_catalogue). Three groups:
--
--   * `tls` rows are hashes the egress proxy derives from a destination host and path. They are
--     listed one row per observed path because the fingerprint is over host+path: one tool (Claude
--     Code) legitimately has several. `evidence` carries the host and path so the row can be
--     regenerated and reviewed rather than being an unexplained hash.
--   * `process` rows are the process-detector's image-derived names (`proc_<signature>`) and the
--     local-inference fingerprints the loopback broker and the lab simulator emit.
--   * `extension` rows are the names the Chromium extension emits for a known browser tool.
--
-- Unknown fingerprints are deliberately absent. This seed is the common tools, not a claim to know
-- every destination; the read path renders an absent fingerprint as "Unrecognised tool".
INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence) VALUES
  ('tls_b6681b043244c43f', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/v1/messages"}'),
  ('tls_5a5a41ed0bf50d9d', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/v1/messages/count_tokens"}'),
  ('tls_69f6ab029df3c019', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/event_logging/v2/batch"}'),
  ('tls_31299ef7601928f7', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/event_logging/batch"}'),
  ('tls_cb53d2b2add3d450', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/claude_cli_profile"}'),
  ('tls_f32477ff734d70d1', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/chat/completions"}'),
  ('tls_4a602150609f427e', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/responses"}'),
  ('tls_15ab95c6f0615e12', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/models"}'),
  ('tls_8a9512eae8499418', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/completions"}'),
  ('tls_11574658dafb8805', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chatgpt.com","path":"/backend-api/conversation"}'),
  ('tls_fd863543bed5e1fd', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chatgpt.com","path":"/backend-api/chat/completions"}'),
  ('tls_2af2dd0ea445e033', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chat.openai.com","path":"/backend-api/conversation"}'),
  ('tls_f412811be7ac6539', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/chat/completions"}'),
  ('tls_336b980c5f15d4f0', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/v1/chat/completions"}'),
  ('tls_ba5b03450233e3fc', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/models"}'),
  ('tls_3a0703d7a0dc3a68', 'GitHub Copilot', 'github', 'tls', '{"host":"copilot.microsoft.com","path":"/chat/completions"}'),
  ('tls_7659f7a5e64cd885', 'Gemini (web)', 'google', 'tls', '{"host":"gemini.google.com","path":"/"}'),
  ('tls_0320fa08a4f64851', 'Gemini API', 'google', 'tls', '{"host":"generativelanguage.googleapis.com","path":"/v1beta/models"}'),
  ('tls_a40a04ef6e27de9f', 'Perplexity (web)', 'perplexity', 'tls', '{"host":"www.perplexity.ai","path":"/"}'),
  ('tls_aefb9cd055abfffb', 'Perplexity API', 'perplexity', 'tls', '{"host":"api.perplexity.ai","path":"/chat/completions"}'),
  ('tls_9230903190dcf0dc', 'Cursor', 'cursor', 'tls', '{"host":"api2.cursor.sh","path":"/"}'),
  ('tls_14a9086e088f0d82', 'Cursor', 'cursor', 'tls', '{"host":"api.cursor.com","path":"/"}'),
  ('tls_84160351478923e0', 'Mistral API', 'mistral', 'tls', '{"host":"api.mistral.ai","path":"/v1/chat/completions"}'),
  ('tls_41b7a7dd22bb3bae', 'Groq API', 'groq', 'tls', '{"host":"api.groq.com","path":"/openai/v1/chat/completions"}'),
  ('tls_823b80bdeef13f38', 'OpenRouter', 'openrouter', 'tls', '{"host":"openrouter.ai","path":"/api/v1/chat/completions"}'),
  ('tls_ce5fbce2b73c1868', 'DeepSeek API', 'deepseek', 'tls', '{"host":"api.deepseek.com","path":"/chat/completions"}'),
  ('tls_d829c7e9598888f9', 'xAI API', 'xai', 'tls', '{"host":"api.x.ai","path":"/v1/chat/completions"}'),
  ('tls_544b80dcc1446f8b', 'Together AI', 'together', 'tls', '{"host":"api.together.xyz","path":"/v1/chat/completions"}'),
  ('tls_4fb4f5cb98d7a1c3', 'Cohere API', 'cohere', 'tls', '{"host":"api.cohere.ai","path":"/v1/chat"}'),
  ('ollama_local',     'Ollama (local)',   'ollama',   'process',   '{"basis":"loopback port map","note":"lab and simulator fingerprint for a local Ollama runtime"}'),
  ('proc_ollama',      'Ollama (local)',   'ollama',   'process',   '{"image_signature":"ollama"}'),
  ('proc_lmstudio',    'LM Studio (local)','lmstudio', 'process',   '{"image_signature":"lm studio"}'),
  ('proc_llama',       'llama.cpp (local)','llama.cpp','process',   '{"image_signature":"llama.cpp"}'),
  ('proc_vllm',        'vLLM (local)',     'vllm',     'process',   '{"image_signature":"vllm"}'),
  ('chatgpt_web',      'ChatGPT (web)',    'openai',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('claude_web',       'Claude (web)',     'anthropic','extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('copilot_chat',     'GitHub Copilot',   'github',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('cursor_ide',       'Cursor',           'cursor',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('gemini_web',       'Gemini (web)',     'google',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('perplexity_web',   'Perplexity (web)', 'perplexity','extension','{"note":"simulator/legacy extension fingerprint"}'),
  ('genai.web.chat.v1:chatgpt', 'ChatGPT (web)', 'openai',   'extension', '{"note":"extension fingerprint, pre-tf1 protocol"}'),
  ('genai.web.chat.v1:claude',  'Claude (web)',  'anthropic','extension', '{"note":"extension fingerprint, pre-tf1 protocol"}'),
  ('genai.web.chat.v1:gemini',  'Gemini (web)',  'google',   'extension', '{"note":"extension fingerprint, pre-tf1 protocol"}');


-- =====================================================================================
-- 12. Closing notes
-- =====================================================================================
--
-- Things this file deliberately does not do, with the reason, so that the omissions are not
-- mistaken for oversights:
--
--   * No partitioning. Master doc D2 / ADR 0009. Tenant is the leading column of every primary
--     key and index, and every access path is expressed through a tenant-leading key, so
--     introducing monthly range partitioning on receive time later is a migration rather than
--     an application change. The trigger for doing so is a tenant exceeding 50M event rows.
--
--   * A content search index, and no attachment CONTENT in it. `ingest.search_text` holds prompt
--     bodies (for `full_text` tenants) and attachment filenames (for `attachment_names` and
--     above). Attachment bytes are still never indexed: that is the 50-500 GB/year tier in brief
--     §3.1 and a separate decision with its own cost and breach surface. ADR 0014 supersedes
--     ADR 0008, which refused content search entirely; brief §3.5 is satisfied by making the
--     choice per tenant and enforcing the incompatible combination as unrepresentable, rather
--     than by refusing the capability to everyone.
--
--   * No aggregate refresh logic here. The upsert statements live in the aggregator job
--     (aggregation/aggregator) and are specified in docs/04-dashboard-and-query.md §4. This file
--     provides the primary keys they conflict on, which is the part that has to be stable.
--
--   * No deletion of content objects from Blob Storage. The database records the intent and
--     the receipt; the reconciler performs it. Brief C34's second independent mechanism is a
--     blob lifecycle rule that removes ciphertext by age without consulting this database at
--     all, which is precisely why the two must be reconciled rather than trusted.
--
--   * No platform-level audit rows. Every row in ops.audit belongs to a tenant. Operations
--     that are genuinely not about any tenant (schema migrations, infrastructure changes) are
--     recorded in the platform log, not here, so that a tenant export of the audit trail is
--     complete and does not contain rows the tenant cannot interpret.
