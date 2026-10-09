-- The endpoint kinds. `discovery` replaces `model_detection`, `agent_activity` is new, and
-- ingest.observation gains both kinds' fields, with the per-kind CHECKs that mirror the envelope
-- contract. The new routes get their fidelity ranks.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: columns are added only when missing, and each changed CHECK
-- is dropped when present and added again.

-- Nothing emits model_detection, so no row is converted; a row that exists stops the migration.
-- Row-level security is forced on these tables, so the owner lifts it for the check and restores it.
ALTER TABLE ingest.observation NO FORCE ROW LEVEL SECURITY;
ALTER TABLE ingest.submission NO FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM ingest.observation WHERE kind = 'model_detection')
     OR EXISTS (SELECT 1 FROM ingest.submission WHERE kind = 'model_detection') THEN
    RAISE EXCEPTION 'ingest holds model_detection rows, which this migration does not convert';
  END IF;
END $$;
ALTER TABLE ingest.observation FORCE ROW LEVEL SECURITY;
ALTER TABLE ingest.submission FORCE ROW LEVEL SECURITY;

ALTER TABLE ingest.observation
  ADD COLUMN IF NOT EXISTS discovery_type   text,
  ADD COLUMN IF NOT EXISTS app_version      text,
  ADD COLUMN IF NOT EXISTS publisher        text,
  ADD COLUMN IF NOT EXISTS host_app         text,
  ADD COLUMN IF NOT EXISTS destination_host text,
  ADD COLUMN IF NOT EXISTS model_names      text[],
  ADD COLUMN IF NOT EXISTS activity_type    text,
  ADD COLUMN IF NOT EXISTS model            text,
  ADD COLUMN IF NOT EXISTS input_tokens     bigint,
  ADD COLUMN IF NOT EXISTS output_tokens    bigint,
  ADD COLUMN IF NOT EXISTS duration_ms      bigint,
  ADD COLUMN IF NOT EXISTS tool_name        text,
  ADD COLUMN IF NOT EXISTS outcome          text;

ALTER TABLE ingest.observation
  DROP CONSTRAINT IF EXISTS observation_kind_check,
  DROP CONSTRAINT IF EXISTS observation_detection_basis_check,
  DROP CONSTRAINT IF EXISTS observation_discovery_type_check,
  DROP CONSTRAINT IF EXISTS observation_activity_type_check,
  DROP CONSTRAINT IF EXISTS observation_input_tokens_check,
  DROP CONSTRAINT IF EXISTS observation_output_tokens_check,
  DROP CONSTRAINT IF EXISTS observation_duration_ms_check,
  DROP CONSTRAINT IF EXISTS observation_outcome_check,
  DROP CONSTRAINT IF EXISTS observation_prompt_shape,
  DROP CONSTRAINT IF EXISTS observation_rollup_shape,
  DROP CONSTRAINT IF EXISTS observation_detection_shape,
  DROP CONSTRAINT IF EXISTS observation_discovery_shape,
  DROP CONSTRAINT IF EXISTS observation_activity_shape;

ALTER TABLE ingest.observation
  ADD CONSTRAINT observation_kind_check
    CHECK (kind IN ('prompt','usage_rollup','discovery','agent_activity')),
  ADD CONSTRAINT observation_detection_basis_check
    CHECK (detection_basis IN ('installed_scan','package_scan','extension_scan','process_event','model_store','port_listen','flow_metadata')),
  ADD CONSTRAINT observation_discovery_type_check
    CHECK (discovery_type IN ('app_installed','app_running','cli_installed','ide_extension','local_model','inference_connection')),
  ADD CONSTRAINT observation_activity_type_check
    CHECK (activity_type IN ('model_request','tool_call')),
  ADD CONSTRAINT observation_input_tokens_check CHECK (input_tokens >= 0),
  ADD CONSTRAINT observation_output_tokens_check CHECK (output_tokens >= 0),
  ADD CONSTRAINT observation_duration_ms_check CHECK (duration_ms >= 0),
  ADD CONSTRAINT observation_outcome_check
    CHECK (outcome IN ('success','error','denied')),
  ADD CONSTRAINT observation_prompt_shape
    CHECK (kind <> 'prompt' OR (
      direction = 'egress' AND size_bytes IS NOT NULL AND policy_decision IS NOT NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL AND detection_basis IS NULL
      AND discovery_type IS NULL AND app_version IS NULL AND publisher IS NULL
      AND host_app IS NULL AND destination_host IS NULL AND model_names IS NULL
      AND activity_type IS NULL AND model IS NULL AND input_tokens IS NULL
      AND output_tokens IS NULL AND duration_ms IS NULL AND tool_name IS NULL AND outcome IS NULL)),
  ADD CONSTRAINT observation_rollup_shape
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
  ADD CONSTRAINT observation_discovery_shape
    CHECK (kind <> 'discovery' OR (
      direction = 'none' AND discovery_type IS NOT NULL AND detection_basis IS NOT NULL
      AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND content_excerpt IS NULL AND confidence IS NULL AND prompt_kind IS NULL
      AND policy_decision IS NULL AND size_bytes IS NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL
      AND activity_type IS NULL AND model IS NULL AND input_tokens IS NULL
      AND output_tokens IS NULL AND duration_ms IS NULL AND tool_name IS NULL AND outcome IS NULL)),
  ADD CONSTRAINT observation_activity_shape
    CHECK (kind <> 'agent_activity' OR (
      direction = 'none' AND activity_type IS NOT NULL
      AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
      AND content_excerpt IS NULL AND confidence IS NULL AND prompt_kind IS NULL
      AND policy_decision IS NULL
      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
      AND bytes_total IS NULL AND detection_basis IS NULL
      AND discovery_type IS NULL AND app_version IS NULL AND publisher IS NULL
      AND host_app IS NULL AND destination_host IS NULL AND model_names IS NULL));

ALTER TABLE ingest.submission
  DROP CONSTRAINT IF EXISTS submission_kind_check,
  ADD CONSTRAINT submission_kind_check
    CHECK (kind IN ('prompt','usage_rollup','discovery','agent_activity'));

-- record_event copies the new fields into the observation. The body is schema.sql's, verbatim.
DROP FUNCTION IF EXISTS ingest.record_event(jsonb, timestamptz);

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

GRANT EXECUTE ON FUNCTION ingest.record_event(jsonb, timestamptz) TO sac_ingest;

INSERT INTO ref.route_fidelity (source, fidelity_rank, yields_content, description) VALUES
  ('tool.hook',         5, true,  'The tool''s own hook, handed the prompt by the tool itself before it is sent. Exactly what the user submitted, with no reconstruction.'),
  ('tool.otel',        15, true,  'The tool''s own OpenTelemetry export, received on the device. Structured by the tool; prompt text is present only when the tool is configured to export it.'),
  ('inv.scan',         75, false, 'Inventory of installed applications, CLIs, IDE extensions and local models. Establishes that a tool is present; carries no content.'),
  ('net.flow',         80, false, 'Connection metadata only: the host a process connected to. Establishes that a tool was used; carries no content.')
ON CONFLICT (source) DO NOTHING;
