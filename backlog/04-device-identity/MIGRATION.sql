-- In-place migration: device identity, hostname and the submitting user (backlog/04-device-identity).
-- Applies the ADR 0021 schema change to an existing database without touching ingest data.
-- Every step is additive or a drop-and-recreate of a derived object; re-running is safe.
BEGIN;

-- 1. The per-tenant identity setting. Existing tenants default to 'clear', which is the product
--    default the owner chose; a tenant that wants hashed sets it after the migration.
ALTER TABLE ops.tenant
  ADD COLUMN IF NOT EXISTS device_identity text NOT NULL DEFAULT 'clear'
    CHECK (device_identity IN ('clear','hashed'));

-- 2. Device identity fields. All nullable, so no backfill is needed; the next heartbeat/enrolment
--    fills them. hostname_hash was already nullable and unused.
ALTER TABLE ops.device
  ADD COLUMN IF NOT EXISTS hostname         text,
  ADD COLUMN IF NOT EXISTS agent_version    text,
  ADD COLUMN IF NOT EXISTS collection_mode  text CHECK (collection_mode IN ('m0','m1','m2','m3')),
  ADD COLUMN IF NOT EXISTS last_user_ref    text,
  ADD COLUMN IF NOT EXISTS last_subject_name text;

COMMENT ON COLUMN ops.device.hostname IS
  'The clear machine name, when the tenant''s device_identity is ''clear''. NULL when the setting is ''hashed'', in which case hostname_hash carries the device-supplied hash and no clear name is stored. The reversal of the earlier hash-only design is recorded in backlog/04-device-identity/DECISIONS.md and the accompanying ADR.';
COMMENT ON COLUMN ops.device.collection_mode IS
  'The effective base collection mode the device resolved from the signed bundle for its own scope: the device override when the bundle names this device, otherwise the tenant default. Per-tool, per-population and per-class modes can be narrower and are not reflected here; this is the blanket coverage the device applies where no narrower scope does (DECISIONS.md D4).';
COMMENT ON COLUMN ops.device.last_subject_name IS
  'The clear account name from the device''s most recent submission, present only while device_identity is ''clear''. It exists so the Devices list can name the user as-of the last submission without a subject join on every read; it is the reason the Devices read is subject-level and audited (docs/04 section 5.2).';

-- 3. The clear account name on the observation and the merged submission.
ALTER TABLE ingest.observation ADD COLUMN IF NOT EXISTS subject_name text;
ALTER TABLE ingest.submission  ADD COLUMN IF NOT EXISTS subject_name text;

-- 4. The device read view gains the identity fields. It is derived and rebuildable, so it is
--    recreated rather than altered.
DROP VIEW IF EXISTS mart.v_device_liveness;
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
GRANT SELECT ON mart.v_device_liveness TO sac_query;
COMMENT ON VIEW mart.v_device_liveness IS
  'Turns an absence of events into an explicit state. `stale` and `never_reported` are different facts and are not merged, and neither is `revoked`: a device that was deliberately removed is not a device that has gone quiet. It also carries the device identity fields the Devices page shows (hostname, agent_version, collection_mode, managed_state) and the most-recent user (last_user_ref, last_subject_name), which makes a read of it subject-level: docs/04 §5.2 audits any query that returns a subject reference, so this view''s reads are audited as served.';

-- 5. The single write path now stores the gated subject_name and maintains the device's most recent
--    user. CREATE OR REPLACE preserves the EXECUTE grants.
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
    dedup_key, schema_version, expires_at
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
    tenant_id, submission_id, dedup_key, dedup_weak_key, kind, device_id, user_ref, subject_name,
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

-- 6. The ingest role may stamp the two new activity columns, still column-level and one-way.
GRANT UPDATE (last_seen_at, last_user_ref, last_subject_name) ON ops.device TO sac_ingest;

COMMIT;
