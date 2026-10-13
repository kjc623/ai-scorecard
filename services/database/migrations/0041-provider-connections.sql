-- AI providers' own records of prompts, read from their admin APIs: the collection path that needs
-- no device. A tenant connects a provider (ops.provider_connection, one of ref.provider);
-- control-api reads the provider's record of each prompt the tenant's people sent, classifies it
-- and records it through ingest.record_provider_event, which runs as sac_recorder with exactly the
-- privileges record_event needs. An observation, a submission and an upload grant name their
-- origin: the device, or the provider connection. A provider's record of a prompt a device also
-- captured merges into the device's submission, so the prompt counts once.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'sac_recorder') THEN
    CREATE ROLE sac_recorder NOLOGIN;
  END IF;
END $$;
GRANT USAGE ON SCHEMA ref, ops, ingest TO sac_recorder;

CREATE TABLE IF NOT EXISTS ref.provider (
  provider      text PRIMARY KEY CHECK (provider ~ '^[a-z][a-z0-9_]{1,63}$'),
  display_name  text NOT NULL,
  vendor        text NOT NULL,
  credential    text NOT NULL CHECK (credential IN ('entra_app','api_key')),
  source_url    text NOT NULL
);

INSERT INTO ref.provider (provider, display_name, vendor, credential, source_url) VALUES
  ('microsoft_365_copilot', 'Microsoft 365 Copilot', 'microsoft', 'entra_app',
   'https://learn.microsoft.com/en-us/microsoft-365/copilot/extensibility/api/ai-services/interaction-export/aiinteractionhistory-getallenterpriseinteractions'),
  ('chatgpt_enterprise', 'ChatGPT Enterprise', 'openai', 'api_key',
   'https://cookbook.openai.com/examples/chatgpt/compliance_api/logs_platform'),
  ('claude_enterprise', 'Claude Enterprise', 'anthropic', 'api_key',
   'https://platform.claude.com/docs/en/manage-claude/compliance-api')
ON CONFLICT (provider) DO NOTHING;

INSERT INTO ref.route_fidelity (source, fidelity_rank, yields_content, description) VALUES
  ('provider.api',     35, true,  'The provider''s own record of the prompt, read from its admin API under the tenant''s consent (ops.provider_connection). The text the provider received, exactly; it does not see what the device composed, decided or attached.')
ON CONFLICT (source) DO NOTHING;

CREATE TABLE IF NOT EXISTS ops.provider_connection (
  tenant_id          uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  connection_id      uuid NOT NULL DEFAULT gen_random_uuid(),
  provider           text NOT NULL REFERENCES ref.provider(provider),
  -- The provider-side account the connection reads, where the credential alone does not name it:
  -- a ChatGPT workspace, an Anthropic organization. A Microsoft connection reads the directory of
  -- the tenant's Entra connection.
  account            text CONSTRAINT provider_connection_account_bounded CHECK (length(account) BETWEEN 1 AND 200),
  -- The key the customer's admin issued, sealed under the tenant's directory key. NULL for a
  -- provider read with the vendor's Entra application, which holds no secret.
  credential_enc     bytea,
  status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  -- Where the next pass resumes, as the provider's adapter spells it; NULL before the first pass.
  cursor             text CONSTRAINT provider_connection_cursor_bounded CHECK (length(cursor) <= 4096),
  created_by         text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         text NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  last_started_at    timestamptz,
  last_completed_at  timestamptz,
  last_status        text CHECK (last_status IN ('ok','failed')),
  last_error         text CONSTRAINT provider_connection_error_is_code CHECK (last_error ~ '^[a-z_]{1,64}$'),
  -- Records the passes have read, and those among them the tenant did not already hold.
  records_read       bigint NOT NULL DEFAULT 0 CHECK (records_read >= 0),
  records_new        bigint NOT NULL DEFAULT 0 CHECK (records_new >= 0),
  PRIMARY KEY (tenant_id, connection_id),
  UNIQUE (tenant_id, provider),
  CONSTRAINT provider_connection_failure_has_code CHECK ((last_status = 'failed') = (last_error IS NOT NULL))
);

ALTER TABLE ops.provider_connection ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.provider_connection FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.provider_connection;
CREATE POLICY tenant_isolation ON ops.provider_connection
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

