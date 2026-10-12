-- Attachment filenames reach Search, and each person's usage counts the files they attached.
--
-- record_event indexes a prompt's attachment filenames from the envelope into ingest.search_text at
-- the attachment_names search tier and above, each by its path where the device saw one; ingest-api
-- may insert into the index but not read it.
-- The per-person aggregate sums the attachments of a person's prompts beside the prompts able to
-- describe one, as the per-tool aggregate does, and is filled from the submissions already recorded.
-- Filenames recorded before this change were never stored, so they cannot be indexed.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema. It runs as the tables' owner, which forced row-level security
-- binds like any other role, so it lifts FORCE for the fill and restores it.

ALTER TABLE mart.agg_user_period
  ADD COLUMN IF NOT EXISTS attachments     bigint NOT NULL DEFAULT 0 CHECK (attachments >= 0),
  ADD COLUMN IF NOT EXISTS content_prompts bigint NOT NULL DEFAULT 0 CHECK (content_prompts >= 0);

ALTER TABLE ingest.submission NO FORCE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_user_period NO FORCE ROW LEVEL SECURITY;

-- The prompts once per bucket size, in the bucket the rollup counts each in.
UPDATE mart.agg_user_period u
   SET attachments = p.attachments, content_prompts = p.content_prompts
  FROM (SELECT s.tenant_id, b.bucket_size, date_trunc(b.bucket_size, s.received_at) AS bucket_start,
               s.user_ref,
               coalesce(sum(s.attachment_count), 0)::bigint AS attachments,
               count(*) FILTER (WHERE s.collection_mode <> 'm0') AS content_prompts
          FROM ingest.submission s
         CROSS JOIN (VALUES ('hour'), ('day')) AS b(bucket_size)
         WHERE s.kind = 'prompt'
         GROUP BY 1, 2, 3, 4) p
 WHERE u.tenant_id = p.tenant_id AND u.bucket_size = p.bucket_size
   AND u.bucket_start = p.bucket_start AND u.user_ref = p.user_ref;

ALTER TABLE ingest.submission FORCE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_user_period FORCE ROW LEVEL SECURITY;

GRANT INSERT ON ingest.search_text TO sac_ingest;

-- record_event indexes attachment filenames. The body is schema.sql's, verbatim.
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
  v_search        text;
  v_outcome       text;
  v_attachments   int := CASE WHEN jsonb_typeof(p_envelope->'attachments') = 'array'
                              THEN jsonb_array_length(p_envelope->'attachments') END;
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
      merge_confidence, content_state, expires_at, attachment_count
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
      p_received_at + make_interval(days => v_ttl),
      v_attachments
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

GRANT EXECUTE ON FUNCTION ingest.record_event(jsonb, timestamptz) TO sac_ingest;
