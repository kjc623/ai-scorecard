-- In-place migration: the request kind on the device and its storage (task 08,
-- backlog/08-client-generated-requests). Applies the prompt_kind schema change to an existing
-- database without touching ingest data. Every step is additive or a drop-and-recreate of a
-- constraint, and the function is replaced in full; re-running is safe.
BEGIN;

-- 1. The device request kind on the observation and the merged submission. Nullable, so there is
--    no backfill: an existing row reads as unknown downstream. A submission kind is constrained
--    to a prompt, because no rollup or detection has one.
ALTER TABLE ingest.observation
  ADD COLUMN IF NOT EXISTS prompt_kind text CHECK (prompt_kind IN ('user','client_generated','unknown'));
ALTER TABLE ingest.submission
  ADD COLUMN IF NOT EXISTS prompt_kind text
    CHECK (prompt_kind IS NULL OR (prompt_kind IN ('user','client_generated','unknown') AND kind = 'prompt'));

COMMENT ON COLUMN ingest.observation.prompt_kind IS
  'The device request kind (task 08): user for text a person authored, client_generated for a request the client made for itself (titling, summarisation, telemetry, an injected message), unknown when the device could not decide. Decided on the device from request shape, never from meaning; defaults to user when unsure so nothing a person typed is hidden. Present only for kind=prompt at M1 and above: M0 read no body and the field is not in its closed list. NULL reads as unknown.';
COMMENT ON COLUMN ingest.submission.prompt_kind IS
  'The winning observation request kind (task 08), carried on the submission so the event list can filter without a semi-join on ingest.observation. NULL for the rollup and detection kinds and for a prompt folded from an M0 or pre-task-08 observation; the read layer treats NULL as unknown. content-vault does not build a search index for a client_generated submission, and Search hides them by default.';

-- 2. The mode/kind boundary constraints now include prompt_kind, so they are dropped and
--    recreated. The bodies match database/schema.sql exactly; db/tools/check-schema.mjs keeps
--    them equal to the contract forbid-lists.
ALTER TABLE ingest.observation DROP CONSTRAINT IF EXISTS observation_m0_carries_no_content;
ALTER TABLE ingest.observation ADD CONSTRAINT observation_m0_carries_no_content
  CHECK (collection_mode <> 'm0' OR (
    content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
    AND confidence IS NULL AND content_excerpt IS NULL AND prompt_kind IS NULL));

ALTER TABLE ingest.observation DROP CONSTRAINT IF EXISTS observation_rollup_shape;
ALTER TABLE ingest.observation ADD CONSTRAINT observation_rollup_shape
  CHECK (kind <> 'usage_rollup' OR (
    direction = 'none' AND window_start IS NOT NULL AND window_end IS NOT NULL
    AND submission_count IS NOT NULL AND bytes_total IS NOT NULL
    AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
    AND content_excerpt IS NULL AND policy_decision IS NULL AND size_bytes IS NULL
    AND detection_basis IS NULL AND prompt_kind IS NULL));

ALTER TABLE ingest.observation DROP CONSTRAINT IF EXISTS observation_detection_shape;
ALTER TABLE ingest.observation ADD CONSTRAINT observation_detection_shape
  CHECK (kind <> 'model_detection' OR (
    direction = 'none' AND detection_basis IS NOT NULL
    AND content_digest IS NULL AND labels IS NULL AND classifier_version IS NULL
    AND content_excerpt IS NULL AND policy_decision IS NULL AND size_bytes IS NULL
    AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL
    AND bytes_total IS NULL AND prompt_kind IS NULL));

-- 3. The single write path stores the request kind on the observation and carries the winning
--    observation kind onto the submission. CREATE OR REPLACE preserves the EXECUTE grants.

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
    p_envelope->>'user_ref',
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
     SET last_user_ref     = p_envelope->>'user_ref',
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
    p_envelope->>'user_ref',
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

COMMIT;