-- The origin of an observation, a submission and an upload grant: exactly one of a device and a
-- provider connection.
ALTER TABLE ops.grant ALTER COLUMN device_id DROP NOT NULL;
ALTER TABLE ops.grant ADD COLUMN IF NOT EXISTS connection_id uuid;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ops.grant'::regclass AND conname = 'grant_tenant_id_connection_id_fkey') THEN
    ALTER TABLE ops.grant ADD CONSTRAINT grant_tenant_id_connection_id_fkey FOREIGN KEY (tenant_id, connection_id) REFERENCES ops.provider_connection(tenant_id, connection_id);
  END IF;
END $$;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ops.grant'::regclass AND conname = 'grant_has_one_origin') THEN
    ALTER TABLE ops.grant ADD CONSTRAINT grant_has_one_origin CHECK ((device_id IS NULL) <> (connection_id IS NULL));
  END IF;
END $$;

ALTER TABLE ingest.observation ALTER COLUMN device_id DROP NOT NULL;
ALTER TABLE ingest.observation ADD COLUMN IF NOT EXISTS connection_id uuid;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ingest.observation'::regclass AND conname = 'observation_tenant_id_connection_id_fkey') THEN
    ALTER TABLE ingest.observation ADD CONSTRAINT observation_tenant_id_connection_id_fkey FOREIGN KEY (tenant_id, connection_id) REFERENCES ops.provider_connection(tenant_id, connection_id);
  END IF;
END $$;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ingest.observation'::regclass AND conname = 'observation_has_one_origin') THEN
    ALTER TABLE ingest.observation ADD CONSTRAINT observation_has_one_origin CHECK ((device_id IS NULL) <> (connection_id IS NULL));
  END IF;
END $$;

ALTER TABLE ingest.submission ALTER COLUMN device_id DROP NOT NULL;
ALTER TABLE ingest.submission ADD COLUMN IF NOT EXISTS connection_id uuid;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ingest.submission'::regclass AND conname = 'submission_tenant_id_connection_id_fkey') THEN
    ALTER TABLE ingest.submission ADD CONSTRAINT submission_tenant_id_connection_id_fkey FOREIGN KEY (tenant_id, connection_id) REFERENCES ops.provider_connection(tenant_id, connection_id);
  END IF;
END $$;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ingest.submission'::regclass AND conname = 'submission_has_one_origin') THEN
    ALTER TABLE ingest.submission ADD CONSTRAINT submission_has_one_origin CHECK ((device_id IS NULL) <> (connection_id IS NULL));
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS submission_by_digest ON ingest.submission (tenant_id, content_digest)
  WHERE content_digest IS NOT NULL;

CREATE OR REPLACE FUNCTION ingest.record_event(p_envelope jsonb, p_received_at timestamptz)
RETURNS TABLE(event_outcome text, event_submission_id uuid)
LANGUAGE plpgsql AS $$
DECLARE
  v_tenant        uuid := (p_envelope->>'tenant_id')::uuid;
  v_event_id      uuid := (p_envelope->>'event_id')::uuid;
  v_kind          text := p_envelope->>'kind';
  v_mode          text := p_envelope->>'collection_mode';
  v_source        text := p_envelope->>'source';
  -- The observation's origin: the device that made it, or the provider connection whose record it
  -- is (ingest.record_provider_event).
  v_device        uuid := (p_envelope->>'device_id')::uuid;
  v_connection    uuid := (p_envelope->>'connection_id')::uuid;
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
  v_search        text;
  v_outcome       text;
  v_attachments   int := CASE WHEN jsonb_typeof(p_envelope->'attachments') = 'array'
                              THEN jsonb_array_length(p_envelope->'attachments') END;
BEGIN
  SELECT f.fidelity_rank INTO v_fidelity FROM ref.route_fidelity f WHERE f.source = v_source;
  IF v_fidelity IS NULL THEN
    RAISE EXCEPTION 'unknown route %', v_source;
  END IF;
  -- A provider connection is an origin only record_provider_event vouches for.
  IF v_connection IS NOT NULL AND current_user <> 'sac_recorder' THEN
    RAISE EXCEPTION 'a provider''s record is recorded only through ingest.record_provider_event';
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
  v_weak     := ingest.weak_dedup_key(v_tenant, coalesce(v_device, v_connection),
                                      p_envelope->>'tool_fingerprint', v_kind, v_occurred,
                                      (p_envelope->>'size_bytes')::bigint);

  -- The clear account name is stored only for a tenant whose device_identity is 'clear'; a device
  -- that still sends one to a 'hashed' tenant has it dropped here.
  SELECT CASE WHEN t.device_identity = 'clear' THEN nullif(p_envelope->>'subject_name', '') END,
         t.content_search
    INTO v_subject_name, v_search
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
    activity_type, model, input_tokens, output_tokens, duration_ms, tool_name, outcome, connection_id
  )
  VALUES (
    v_tenant,
    v_event_id,
    v_device,
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
    p_envelope->>'outcome',
    v_connection
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
  -- it backwards. A provider's record has no device.
  IF v_device IS NOT NULL THEN
    UPDATE ops.device
       SET last_user_ref     = v_user_ref,
           last_subject_name = v_subject_name
     WHERE tenant_id = v_tenant
       AND device_id = v_device
       AND (last_seen_at IS NULL OR last_seen_at <= p_received_at);
  END IF;

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
           expires_at         = greatest(s.expires_at, p_received_at + make_interval(days => v_ttl)),
           -- A route that described no attachments does not erase a count another route supplied.
           attachment_count   = CASE WHEN v_fidelity < s.winning_fidelity
                                     THEN coalesce(v_attachments, s.attachment_count)
                                     ELSE coalesce(s.attachment_count, v_attachments) END
     WHERE s.tenant_id = v_tenant AND s.submission_id = v_sub_id;
    v_outcome := 'merged';
  ELSE
    INSERT INTO ingest.submission (
      tenant_id, submission_id, dedup_key, dedup_weak_key, kind, prompt_kind, device_id, user_ref, subject_name,
      tool_fingerprint, first_occurred_at, last_occurred_at, received_at, collection_mode, size_bytes,
      content_digest, labels, classifier_version, confidence, policy_action, policy_rule_id,
      decided_locally, winning_source, winning_fidelity, observed_routes, observation_count,
      merge_confidence, content_state, expires_at, attachment_count, connection_id
    )
    VALUES (
      v_tenant,
      gen_random_uuid(),
      CASE WHEN v_is_exact THEN v_exact END,
      v_weak,
      v_kind,
      nullif(p_envelope->>'prompt_kind', ''),
      v_device,
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
      p_received_at + make_interval(days => v_ttl),
      v_attachments,
      v_connection
    )
    RETURNING ingest.submission.submission_id INTO v_sub_id;
    v_outcome := 'inserted';
  END IF;

  -- Step 3: index the prompt's attachment filenames at the attachment_names tier and above, each by
  -- the path the device saw it at when it saw one, so a search finds a file by its folder too. No
  -- other path carries them: the content a device uploads at M3 is the prompt text. A file already
  -- indexed at a position of the submission keeps it.
  IF v_search IN ('attachment_names', 'full_text') AND v_kind = 'prompt'
     AND p_envelope->>'prompt_kind' IS DISTINCT FROM 'client_generated'
     AND jsonb_typeof(p_envelope->'attachments') = 'array' THEN
    INSERT INTO ingest.search_text (tenant_id, submission_id, unit_kind, unit_index, body, expires_at)
    SELECT v_tenant, v_sub_id, 'attachment_name', f.ord - 1, f.body,
           p_received_at + make_interval(days => v_ttl)
      FROM (SELECT a.ord, coalesce(nullif(a.value->>'path', ''), a.value->>'name') AS body
              FROM jsonb_array_elements(p_envelope->'attachments') WITH ORDINALITY AS a(value, ord)) AS f
     WHERE length(f.body) BETWEEN 1 AND 65536
    ON CONFLICT DO NOTHING;
  END IF;

  RETURN QUERY SELECT v_outcome, v_sub_id;
END $$;

-- sac_recorder holds what record_event reads and writes, as ingest-api does, and what
-- record_provider_event adds: the connection check and the tool lookup that pairs a record with a
-- device's capture.
GRANT SELECT ON ref.data_class, ref.route_fidelity, ref.retention_class, ref.tool_catalogue,
      ref.app_signal TO sac_recorder;
GRANT SELECT (tenant_id, device_identity, content_search) ON ops.tenant TO sac_recorder;
GRANT SELECT ON ops.retention_policy, ops.user_ref_alias TO sac_recorder;
GRANT SELECT (tenant_id, connection_id, status) ON ops.provider_connection TO sac_recorder;
GRANT SELECT, INSERT ON ingest.observation TO sac_recorder;
GRANT SELECT, INSERT, UPDATE ON ingest.submission TO sac_recorder;
GRANT INSERT ON ingest.search_text TO sac_recorder;

-- The function is created, or replaced, as its owner sac_recorder, which holds the membership and
-- the CREATE on ingest only while it does.
GRANT sac_recorder TO CURRENT_USER;
GRANT CREATE ON SCHEMA ingest TO sac_recorder;
SET ROLE sac_recorder;
CREATE OR REPLACE FUNCTION ingest.record_provider_event(p_envelope jsonb)
RETURNS TABLE(event_outcome text, event_submission_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  v_tenant    uuid := (p_envelope->>'tenant_id')::uuid;
  v_occurred  timestamptz := (p_envelope->>'occurred_at')::timestamptz;
  v_user_ref  text;
  v_key       text;
BEGIN
  IF v_tenant IS DISTINCT FROM ops.current_tenant() THEN
    RAISE EXCEPTION 'a provider record must be for the session tenant';
  END IF;
  IF p_envelope ? 'device_id' OR p_envelope->>'source' IS DISTINCT FROM 'provider.api' THEN
    RAISE EXCEPTION 'a provider record has no device and arrives by provider.api';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM ops.provider_connection c
                  WHERE c.tenant_id = v_tenant
                    AND c.connection_id = (p_envelope->>'connection_id')::uuid
                    AND c.status = 'active') THEN
    RAISE EXCEPTION 'provider connection % is not an active connection of the tenant',
      p_envelope->>'connection_id';
  END IF;

  IF p_envelope->>'kind' = 'prompt' AND p_envelope->>'content_digest' IS NOT NULL THEN
    v_user_ref := coalesce(
      (SELECT a.user_ref FROM ops.user_ref_alias a
        WHERE a.tenant_id = v_tenant AND a.alias_ref = p_envelope->>'user_ref'),
      p_envelope->>'user_ref');
    SELECT s.dedup_key INTO v_key
      FROM ingest.submission s
     WHERE s.tenant_id = v_tenant
       AND s.content_digest = p_envelope->>'content_digest'
       AND s.kind = 'prompt'
       AND s.device_id IS NOT NULL
       AND s.dedup_key IS NOT NULL
       AND s.user_ref = v_user_ref
       AND ops.tool_id(s.tool_fingerprint) = ops.tool_id(p_envelope->>'tool_fingerprint')
       AND s.first_occurred_at BETWEEN v_occurred - interval '10 minutes'
                                   AND v_occurred + interval '10 minutes'
     ORDER BY abs(extract(epoch FROM s.first_occurred_at - v_occurred))
     LIMIT 1;
    IF v_key IS NOT NULL THEN
      p_envelope := jsonb_set(p_envelope, '{dedup_key}', to_jsonb(v_key));
    END IF;
  END IF;

  RETURN QUERY SELECT r.event_outcome, r.event_submission_id
                 FROM ingest.record_event(p_envelope, now()) AS r;
END $$;
REVOKE EXECUTE ON FUNCTION ingest.record_provider_event(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ingest.record_provider_event(jsonb) TO sac_control;
RESET ROLE;
REVOKE CREATE ON SCHEMA ingest FROM sac_recorder;
REVOKE sac_recorder FROM CURRENT_USER;

-- control-api connects providers and records their records; query-api names a record's provider.
GRANT SELECT, INSERT, UPDATE ON ops.provider_connection TO sac_control;
GRANT SELECT ON ref.provider TO sac_control;
GRANT SELECT (connection_id) ON ingest.observation TO sac_control;
GRANT SELECT (connection_id) ON ingest.submission TO sac_control;
GRANT SELECT (tenant_id, connection_id, provider) ON ops.provider_connection TO sac_query;
GRANT SELECT ON ref.provider TO sac_query;
