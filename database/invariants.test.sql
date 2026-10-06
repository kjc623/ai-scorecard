-- =====================================================================================
-- Shadow AI Capture: schema invariant tests
-- =====================================================================================
-- Run by database/tools/test-database.mjs, with psql, against a database the migrator has just
-- built:
--
--   psql -v ON_ERROR_STOP=1 -f database/invariants.test.sql
--
-- Every block prints PASS or raises, and ON_ERROR_STOP turns a raise into a failed run. The session
-- is a superuser so it can write fixtures, but every assertion about isolation or privilege runs as
-- a runtime role (SET ROLE): a superuser bypasses row-level security and would prove nothing.
-- =====================================================================================

\set ON_ERROR_STOP on
\set QUIET on
SET client_min_messages = notice;

-- Terms used throughout.
\set ta '''11111111-1111-7111-8111-111111111111'''
\set tb '''22222222-2222-7222-8222-222222222222'''
\set dev_a '''aaaaaaaa-0000-7000-8000-000000000001'''
\set dev_b '''bbbbbbbb-0000-7000-8000-000000000002'''

-- =====================================================================================
-- Fixtures
-- =====================================================================================

INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode, content_budget_bytes_per_day)
VALUES
  (:ta, 'Tenant A (M3 ceiling)', 'active', 'eastus', 'm3', 107374182400),
  (:tb, 'Tenant B (M1 ceiling)', 'active', 'eastus', 'm1', 0);

INSERT INTO ops.device (tenant_id, device_id, os, managed_state)
VALUES (:ta, :dev_a, 'windows', 'managed'),
       (:tb, :dev_b, 'macos', 'managed');

INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, public_key_thumbprint, expires_at)
VALUES (:ta, gen_random_uuid(), :dev_a, 'thumb-a', now() + interval '30 days'),
       (:tb, gen_random_uuid(), :dev_b, 'thumb-b', now() + interval '30 days');

-- A retention policy that differs from the standard class, so materialisation is observable.
INSERT INTO ops.retention_policy (tenant_id, applies_to, data_class, collection_mode, ttl_days, updated_by)
VALUES (:ta, 'event', 'payment_card', 'm1', 30, 'test');

DO $$ BEGIN RAISE NOTICE 'fixtures loaded'; END $$;


-- =====================================================================================
-- T1-T4  Policy ceiling
-- =====================================================================================
-- A policy bundle can never exceed its tenant's ceiling (ops.enforce_policy_ceiling).

DO $$
BEGIN
  INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix,
                                 retention_class, signature_kid, signed_digest, created_by)
  VALUES ('11111111-1111-7111-8111-111111111111', 1,
          '{"tools": {"chatgpt": "m2", "claude": "m1"}, "default": "m1"}'::jsonb,
          'standard', 'kid-test', 'sha256:' || repeat('cd', 32), 'test');
  RAISE NOTICE 'PASS T1 bundle within ceiling accepted';
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix,
                                   retention_class, signature_kid, signed_digest, created_by)
    VALUES ('22222222-2222-7222-8222-222222222222', 1,
            '{"tools": {"chatgpt": "m3"}, "default": "m1"}'::jsonb,
            'standard', 'kid-test', 'sha256:' || repeat('ce', 32), 'test');
    RAISE EXCEPTION 'FAIL T2 bundle exceeding the M1 ceiling was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T2 bundle exceeding ceiling rejected (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix,
                                   retention_class, signature_kid, signed_digest, created_by)
    VALUES ('11111111-1111-7111-8111-111111111111', 2,
            '{"tools": {"chatgpt": "m9"}, "default": "m1"}'::jsonb,
            'standard', 'kid-test', 'sha256:' || repeat('cf', 32), 'test');
    RAISE EXCEPTION 'FAIL T3 bundle with an unrecognised mode was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T3 unrecognised mode rejected (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix,
                                   retention_class, signature_kid, signed_digest, created_by)
    VALUES ('11111111-1111-7111-8111-111111111111', 3, '{}'::jsonb,
            'standard', 'kid-test', 'sha256:' || repeat('d0', 32), 'test');
    RAISE EXCEPTION 'FAIL T4 empty scope matrix was accepted as unrestricted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T4 empty scope matrix rejected (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T5-T9  Idempotency and the fidelity tie-break
-- =====================================================================================
-- Three observations of one submission: a low-fidelity route, the same event replayed, then a
-- high-fidelity route. Expected: two observation rows, one submission upgraded to the better route,
-- and the replay reported as a duplicate. Runs as sac_ingest, the role ingest-api uses.

SET ROLE sac_ingest;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  r record;
  n int;
BEGIN
  -- Route 1: the egress proxy, rank 50.
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version', '1.0',
    'event_id', 'e0000000-0000-7000-8000-000000000001',
    'tenant_id', '11111111-1111-7111-8111-111111111111',
    'device_id', 'aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref', 'u_test',
    'tool_fingerprint', 'genai.web.chat.v1:chatgpt',
    'direction', 'egress',
    'kind', 'prompt',
    'occurred_at', '2026-10-02T14:31:07.221Z',
    'monotonic_offset_ms', 184420,
    'source', 'proxy.tls',
    'collection_mode', 'm1',
    'dedup_key', 'sha256:' || repeat('11', 32),
    'size_bytes', 4096,
    'content_digest', 'sha256:' || repeat('22', 32),
    'labels', jsonb_build_array(jsonb_build_object('class', 'payment_card', 'score', 0.97)),
    'classifier_version', 'test-2026.01',
    'confidence', 'high',
    'policy_decision', jsonb_build_object('rule_id', 'POL-1', 'action', 'warned', 'decided_locally', true)
  ), now());

  IF r.event_outcome <> 'inserted' THEN
    RAISE EXCEPTION 'FAIL T5 expected inserted, got %', r.event_outcome;
  END IF;
  RAISE NOTICE 'PASS T5 first observation recorded as inserted';

  -- Route 1 replayed verbatim: a retry costs nothing.
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version', '1.0',
    'event_id', 'e0000000-0000-7000-8000-000000000001',
    'tenant_id', '11111111-1111-7111-8111-111111111111',
    'device_id', 'aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref', 'u_test',
    'tool_fingerprint', 'genai.web.chat.v1:chatgpt',
    'direction', 'egress', 'kind', 'prompt',
    'occurred_at', '2026-10-02T14:31:07.221Z',
    'monotonic_offset_ms', 184420,
    'source', 'proxy.tls', 'collection_mode', 'm1',
    'dedup_key', 'sha256:' || repeat('11', 32),
    'size_bytes', 4096,
    'content_digest', 'sha256:' || repeat('22', 32),
    'labels', jsonb_build_array(jsonb_build_object('class', 'payment_card', 'score', 0.97)),
    'classifier_version', 'test-2026.01', 'confidence', 'high',
    'policy_decision', jsonb_build_object('rule_id', 'POL-1', 'action', 'warned', 'decided_locally', true)
  ), now());

  IF r.event_outcome <> 'duplicate' THEN
    RAISE EXCEPTION 'FAIL T6 expected duplicate, got %', r.event_outcome;
  END IF;
  RAISE NOTICE 'PASS T6 replayed event reported as duplicate';

  -- Route 2: the extension reading page context, rank 10. Same submission, better route.
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version', '1.0',
    'event_id', 'e0000000-0000-7000-8000-000000000002',
    'tenant_id', '11111111-1111-7111-8111-111111111111',
    'device_id', 'aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref', 'u_test',
    'tool_fingerprint', 'genai.web.chat.v1:chatgpt',
    'direction', 'egress', 'kind', 'prompt',
    'occurred_at', '2026-10-02T14:31:07.190Z',
    'monotonic_offset_ms', 184390,
    'source', 'ext.page_context', 'collection_mode', 'm1',
    'dedup_key', 'sha256:' || repeat('11', 32),
    'size_bytes', 4096,
    'content_digest', 'sha256:' || repeat('22', 32),
    'labels', jsonb_build_array(jsonb_build_object('class', 'payment_card', 'score', 0.97)),
    'classifier_version', 'test-2026.01', 'confidence', 'high',
    'policy_decision', jsonb_build_object('rule_id', 'POL-1', 'action', 'warned', 'decided_locally', true)
  ), now());

  IF r.event_outcome <> 'merged' THEN
    RAISE EXCEPTION 'FAIL T7 expected merged for the second route, got %', r.event_outcome;
  END IF;

  SELECT count(*) INTO n FROM ingest.observation
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T7 expected 2 observations, found %', n;
  END IF;

  SELECT count(*) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T7 expected 1 submission after two routes, found % (double counting)', n;
  END IF;

  SELECT array_length(observed_routes, 1) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T7 expected 2 observed routes, found %', n;
  END IF;
  RAISE NOTICE 'PASS T7 two routes collapsed to one submission with both routes recorded';

  SELECT * INTO r FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF r.winning_source <> 'ext.page_context' THEN
    RAISE EXCEPTION 'FAIL T8 expected upgrade to ext.page_context, still %', r.winning_source;
  END IF;
  IF r.winning_fidelity <> 10 THEN
    RAISE EXCEPTION 'FAIL T8 expected winning fidelity 10, got %', r.winning_fidelity;
  END IF;
  RAISE NOTICE 'PASS T8 higher-fidelity route won the tie-break (%)', r.winning_source;
END $$;

-- Retention was materialised from the tenant policy (payment_card, m1 -> 30 days).
DO $$
DECLARE d int;
BEGIN
  SELECT extract(day FROM (expires_at - received_at))::int INTO d
    FROM ingest.submission WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF d <> 30 THEN
    RAISE EXCEPTION 'FAIL T9 expected 30-day retention from policy, got % days', d;
  END IF;
  RAISE NOTICE 'PASS T9 retention materialised from ops.retention_policy (30 days)';
END $$;


-- =====================================================================================
-- T10-T12  The two-tier dedup ladder
-- =====================================================================================
-- At M0 there is no content digest, so observations merge on the server-derived weak key, and an
-- observation that later supplies a digest adopts the weak-only row rather than creating a twin.

DO $$
DECLARE
  r record;
  n int;
  k text;
BEGIN
  -- Two M0 observations of one submission from two routes: different event ids and device dedup
  -- keys, but the same device, tool, bucket and size.
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-000000000021',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:claude',
    'direction','egress','kind','prompt',
    'occurred_at','2026-10-03T10:00:10Z','monotonic_offset_ms',1000,
    'source','ext.web_request','collection_mode','m0',
    'dedup_key','sha256:' || repeat('a1', 32),'size_bytes',2048,
    'policy_decision', jsonb_build_object('rule_id','POL-2','action','logged','decided_locally',true)
  ), now());
  IF r.event_outcome <> 'inserted' THEN
    RAISE EXCEPTION 'FAIL T10 first M0 observation expected inserted, got %', r.event_outcome;
  END IF;

  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-000000000022',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:claude',
    'direction','egress','kind','prompt',
    'occurred_at','2026-10-03T10:02:30Z','monotonic_offset_ms',140000,
    'source','proxy.tls','collection_mode','m0',
    'dedup_key','sha256:' || repeat('a2', 32),'size_bytes',2048,
    'policy_decision', jsonb_build_object('rule_id','POL-2','action','logged','decided_locally',true)
  ), now());
  IF r.event_outcome <> 'merged' THEN
    RAISE EXCEPTION 'FAIL T10 two M0 routes did not collapse, got %', r.event_outcome;
  END IF;

  SELECT count(*) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T10 expected 2 submissions total, found % (M0 routes double-counted)', n;
  END IF;

  SELECT dedup_key, merge_confidence, observation_count INTO k, r.event_outcome, n
    FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:claude';
  IF k IS NOT NULL THEN
    RAISE EXCEPTION 'FAIL T10 a weak-only submission should have no exact key, got %', k;
  END IF;
  IF r.event_outcome <> 'low' THEN
    RAISE EXCEPTION 'FAIL T10 weak-merged row should be flagged low confidence, got %', r.event_outcome;
  END IF;
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T10 expected 2 observations on the merged row, got %', n;
  END IF;
  RAISE NOTICE 'PASS T10 two M0 routes collapsed to one submission, flagged low confidence';

  -- The same submission seen by a route that reads content, in the same bucket and at the same
  -- size, adopts the weak-only row.
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-000000000023',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:claude',
    'direction','egress','kind','prompt',
    'occurred_at','2026-10-03T10:01:00Z','monotonic_offset_ms',60000,
    'source','ext.page_context','collection_mode','m1',
    'dedup_key','sha256:' || repeat('b1', 32),'size_bytes',2048,
    'content_digest','sha256:' || repeat('b2', 32),
    'labels', jsonb_build_array(jsonb_build_object('class','source_code','score',0.88)),
    'classifier_version','test-2026.01','confidence','high',
    'policy_decision', jsonb_build_object('rule_id','POL-3','action','logged','decided_locally',true)
  ), now());

  SELECT count(*) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T11 an exact observation created a twin instead of adopting, now % submissions', n;
  END IF;

  SELECT dedup_key INTO k FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:claude';
  IF k <> 'sha256:' || repeat('b1', 32) THEN
    RAISE EXCEPTION 'FAIL T11 weak-only row was not adopted and given the exact key, got %', k;
  END IF;
  RAISE NOTICE 'PASS T11 exact observation adopted the weak-only row instead of double-counting';
END $$;

-- A rollup and a detection for one tool in one bucket stay two records. Neither carries a size, so
-- without `kind` in the weak key they would collapse into one.
DO $$
DECLARE
  n int;
BEGIN
  PERFORM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-000000000031',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','ondevice.runtime.v1:ollama',
    'direction','none','kind','usage_rollup',
    'occurred_at','2026-10-04T08:00:00Z','monotonic_offset_ms',0,
    'source','proc.detect','collection_mode','m1',
    'dedup_key','sha256:' || repeat('c1', 32),
    'window_start','2026-10-03T00:00:00Z','window_end','2026-10-04T00:00:00Z',
    'submission_count', 9, 'bytes_total', 4096
  ), now());

  PERFORM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-000000000032',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','ondevice.runtime.v1:ollama',
    'direction','none','kind','model_detection',
    'occurred_at','2026-10-04T08:01:00Z','monotonic_offset_ms',60000,
    'source','proc.detect','collection_mode','m0',
    'dedup_key','sha256:' || repeat('c2', 32),
    'detection_basis','process_scan'
  ), now());

  SELECT count(*) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'ondevice.runtime.v1:ollama';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T12 expected a rollup and a detection to stay separate, found %', n;
  END IF;
  RAISE NOTICE 'PASS T12 rollup and detection kept separate (kind is part of the weak key)';
END $$;


-- =====================================================================================
-- T13-T14  The collection-mode boundary
-- =====================================================================================
-- At M0 the collector reads no content, so a content-derived field on an M0 row is refused.

DO $$
BEGIN
  BEGIN
    INSERT INTO ingest.observation (
      tenant_id, event_id, device_id, user_ref, tool_fingerprint, direction, kind,
      occurred_at, received_at, monotonic_offset_ms, source, collection_mode,
      size_bytes, content_digest, dedup_key, schema_version, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111',
            'e0000000-0000-7000-8000-00000000000a',
            'aaaaaaaa-0000-7000-8000-000000000001', 'u_test', 'genai.web.chat.v1:chatgpt',
            'egress', 'prompt', now(), now(), 1, 'ext.web_request', 'm0',
            100, 'sha256:' || repeat('33', 32), 'sha256:' || repeat('44', 32), '1.0', now());
    RAISE EXCEPTION 'FAIL T13 an M0 observation carrying a content digest was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T13 M0 row with content rejected (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ingest.observation (
      tenant_id, event_id, device_id, user_ref, tool_fingerprint, direction, kind,
      occurred_at, received_at, monotonic_offset_ms, source, collection_mode,
      size_bytes, dedup_key, schema_version, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111',
            'e0000000-0000-7000-8000-00000000000b',
            'aaaaaaaa-0000-7000-8000-000000000001', 'u_test', 'genai.web.chat.v1:chatgpt',
            'egress', 'prompt', now(), now(), 1, 'ext.web_request', 'm1',
            100, 'sha256:' || repeat('55', 32), '1.0', now());
    RAISE EXCEPTION 'FAIL T14 an M1 prompt with no labels was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T14 M1 prompt without classifier output rejected (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T15-T18  Tenant isolation
-- =====================================================================================
-- Still sac_ingest: neither the owner nor a BYPASSRLS role, so these are properties of the store.

DO $$
DECLARE n int;
BEGIN
  -- Tenant A sees its own rows: two prompt submissions, one rollup, one detection.
  SELECT count(*) INTO n FROM ingest.submission;
  IF n <> 4 THEN
    RAISE EXCEPTION 'FAIL T15 tenant A expected 4 own submissions, saw %', n;
  END IF;
  RAISE NOTICE 'PASS T15 tenant A sees its own data (%)', n;

  PERFORM set_config('app.tenant_id', '22222222-2222-7222-8222-222222222222', false);
  SELECT count(*) INTO n FROM ingest.submission;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T16 tenant B saw % of tenant A rows', n;
  END IF;
  SELECT count(*) INTO n FROM ingest.observation;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T16 tenant B saw % of tenant A observations', n;
  END IF;
  RAISE NOTICE 'PASS T16 tenant B sees zero of tenant A rows';

  -- No tenant set: zero rows, not all rows.
  PERFORM set_config('app.tenant_id', '', false);
  SELECT count(*) INTO n FROM ingest.submission;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T17 a session with no tenant saw % rows', n;
  END IF;
  RAISE NOTICE 'PASS T17 unset tenant reads zero rows (fails closed)';
END $$;

-- A write into another tenant is refused by WITH CHECK.
DO $$
BEGIN
  PERFORM set_config('app.tenant_id', '11111111-1111-7111-8111-111111111111', false);
  BEGIN
    INSERT INTO ingest.observation (
      tenant_id, event_id, device_id, user_ref, tool_fingerprint, direction, kind,
      occurred_at, received_at, monotonic_offset_ms, source, collection_mode,
      size_bytes, dedup_key, schema_version, expires_at)
    VALUES ('22222222-2222-7222-8222-222222222222',
            'e0000000-0000-7000-8000-00000000000c',
            'aaaaaaaa-0000-7000-8000-000000000001', 'u_test', 'x', 'egress', 'prompt',
            now(), now(), 1, 'ext.web_request', 'm0', 10, 'sha256:' || repeat('66', 32),
            '1.0', now());
    RAISE EXCEPTION 'FAIL T18 a write into another tenant was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T18 cross-tenant write refused (%)', SQLERRM;
  END;
END $$;

RESET ROLE;


-- =====================================================================================
-- T19-T21  The append-only, hash-chained audit log
-- =====================================================================================

INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail)
VALUES (:ta, 'user', 'analyst@example.com', 'event.read', 'submission', 'sub-1',
        jsonb_build_object('rows', 25, 'reason', 'investigation')),
       (:ta, 'user', 'analyst@example.com', 'content.reveal', 'content', 'obj-1',
        jsonb_build_object('case_reference', 'CASE-2026-0042'));

DO $$
DECLARE
  h1 text; h2 text; p2 text;
BEGIN
  SELECT row_hash INTO h1 FROM ops.audit
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' ORDER BY audit_seq LIMIT 1;
  SELECT row_hash, prev_hash INTO h2, p2 FROM ops.audit
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' ORDER BY audit_seq OFFSET 1 LIMIT 1;

  IF h1 IS NULL OR h2 IS NULL THEN
    RAISE EXCEPTION 'FAIL T19 audit rows have no hash';
  END IF;
  IF p2 <> h1 THEN
    RAISE EXCEPTION 'FAIL T19 hash chain broken: row 2 prev_hash % <> row 1 row_hash %', p2, h1;
  END IF;
  RAISE NOTICE 'PASS T19 audit hash chain links row 2 to row 1';
END $$;

DO $$
BEGIN
  BEGIN
    UPDATE ops.audit SET detail = '{"tampered": true}'::jsonb
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T20 an audit row was updated';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T20 audit UPDATE refused (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    DELETE FROM ops.audit WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T21 an audit row was deleted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T21 audit DELETE refused (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T22-T24  Immutable observations, and no content in quarantine
-- =====================================================================================

DO $$
BEGIN
  BEGIN
    UPDATE ingest.observation SET size_bytes = 1
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T22 an observation was updated in place';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T22 in-place observation UPDATE refused (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    DELETE FROM ingest.observation
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T23 an observation was deleted outside the retention path';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T23 observation DELETE outside retention refused (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ingest.rejected (tenant_id, reason_code, detail, envelope_redacted, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111', 'schema_violation', '{}'::jsonb,
            jsonb_build_object('kind', 'prompt', 'content_excerpt',
                               jsonb_build_object('text', 'this should never be stored')),
            now() + interval '14 days');
    RAISE EXCEPTION 'FAIL T24 quarantine accepted a content excerpt';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T24 quarantine refuses stored content (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T25-T27  States that are never merged
-- =====================================================================================

DO $$
BEGIN
  BEGIN
    UPDATE ingest.submission SET content_state = 'shredded'
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T25 a submission was marked shredded without a reason';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T25 shredded-without-reason refused (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.tool (tenant_id, tool_fingerprint, sanctioned_state, decided_by, decided_at)
    VALUES ('11111111-1111-7111-8111-111111111111', 'x', 'unsanctioned', NULL, NULL);
    RAISE EXCEPTION 'FAIL T26 an unsanctioned decision with no actor was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T26 unattributed sanction decision refused (%)', SQLERRM;
  END;
END $$;

-- A newly discovered tool is `unknown`, not `unsanctioned`: the latter would report every new tool
-- as prohibited.
DO $$
DECLARE n int;
BEGIN
  INSERT INTO ops.tool (tenant_id, tool_fingerprint)
  VALUES ('11111111-1111-7111-8111-111111111111', 'genai.web.chat.v1:newtool');

  SELECT count(*) INTO n FROM ops.tool
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:newtool'
     AND sanctioned_state = 'unknown';
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T27 a newly discovered tool did not default to unknown';
  END IF;
  RAISE NOTICE 'PASS T27 newly discovered tool defaults to unknown, not unsanctioned';
END $$;


-- =====================================================================================
-- T28-T31  Content search tiers, and the index dying with its row
-- =====================================================================================

DO $$
DECLARE n int;
BEGIN
  INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode, content_search)
  VALUES ('44444444-4444-7444-8444-444444444444', 'Filename-search tenant', 'active', 'eastus',
          'm1', 'attachment_names');

  INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode, content_search)
  VALUES ('55555555-5555-7555-8555-555555555555', 'Full-text tenant', 'active', 'eastus',
          'm3', 'full_text');

  SELECT count(*) INTO n FROM ops.tenant WHERE content_search <> 'disabled';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T28 expected 2 search-enabled tenants, found %', n;
  END IF;
  RAISE NOTICE 'PASS T28 attachment_names works at M1 and full_text at M3';
END $$;

-- A search tier over data the tenant cannot collect is refused: filenames cross at M1, prompt text
-- at M3.
DO $$
BEGIN
  BEGIN
    INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode, content_search)
    VALUES ('66666666-6666-7666-8666-666666666666', 'Full-text without M3', 'active', 'eastus',
            'm1', 'full_text');
    RAISE EXCEPTION 'FAIL T29 full_text search on an M1 ceiling was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T29 full_text search without an M3 ceiling refused (%)', SQLERRM;
  END;

  BEGIN
    INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode, content_search)
    VALUES ('77777777-7777-7777-8777-777777777777', 'Filenames without M1', 'active', 'eastus',
            'm0', 'attachment_names');
    RAISE EXCEPTION 'FAIL T29 attachment_names search on an M0 ceiling was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T29 attachment_names search on an M0 ceiling refused (%)', SQLERRM;
  END;
END $$;

DO $$
DECLARE n int; v_sub uuid;
BEGIN
  SELECT submission_id INTO v_sub FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
   ORDER BY first_occurred_at LIMIT 1;

  INSERT INTO ingest.search_text (tenant_id, submission_id, unit_kind, unit_index, body, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', v_sub, 'prompt_body', 0,
          'Please summarise the attached contract for ACME Holdings', now() + interval '30 days'),
         ('11111111-1111-7111-8111-111111111111', v_sub, 'attachment_name', 0,
          'Q3-contract-acme.pdf', now() + interval '30 days');

  SELECT count(*) INTO n FROM ingest.search_text
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND submission_id = v_sub;
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T30 expected 2 index rows, found %', n;
  END IF;

  -- The generated tsvector is populated and matchable.
  SELECT count(*) INTO n FROM ingest.search_text
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tsv @@ to_tsquery('simple', 'contract');
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T30 the generated tsvector did not index the body (matched %)', n;
  END IF;

  -- Filename matching is by substring.
  SELECT count(*) INTO n FROM ingest.search_text
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND unit_kind = 'attachment_name'
     AND body ILIKE '%contract-acme%';
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T30 filename substring match failed (matched %)', n;
  END IF;
  RAISE NOTICE 'PASS T30 index rows written; generated tsvector and filename substring both match';
END $$;

DO $$
DECLARE n int; v_sub uuid;
BEGIN
  SELECT submission_id INTO v_sub FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
   ORDER BY first_occurred_at LIMIT 1;

  DELETE FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND submission_id = v_sub;

  SELECT count(*) INTO n FROM ingest.search_text
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND submission_id = v_sub;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T31 index rows survived the submission they describe (% left)', n;
  END IF;
  RAISE NOTICE 'PASS T31 erasing a submission removes its index entries in the same transaction';
END $$;


-- =====================================================================================
-- T32-T34  Commercial state: gate closures are attributed, the ledger survives erasure
-- =====================================================================================

DO $$
DECLARE n int;
BEGIN
  BEGIN
    UPDATE ops.tenant SET read_enabled = false
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T32 a gate was closed with no recorded actor or reason';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T32 closing a gate without attribution refused (%)', SQLERRM;
  END;

  UPDATE ops.tenant
     SET read_enabled       = false,
         status_reason      = 'invoice 2026-09 unpaid; reads suspended pending payment',
         status_changed_by  = 'ops@example.com',
         status_changed_at  = now()
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';

  SELECT count(*) INTO n FROM ops.tenant
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND read_enabled = false AND status_reason IS NOT NULL AND status_changed_by IS NOT NULL;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T32 an attributed gate closure did not apply';
  END IF;
  RAISE NOTICE 'PASS T32 gate closed with a recorded actor, time and reason';

  UPDATE ops.tenant
     SET read_enabled = true, status_reason = NULL,
         status_changed_by = NULL, status_changed_at = NULL
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
END $$;

-- The bill does not move when data is erased: the ledger is written forward, never derived.
DO $$
DECLARE before_events bigint; after_events bigint; subs_before int; subs_after int;
BEGIN
  INSERT INTO ops.usage_daily (tenant_id, usage_day, events_accepted, content_bytes_added,
                               devices_enrolled, devices_reporting, snapshot_at)
  VALUES ('11111111-1111-7111-8111-111111111111', current_date, 1234, 4096, 1, 1, now())
  ON CONFLICT (tenant_id, usage_day) DO UPDATE
    SET events_accepted = ops.usage_daily.events_accepted + EXCLUDED.events_accepted;

  SELECT events_accepted INTO before_events FROM ops.usage_daily
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND usage_day = current_date;
  IF before_events IS NULL OR before_events < 1234 THEN
    RAISE EXCEPTION 'FAIL T33 the ledger did not record the accepted count (got %)', before_events;
  END IF;

  SELECT count(*) INTO subs_before FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF subs_before = 0 THEN
    RAISE EXCEPTION 'FAIL T33 nothing to erase; the test proves nothing';
  END IF;

  -- Erase as the expiry and erasure paths do: submissions, then observations through the
  -- retention path; the search index goes by cascade.
  DELETE FROM ingest.submission WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  PERFORM set_config('sac.retention_delete', 'on', false);
  DELETE FROM ingest.observation WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  PERFORM set_config('sac.retention_delete', 'off', false);

  SELECT count(*) INTO subs_after FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF subs_after <> 0 THEN
    RAISE EXCEPTION 'FAIL T33 erasure did not remove the submissions';
  END IF;

  SELECT events_accepted INTO after_events FROM ops.usage_daily
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND usage_day = current_date;
  IF after_events <> before_events THEN
    RAISE EXCEPTION 'FAIL T33 the bill moved when data was erased (% -> %)', before_events, after_events;
  END IF;
  RAISE NOTICE 'PASS T33 % submissions erased, usage ledger unchanged at % events',
    subs_before, after_events;
END $$;

-- An operator can see what closing a gate would strand before closing it.
DO $$
DECLARE r record;
BEGIN
  SELECT * INTO r FROM mart.v_tenant_suspension_impact
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF r.tenant_id IS NULL THEN
    RAISE EXCEPTION 'FAIL T34 the suspension impact view returned no row for an existing tenant';
  END IF;
  IF r.ingest_enabled IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'FAIL T34 the view reports ingest as closed for an open tenant';
  END IF;
  RAISE NOTICE 'PASS T34 suspension impact view reports % enrolled device(s), % event(s) spooled',
    r.devices_enrolled, r.events_spooled_on_devices;
END $$;


-- =====================================================================================
-- T35-T36  Adoption at equal fidelity
-- =====================================================================================
-- A weak-only row adopted by an exact observation on a route of the SAME rank must take the exact
-- key and its digest together (submission_exact_key_implies_digest), and must stop being flagged as
-- a submission that could not be merged. Both observations use ext.web_request (rank 40).

SET ROLE sac_ingest;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  r record;
  n int;
  k text;
  d text;
  c text;
BEGIN
  -- First sighting at M0: a weak-only row.
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-000000000041',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:adopt-equal',
    'direction','egress','kind','prompt',
    'occurred_at','2026-11-01T09:00:00Z','monotonic_offset_ms',1000,
    'source','ext.web_request','collection_mode','m0',
    'dedup_key','sha256:' || repeat('1a', 32),'size_bytes',100,
    'policy_decision', jsonb_build_object('rule_id','POL-A','action','logged','decided_locally',true)
  ), now());
  IF r.event_outcome <> 'inserted' THEN
    RAISE EXCEPTION 'FAIL T35 first M0 observation expected inserted, got %', r.event_outcome;
  END IF;

  -- The same submission from a content-reading route of the same rank, in the same bucket.
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-000000000042',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:adopt-equal',
    'direction','egress','kind','prompt',
    'occurred_at','2026-11-01T09:01:00Z','monotonic_offset_ms',61000,
    'source','ext.web_request','collection_mode','m1',
    'dedup_key','sha256:' || repeat('2b', 32),'size_bytes',100,
    'content_digest','sha256:' || repeat('3c', 32),
    'labels', jsonb_build_array(jsonb_build_object('class','payment_card','score',0.94)),
    'classifier_version','test-2026.01','confidence','high',
    'policy_decision', jsonb_build_object('rule_id','POL-A','action','logged','decided_locally',true)
  ), now());
  IF r.event_outcome <> 'merged' THEN
    RAISE EXCEPTION 'FAIL T35 equal-rank exact observation did not adopt the weak-only row, got %', r.event_outcome;
  END IF;

  SELECT count(*) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:adopt-equal';
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T35 equal-rank adoption double-counted: % submissions', n;
  END IF;
  RAISE NOTICE 'PASS T35 equal-rank exact observation adopted the weak-only row without raising or double-counting';

  SELECT dedup_key, content_digest, merge_confidence INTO k, d, c
    FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:adopt-equal';
  IF k <> 'sha256:' || repeat('2b', 32) THEN
    RAISE EXCEPTION 'FAIL T36 adopted row did not take the exact key, got %', k;
  END IF;
  IF d <> 'sha256:' || repeat('3c', 32) THEN
    RAISE EXCEPTION 'FAIL T36 adopted row took the exact key without the digest it was derived from, got %', d;
  END IF;
  IF c <> 'high' THEN
    RAISE EXCEPTION 'FAIL T36 adopted row holds an exact key but is still flagged %', c;
  END IF;
  RAISE NOTICE 'PASS T36 adopted row carries the exact key with its digest and is no longer flagged low';
END $$;


-- =====================================================================================
-- T37  The quarantine reason-code vocabulary
-- =====================================================================================
-- ingest.rejected.reason_code accepts the per-event codes ingest-api quarantines, and nothing else.
-- Exercised with real inserts as sac_ingest, the role that writes quarantine rows.

DO $$
DECLARE
  accepted text[] := ARRAY[
    'schema_violation','unsupported_schema_version','unknown_kind','mode_violation'];
  forbidden text[] := ARRAY[
    -- an event with no tenant to file the row under
    'tenant_mismatch',
    -- batch-level refusals, which reject the whole batch and quarantine nothing
    'unknown_tenant','revoked_device','region_mismatch','oversize',
    -- content-upload codes
    'unknown_event','grant_consumed'];
  c text;
  n int;
  inserted int := 0;
BEGIN
  FOREACH c IN ARRAY accepted LOOP
    BEGIN
      INSERT INTO ingest.rejected (tenant_id, device_id, reason_code, expires_at)
      VALUES ('11111111-1111-7111-8111-111111111111',
              'aaaaaaaa-0000-7000-8000-000000000001', c, now() + interval '7 days');
      -- ROW_COUNT rather than SELECT: sac_ingest may insert quarantine rows but not read them.
      GET DIAGNOSTICS n = ROW_COUNT;
      inserted := inserted + n;
    EXCEPTION WHEN others THEN
      RAISE EXCEPTION 'FAIL T37 the CHECK rejects %, which is part of the vocabulary (%)', c, SQLERRM;
    END;
  END LOOP;

  FOREACH c IN ARRAY forbidden LOOP
    BEGIN
      INSERT INTO ingest.rejected (tenant_id, device_id, reason_code, expires_at)
      VALUES ('11111111-1111-7111-8111-111111111111',
              'aaaaaaaa-0000-7000-8000-000000000001', c, now() + interval '7 days');
      RAISE EXCEPTION 'FAIL T37 the CHECK accepts %, which is not part of the vocabulary', c;
    EXCEPTION WHEN check_violation THEN
      NULL;
    END;
  END LOOP;

  IF inserted <> array_length(accepted, 1) THEN
    RAISE EXCEPTION 'FAIL T37 expected % accepted codes to be written, % were', array_length(accepted, 1), inserted;
  END IF;
  RAISE NOTICE 'PASS T37 quarantine vocabulary pinned: % codes written, % rejected',
    inserted, array_length(forbidden, 1);
END $$;


-- =====================================================================================
-- T38-T42  The kind and mode boundaries match the envelope contract, field for field
-- =====================================================================================
-- database/tools/check-schema.mjs proves each forbid-list equals the contract's; these prove each
-- field is actually enforced. Each block ends with a positive control: a well-formed row of the same
-- kind is still accepted, so the refusals cannot pass by refusing everything. `attachments` has no
-- column on ingest.observation and so appears in none of these lists.

DO $$
DECLARE
  -- the contract's model_detection forbid-list
  forbids text[] := ARRAY['content_digest','labels','classifier_version','content_excerpt',
                          'policy_decision','size_bytes','window_start','window_end',
                          'submission_count','bytes_total','prompt_kind'];
  vals jsonb := jsonb_build_object(
    'content_digest', 'sha256:' || repeat('a1', 32),
    'labels', jsonb_build_array(jsonb_build_object('class','payment_card','score',0.5)),
    'classifier_version', 'test-2026.01',
    'content_excerpt', 'minimised excerpt',
    'policy_decision', jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
    'size_bytes', 100,
    'window_start', '2026-11-01T00:00:00Z',
    'window_end', '2026-11-02T00:00:00Z',
    'submission_count', 1,
    'bytes_total', 100,
    'prompt_kind', 'user');
  base jsonb;
  f text;
  n int;
BEGIN
  FOREACH f IN ARRAY forbids LOOP
    base := jsonb_build_object(
      'tenant_id','11111111-1111-7111-8111-111111111111',
      'event_id', gen_random_uuid(),
      'device_id','aaaaaaaa-0000-7000-8000-000000000001',
      'user_ref','u_test','tool_fingerprint','ondevice.runtime.v1:contract-sweep',
      'direction','none','kind','model_detection',
      'occurred_at','2026-11-02T09:00:00Z','received_at', now(), 'ingested_at', now(),
      'monotonic_offset_ms', 1, 'source','proc.detect','collection_mode','m1',
      'detection_basis','process_scan','dedup_key','sha256:' || repeat('d1', 32),
      'schema_version','1.0','expires_at', now() + interval '30 days')
      || jsonb_build_object(f, vals -> f);
    BEGIN
      INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
      RAISE EXCEPTION 'FAIL T38 model_detection accepted %, which the contract forbids for this kind', f;
    EXCEPTION WHEN check_violation THEN
      NULL;
    END;
  END LOOP;

  base := jsonb_build_object(
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'event_id', gen_random_uuid(),
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','ondevice.runtime.v1:contract-sweep',
    'direction','none','kind','model_detection',
    'occurred_at','2026-11-02T09:00:00Z','received_at', now(), 'ingested_at', now(),
    'monotonic_offset_ms', 1, 'source','proc.detect','collection_mode','m1',
    'detection_basis','process_scan','dedup_key','sha256:' || repeat('d1', 32),
    'schema_version','1.0','expires_at', now() + interval '30 days');
  INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T38 the well-formed model_detection was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T38 model_detection refuses all % contract-forbidden fields and still accepts a valid one', array_length(forbids, 1);
END $$;

DO $$
DECLARE
  -- the contract's usage_rollup forbid-list
  forbids text[] := ARRAY['content_digest','labels','classifier_version','content_excerpt',
                          'policy_decision','size_bytes','detection_basis','prompt_kind'];
  vals jsonb := jsonb_build_object(
    'content_digest', 'sha256:' || repeat('a2', 32),
    'labels', jsonb_build_array(jsonb_build_object('class','payment_card','score',0.5)),
    'classifier_version', 'test-2026.01',
    'content_excerpt', 'minimised excerpt',
    'policy_decision', jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
    'size_bytes', 100,
    'detection_basis', 'process_scan',
    'prompt_kind', 'user');
  base jsonb;
  f text;
  n int;
BEGIN
  FOREACH f IN ARRAY forbids LOOP
    base := jsonb_build_object(
      'tenant_id','11111111-1111-7111-8111-111111111111',
      'event_id', gen_random_uuid(),
      'device_id','aaaaaaaa-0000-7000-8000-000000000001',
      'user_ref','u_test','tool_fingerprint','ondevice.runtime.v1:contract-sweep',
      'direction','none','kind','usage_rollup',
      'occurred_at','2026-11-02T10:00:00Z','received_at', now(), 'ingested_at', now(),
      'monotonic_offset_ms', 1, 'source','proc.detect','collection_mode','m1',
      'window_start','2026-11-01T00:00:00Z','window_end','2026-11-02T00:00:00Z',
      'submission_count', 3, 'bytes_total', 2048,
      'dedup_key','sha256:' || repeat('d2', 32),
      'schema_version','1.0','expires_at', now() + interval '30 days')
      || jsonb_build_object(f, vals -> f);
    BEGIN
      INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
      RAISE EXCEPTION 'FAIL T39 usage_rollup accepted %, which the contract forbids for this kind', f;
    EXCEPTION WHEN check_violation THEN
      NULL;
    END;
  END LOOP;

  base := jsonb_build_object(
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'event_id', gen_random_uuid(),
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','ondevice.runtime.v1:contract-sweep',
    'direction','none','kind','usage_rollup',
    'occurred_at','2026-11-02T10:00:00Z','received_at', now(), 'ingested_at', now(),
    'monotonic_offset_ms', 1, 'source','proc.detect','collection_mode','m1',
    'window_start','2026-11-01T00:00:00Z','window_end','2026-11-02T00:00:00Z',
    'submission_count', 3, 'bytes_total', 2048,
    'dedup_key','sha256:' || repeat('d2', 32),
    'schema_version','1.0','expires_at', now() + interval '30 days');
  INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T39 the well-formed usage_rollup was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T39 usage_rollup refuses all % contract-forbidden fields and still accepts a valid one', array_length(forbids, 1);
END $$;

DO $$
DECLARE
  -- the contract's prompt forbid-list: the window fields and detection_basis
  forbids text[] := ARRAY['window_start','window_end','submission_count','bytes_total','detection_basis'];
  vals jsonb := jsonb_build_object(
    'window_start','2026-11-01T00:00:00Z',
    'window_end','2026-11-02T00:00:00Z',
    'submission_count', 1,
    'bytes_total', 100,
    'detection_basis','process_scan');
  base jsonb;
  f text;
  n int;
BEGIN
  FOREACH f IN ARRAY forbids LOOP
    base := jsonb_build_object(
      'tenant_id','11111111-1111-7111-8111-111111111111',
      'event_id', gen_random_uuid(),
      'device_id','aaaaaaaa-0000-7000-8000-000000000001',
      'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:contract-sweep',
      'direction','egress','kind','prompt',
      'occurred_at','2026-11-02T11:00:00Z','received_at', now(), 'ingested_at', now(),
      'monotonic_offset_ms', 1, 'source','ext.page_context','collection_mode','m1',
      'size_bytes', 100,
      'content_digest','sha256:' || repeat('a3', 32),
      'labels', jsonb_build_array(jsonb_build_object('class','payment_card','score',0.9)),
      'classifier_version','test-2026.01','confidence','high',
      'policy_decision', jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
      'dedup_key','sha256:' || repeat('d3', 32),
      'schema_version','1.0','expires_at', now() + interval '30 days')
      || jsonb_build_object(f, vals -> f);
    BEGIN
      INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
      RAISE EXCEPTION 'FAIL T40 prompt accepted %, which the contract forbids for this kind', f;
    EXCEPTION WHEN check_violation THEN
      NULL;
    END;
  END LOOP;

  base := jsonb_build_object(
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'event_id', gen_random_uuid(),
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:contract-sweep',
    'direction','egress','kind','prompt',
    'occurred_at','2026-11-02T11:00:00Z','received_at', now(), 'ingested_at', now(),
    'monotonic_offset_ms', 1, 'source','ext.page_context','collection_mode','m1',
    'size_bytes', 100,
    'content_digest','sha256:' || repeat('a3', 32),
    'labels', jsonb_build_array(jsonb_build_object('class','payment_card','score',0.9)),
    'classifier_version','test-2026.01','confidence','high',
    'policy_decision', jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
    'dedup_key','sha256:' || repeat('d3', 32),
    'schema_version','1.0','expires_at', now() + interval '30 days');
  INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T40 the well-formed prompt was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T40 prompt refuses all % contract-forbidden fields and still accepts a valid one', array_length(forbids, 1);
END $$;

DO $$
DECLARE
  -- the contract's M0 prompt forbid-list: everything derived from reading content
  forbids text[] := ARRAY['content_digest','labels','classifier_version','confidence','content_excerpt','prompt_kind'];
  vals jsonb := jsonb_build_object(
    'content_digest','sha256:' || repeat('a4', 32),
    'labels', jsonb_build_array(jsonb_build_object('class','payment_card','score',0.9)),
    'classifier_version','test-2026.01',
    'confidence','high',
    'content_excerpt','minimised excerpt',
    'prompt_kind','user');
  base jsonb;
  f text;
  n int;
BEGIN
  FOREACH f IN ARRAY forbids LOOP
    base := jsonb_build_object(
      'tenant_id','11111111-1111-7111-8111-111111111111',
      'event_id', gen_random_uuid(),
      'device_id','aaaaaaaa-0000-7000-8000-000000000001',
      'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:contract-sweep',
      'direction','egress','kind','prompt',
      'occurred_at','2026-11-02T12:00:00Z','received_at', now(), 'ingested_at', now(),
      'monotonic_offset_ms', 1, 'source','ext.page_context','collection_mode','m0',
      'size_bytes', 100,
      'policy_decision', jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
      'dedup_key','sha256:' || repeat('d4', 32),
      'schema_version','1.0','expires_at', now() + interval '30 days')
      || jsonb_build_object(f, vals -> f);
    BEGIN
      INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
      RAISE EXCEPTION 'FAIL T41 an M0 record accepted %, a field derived from content it may not read', f;
    EXCEPTION WHEN check_violation THEN
      NULL;
    END;
  END LOOP;

  base := jsonb_build_object(
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'event_id', gen_random_uuid(),
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:contract-sweep',
    'direction','egress','kind','prompt',
    'occurred_at','2026-11-02T12:00:00Z','received_at', now(), 'ingested_at', now(),
    'monotonic_offset_ms', 1, 'source','ext.page_context','collection_mode','m0',
    'size_bytes', 100,
    'policy_decision', jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
    'dedup_key','sha256:' || repeat('d4', 32),
    'schema_version','1.0','expires_at', now() + interval '30 days');
  INSERT INTO ingest.observation SELECT * FROM jsonb_populate_record(NULL::ingest.observation, base);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T41 the well-formed M0 record was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T41 an M0 record refuses all % content-derived fields and a clean M0 record is still accepted', array_length(forbids, 1);
END $$;

-- `confidence` is a classifier output, so an M0 record carrying it means the collector read content
-- it was not permitted to read. Asserted by name, outside the loop above.
DO $$
DECLARE
  n int;
BEGIN
  BEGIN
    INSERT INTO ingest.observation (tenant_id, event_id, device_id, user_ref, tool_fingerprint,
                                    direction, kind, occurred_at, received_at,
                                    monotonic_offset_ms, source, collection_mode, size_bytes,
                                    confidence, policy_decision, dedup_key, schema_version, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(),
            'aaaaaaaa-0000-7000-8000-000000000001', 'u_test',
            'genai.web.chat.v1:contract-sweep', 'egress', 'prompt', now(), now(), 1,
            'ext.page_context', 'm0', 100,
            'high',
            jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
            'sha256:' || repeat('d5', 32), '1.0', now() + interval '30 days');
    RAISE EXCEPTION 'FAIL T42 an M0 record was accepted carrying `confidence`, a classifier output';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  INSERT INTO ingest.observation (tenant_id, event_id, device_id, user_ref, tool_fingerprint,
                                  direction, kind, occurred_at, received_at,
                                  monotonic_offset_ms, source, collection_mode, size_bytes,
                                  policy_decision, dedup_key, schema_version, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(),
          'aaaaaaaa-0000-7000-8000-000000000001', 'u_test',
          'genai.web.chat.v1:contract-sweep', 'egress', 'prompt', now(), now(), 1,
          'ext.page_context', 'm0', 100,
          jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
          'sha256:' || repeat('d5', 32), '1.0', now() + interval '30 days');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T42 the clean M0 record was not accepted, so the refusal above proves nothing';
  END IF;
  RAISE NOTICE 'PASS T42 an M0 record carrying `confidence` is refused, and a clean M0 record is accepted';
END $$;


-- =====================================================================================
-- T43-T45  The single-use retrieval grant
-- =====================================================================================
-- content-vault redeems a grant with
--   UPDATE ops.retrieval_grant SET used_at = ..., used_by = ...
--    WHERE tenant_id = ... AND grant_id = ... AND used_at IS NULL
-- and treats zero rows as a refusal. Run as sac_vault, so the table, its policy and the grants are
-- exercised together. Single use holds at three levels: the first claim redeems; the guarded claim
-- a second time affects nothing; and an UPDATE that forgets the guard is refused by the trigger.

SET ROLE sac_vault;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  v_tenant constant uuid := '11111111-1111-7111-8111-111111111111';
  v_grant  constant uuid := 'cccccccc-0000-7000-8000-000000000001';
  n int;
  l_used_at timestamptz;
  l_used_by text;
BEGIN
  INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                   principal, case_reference, second_approver, issued_at, expires_at,
                                   raw_digest)
  VALUES (v_tenant, v_grant, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
          'analyst@example.test', 'CASE-1', 'approver@example.test', now(),
          now() + interval '1 hour', 'sha256:' || repeat('e1', 32));

  SELECT used_at, used_by INTO l_used_at, l_used_by
    FROM ops.retrieval_grant WHERE tenant_id = v_tenant AND grant_id = v_grant;
  IF l_used_at IS NOT NULL OR l_used_by IS NOT NULL THEN
    RAISE EXCEPTION 'FAIL T43 a freshly issued grant is already marked as redeemed';
  END IF;

  UPDATE ops.retrieval_grant
     SET used_at = now(), used_by = 'analyst@example.test'
   WHERE tenant_id = v_tenant AND grant_id = v_grant AND used_at IS NULL
  RETURNING used_at, used_by INTO l_used_at, l_used_by;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T43 the first claim affected % rows, expected exactly 1', n;
  END IF;
  IF l_used_at IS NULL OR l_used_by IS NULL THEN
    RAISE EXCEPTION 'FAIL T43 the first claim left a half-claimed row (used_at=%, used_by=%)', l_used_at, l_used_by;
  END IF;

  UPDATE ops.retrieval_grant
     SET used_at = now(), used_by = 'attacker@example.test'
   WHERE tenant_id = v_tenant AND grant_id = v_grant AND used_at IS NULL;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T43 a second claim affected % rows; the grant is not single-use', n;
  END IF;

  SELECT used_by INTO l_used_by FROM ops.retrieval_grant
   WHERE tenant_id = v_tenant AND grant_id = v_grant;
  IF l_used_by <> 'analyst@example.test' THEN
    RAISE EXCEPTION 'FAIL T43 the second claim overwrote the first redemption (used_by is now %)', l_used_by;
  END IF;

  RAISE NOTICE 'PASS T43 first claim redeems the grant; a second claim affects 0 rows and cannot overwrite it';
END $$;

DO $$
DECLARE
  v_tenant constant uuid := '11111111-1111-7111-8111-111111111111';
  v_grant  constant uuid := 'cccccccc-0000-7000-8000-000000000002';
  n int;
  l_used_by text;
BEGIN
  INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                   principal, case_reference, second_approver, issued_at, expires_at,
                                   raw_digest)
  VALUES (v_tenant, v_grant, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
          'analyst@example.test', 'CASE-2', 'approver@example.test', now(),
          now() + interval '1 hour', 'sha256:' || repeat('e2', 32));

  UPDATE ops.retrieval_grant SET used_at = now(), used_by = 'analyst@example.test'
   WHERE tenant_id = v_tenant AND grant_id = v_grant AND used_at IS NULL;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T44 setup: the first claim affected % rows', n; END IF;

  -- The UPDATE that forgot the guard. The message must name single use, so a different refusal
  -- cannot pass for this one.
  BEGIN
    UPDATE ops.retrieval_grant
       SET used_at = now(), used_by = 'attacker@example.test'
     WHERE tenant_id = v_tenant AND grant_id = v_grant;
    RAISE EXCEPTION 'FAIL T44 an UPDATE without the `used_at IS NULL` guard re-redeemed a claimed grant';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('single-use' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T44 the unguarded UPDATE was refused, but not by the single-use trigger (%)', SQLERRM;
    END IF;
  END;

  SELECT used_by INTO l_used_by FROM ops.retrieval_grant
   WHERE tenant_id = v_tenant AND grant_id = v_grant;
  IF l_used_by <> 'analyst@example.test' THEN
    RAISE EXCEPTION 'FAIL T44 the refused update still changed the row (used_by is now %)', l_used_by;
  END IF;
  RAISE NOTICE 'PASS T44 the store refuses a redemption that bypasses the `used_at IS NULL` guard';

  -- A half-claimed row is unrepresentable. Tested by INSERT: an UPDATE of a claimed row would be
  -- refused by the trigger before the CHECK could fire.
  BEGIN
    INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                     principal, case_reference, second_approver, issued_at, expires_at,
                                     used_at, used_by, raw_digest)
    VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000003', gen_random_uuid(), gen_random_uuid(),
            gen_random_uuid(), 'analyst@example.test', 'CASE-3', 'approver@example.test',
            now(), now() + interval '1 hour', now(), NULL, 'sha256:' || repeat('e3', 32));
    RAISE EXCEPTION 'FAIL T44 a half-claimed row (used_at set, used_by NULL) was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_claim_is_whole' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T44 the half-claimed row was refused, but not by retrieval_grant_claim_is_whole (%)', SQLERRM;
    END IF;
  END;

  -- Two-person control: an approver cannot approve their own request.
  BEGIN
    INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                     principal, case_reference, second_approver, issued_at, expires_at,
                                     raw_digest)
    VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000004', gen_random_uuid(), gen_random_uuid(),
            gen_random_uuid(), 'same@example.test', 'CASE-4', 'same@example.test',
            now(), now() + interval '1 hour', 'sha256:' || repeat('e4', 32));
    RAISE EXCEPTION 'FAIL T44 a grant whose approver is also the requester was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_second_approver_distinct' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T44 the self-approved grant was refused, but not by retrieval_grant_second_approver_distinct (%)', SQLERRM;
    END IF;
  END;

  RAISE NOTICE 'PASS T44 a half-claimed row and a self-approved grant are both unrepresentable';
END $$;

-- A retrieval grant's raw_digest has exactly one spelling: "sha256:" + 64 lowercase hex.
DO $$
DECLARE
  v_tenant constant uuid := '11111111-1111-7111-8111-111111111111';
  n int;
BEGIN
  INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                   principal, case_reference, second_approver, issued_at, expires_at,
                                   raw_digest)
  VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000005', gen_random_uuid(), gen_random_uuid(),
          gen_random_uuid(), 'analyst@example.test', 'CASE-5', 'approver@example.test',
          now(), now() + interval '1 hour', 'sha256:' || repeat('a5', 32));
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T45 a well-formed raw_digest was not accepted'; END IF;

  BEGIN
    INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                     principal, case_reference, second_approver, issued_at, expires_at,
                                     raw_digest)
    VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000006', gen_random_uuid(), gen_random_uuid(),
            gen_random_uuid(), 'analyst@example.test', 'CASE-6', 'approver@example.test',
            now(), now() + interval '1 hour', 'not-a-digest');
    RAISE EXCEPTION 'FAIL T45 a raw_digest that is not a sha256 digest was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_raw_digest_is_sha256' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T45 the malformed digest was refused, but not by retrieval_grant_raw_digest_is_sha256 (%)', SQLERRM;
    END IF;
  END;

  BEGIN
    INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                     principal, case_reference, second_approver, issued_at, expires_at,
                                     raw_digest)
    VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000007', gen_random_uuid(), gen_random_uuid(),
            gen_random_uuid(), 'analyst@example.test', 'CASE-7', 'approver@example.test',
            now(), now() + interval '1 hour', 'sha256:' || repeat('A5', 32));
    RAISE EXCEPTION 'FAIL T45 an uppercase-hex raw_digest was accepted; the digest has one spelling';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_raw_digest_is_sha256' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T45 the uppercase digest was refused, but not by retrieval_grant_raw_digest_is_sha256 (%)', SQLERRM;
    END IF;
  END;

  RAISE NOTICE 'PASS T45 a grant carries a lowercase sha256 digest or it is refused';
END $$;

RESET ROLE;


-- =====================================================================================
-- T46  Re-enrolment idempotency: one device per hardware identity
-- =====================================================================================

SET ROLE sac_control;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
BEGIN
  INSERT INTO ops.device (tenant_id, device_id, os, hardware_identity_hash)
  VALUES ('11111111-1111-7111-8111-111111111111',
          'aaaaaaaa-0000-7000-8000-0000000000f1', 'windows', 'hwid-t47');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T46 the first device with a hardware identity hash was not accepted';
  END IF;

  BEGIN
    INSERT INTO ops.device (tenant_id, device_id, os, hardware_identity_hash)
    VALUES ('11111111-1111-7111-8111-111111111111',
            'aaaaaaaa-0000-7000-8000-0000000000f2', 'windows', 'hwid-t47');
    RAISE EXCEPTION 'FAIL T46 a duplicate (tenant_id, hardware_identity_hash) was accepted';
  EXCEPTION WHEN unique_violation THEN
    NULL;
  END;

  -- A device with no hardware identity is still allowed: the index is partial.
  INSERT INTO ops.device (tenant_id, device_id, os, hardware_identity_hash)
  VALUES ('11111111-1111-7111-8111-111111111111',
          'aaaaaaaa-0000-7000-8000-0000000000f3', 'windows', NULL);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T46 a device with a NULL hardware identity was refused, so the partial predicate is wrong';
  END IF;

  RAISE NOTICE 'PASS T46 duplicate (tenant_id, hardware_identity_hash) refused; NULL identities remain unconstrained';
END $$;


-- =====================================================================================
-- T47-T53  Stored content: the upload grant, ops.content and who can reach it
-- =====================================================================================
-- control-api records the upload grant; content-vault claims it and stores the sealed object in one
-- transaction; the expire job deletes expired objects and index entries without reading them; no
-- other role can read content at all.

-- control-api decides three grants for tenant A's device.
DO $$
BEGIN
  INSERT INTO ops.grant (tenant_id, grant_id, event_id, device_id, decided_at, decision, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', '9a000000-0000-7000-8000-000000000001',
          'e0000000-0000-7000-8000-0000000000c1', 'aaaaaaaa-0000-7000-8000-000000000001',
          now(), 'granted', now() + interval '10 minutes'),
         ('11111111-1111-7111-8111-111111111111', '9a000000-0000-7000-8000-000000000002',
          'e0000000-0000-7000-8000-0000000000c2', 'aaaaaaaa-0000-7000-8000-000000000001',
          now(), 'granted', now() + interval '10 minutes'),
         ('11111111-1111-7111-8111-111111111111', '9a000000-0000-7000-8000-000000000003',
          'e0000000-0000-7000-8000-0000000000c3', 'aaaaaaaa-0000-7000-8000-000000000001',
          now() - interval '2 days', 'granted', now() + interval '10 minutes');
  BEGIN
    INSERT INTO ops.grant (tenant_id, grant_id, event_id, device_id, decided_at, decision)
    VALUES ('11111111-1111-7111-8111-111111111111', '9a000000-0000-7000-8000-000000000009',
            'e0000000-0000-7000-8000-0000000000c9', 'aaaaaaaa-0000-7000-8000-000000000001',
            now(), 'granted');
    RAISE EXCEPTION 'FAIL T47 a granted upload with no expiry was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
END $$;

SET ROLE sac_vault;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
BEGIN
  -- The claim content-vault makes: once.
  UPDATE ops.grant SET used_at = now()
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND grant_id = '9a000000-0000-7000-8000-000000000001'
     AND event_id = 'e0000000-0000-7000-8000-0000000000c1'
     AND used_at IS NULL AND decision = 'granted' AND expires_at > now();
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T47 the first claim of an upload grant affected % rows', n; END IF;

  UPDATE ops.grant SET used_at = now()
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND grant_id = '9a000000-0000-7000-8000-000000000001'
     AND used_at IS NULL AND decision = 'granted' AND expires_at > now();
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T47 a second claim of an upload grant affected % rows', n; END IF;

  -- content-vault sets used_at and nothing else.
  BEGIN
    UPDATE ops.grant SET decision = 'voided'
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
       AND grant_id = '9a000000-0000-7000-8000-000000000002';
    RAISE EXCEPTION 'FAIL T47 sac_vault changed a grant decision';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;

  RAISE NOTICE 'PASS T47 an upload grant needs an expiry, is claimed once, and content-vault can only claim it';
END $$;

DO $$
DECLARE
  n int;
  l_digest text;
BEGIN
  -- 100 bytes of plaintext seal to 12 (nonce) + 100 + 16 (tag) = 128 bytes.
  INSERT INTO ops.content (tenant_id, object_id, event_id, grant_id, key_version, ciphertext,
                           plaintext_size_bytes, raw_digest, retention_class, prompt_kind, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', '0b000000-0000-7000-8000-000000000001',
          'e0000000-0000-7000-8000-0000000000c1', '9a000000-0000-7000-8000-000000000001', 'v1',
          decode(repeat('ab', 128), 'hex'), 100, 'sha256:' || repeat('c1', 32), 'content', 'user',
          now() + interval '30 days');
  SELECT raw_digest INTO l_digest FROM ops.content
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND event_id = 'e0000000-0000-7000-8000-0000000000c1';
  IF l_digest IS DISTINCT FROM 'sha256:' || repeat('c1', 32) THEN
    RAISE EXCEPTION 'FAIL T48 content-vault could not read back the object it stored';
  END IF;

  -- A value whose length is not the sealed length of plaintext_size_bytes is refused, so plaintext
  -- cannot be stored by mistake.
  BEGIN
    INSERT INTO ops.content (tenant_id, object_id, event_id, grant_id, key_version, ciphertext,
                             plaintext_size_bytes, raw_digest, retention_class, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(),
            'e0000000-0000-7000-8000-0000000000c2', '9a000000-0000-7000-8000-000000000002', 'v1',
            convert_to('plaintext prompt', 'UTF8'), 16, 'sha256:' || repeat('c2', 32), 'content',
            now() + interval '30 days');
    RAISE EXCEPTION 'FAIL T48 an object whose length is not nonce + plaintext + tag was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  -- One object per event.
  BEGIN
    INSERT INTO ops.content (tenant_id, object_id, event_id, grant_id, key_version, ciphertext,
                             plaintext_size_bytes, raw_digest, retention_class, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(),
            'e0000000-0000-7000-8000-0000000000c1', '9a000000-0000-7000-8000-000000000002', 'v1',
            decode(repeat('ab', 29), 'hex'), 1, 'sha256:' || repeat('c3', 32), 'content',
            now() + interval '30 days');
    RAISE EXCEPTION 'FAIL T48 a second object for one event was accepted';
  EXCEPTION WHEN unique_violation THEN NULL; END;

  -- Only for the event the grant names.
  BEGIN
    INSERT INTO ops.content (tenant_id, object_id, event_id, grant_id, key_version, ciphertext,
                             plaintext_size_bytes, raw_digest, retention_class, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(),
            'e0000000-0000-7000-8000-0000000000c8', '9a000000-0000-7000-8000-000000000002', 'v1',
            decode(repeat('ab', 29), 'hex'), 1, 'sha256:' || repeat('c4', 32), 'content',
            now() + interval '30 days');
    RAISE EXCEPTION 'FAIL T48 content was stored for an event its grant does not name';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;

  -- Over 16 MiB is refused.
  BEGIN
    INSERT INTO ops.content (tenant_id, object_id, event_id, grant_id, key_version, ciphertext,
                             plaintext_size_bytes, raw_digest, retention_class, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(),
            'e0000000-0000-7000-8000-0000000000c2', '9a000000-0000-7000-8000-000000000002', 'v1',
            decode(repeat('ab', 29), 'hex'), 16777217, 'sha256:' || repeat('c5', 32), 'content',
            now() + interval '30 days');
    RAISE EXCEPTION 'FAIL T48 an object larger than 16 MiB was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  -- An expired object for the expire job to find (T51).
  INSERT INTO ops.content (tenant_id, object_id, event_id, grant_id, key_version, ciphertext,
                           plaintext_size_bytes, raw_digest, retention_class, created_at, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', '0b000000-0000-7000-8000-000000000003',
          'e0000000-0000-7000-8000-0000000000c3', '9a000000-0000-7000-8000-000000000003', 'v1',
          decode(repeat('ab', 29), 'hex'), 1, 'sha256:' || repeat('c6', 32), 'content',
          now() - interval '2 days', now() - interval '1 day');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T48 the expired fixture object was not stored'; END IF;

  RAISE NOTICE 'PASS T48 content-vault stores one sealed object per granted event, at most 16 MiB';
END $$;

SET app.tenant_id = '22222222-2222-7222-8222-222222222222';

DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM ops.content;
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T49 content-vault in tenant B sees % of tenant A''s objects', n; END IF;
  RAISE NOTICE 'PASS T49 stored content is isolated by forced row-level security';
END $$;

RESET ROLE;

-- No role but content-vault and the expire job can touch ops.content.
DO $$
DECLARE
  r text;
BEGIN
  FOREACH r IN ARRAY ARRAY['sac_query', 'sac_ingest', 'sac_control'] LOOP
    EXECUTE format('SET LOCAL ROLE %I', r);
    PERFORM set_config('app.tenant_id', '11111111-1111-7111-8111-111111111111', true);
    BEGIN
      PERFORM count(*) FROM ops.content;
      RAISE EXCEPTION 'FAIL T50 % can read ops.content', r;
    EXCEPTION WHEN insufficient_privilege THEN NULL; END;
    RESET ROLE;
  END LOOP;
  RAISE NOTICE 'PASS T50 sac_query, sac_ingest and sac_control cannot read stored content';
END $$;

SET ROLE sac_ops;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
BEGIN
  -- The expire job reads only the columns that decide expiry...
  SELECT count(*) INTO n FROM (SELECT object_id, expires_at FROM ops.content) c;
  IF n <> 2 THEN RAISE EXCEPTION 'FAIL T51 the expire job sees % objects, expected 2', n; END IF;

  -- ...never the ciphertext or the digest...
  BEGIN
    PERFORM ciphertext FROM ops.content;
    RAISE EXCEPTION 'FAIL T51 the expire job can read ciphertext';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  BEGIN
    PERFORM raw_digest FROM ops.content;
    RAISE EXCEPTION 'FAIL T51 the expire job can read the content digest';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;

  -- ...and deletes what has expired.
  DELETE FROM ops.content
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND expires_at <= now();
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T51 the expiry delete removed % objects, expected 1', n; END IF;

  RAISE NOTICE 'PASS T51 the expire job deletes expired content without being able to read it';
END $$;

-- The expire job leaves a receipt; content-vault reads receipts but does not write them.
DO $$
DECLARE n int;
BEGIN
  INSERT INTO ops.erasure_receipt (tenant_id, receipt_id, scope_kind, requested_by, requested_at,
                                   completed_at, mechanisms, removed_counts)
  VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(), 'retention', 'jobs', now(),
          now(), ARRAY['row_delete'], '{"content": 1}'::jsonb);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T52 the expire job could not record an erasure receipt'; END IF;
END $$;

RESET ROLE;
SET ROLE sac_vault;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM ops.erasure_receipt;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T52 content-vault reads % receipts, expected 1', n; END IF;
  BEGIN
    INSERT INTO ops.erasure_receipt (tenant_id, receipt_id, scope_kind, subject_ref, requested_by, requested_at)
    VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(), 'subject', 'u_test', 'content-vault', now());
    RAISE EXCEPTION 'FAIL T52 content-vault wrote an erasure receipt';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  RAISE NOTICE 'PASS T52 the expire job records erasure receipts; content-vault can only read them';
END $$;

RESET ROLE;

-- The expire job sweeps expired index entries without reading their text.
DO $$
DECLARE v_sub uuid;
BEGIN
  SELECT submission_id INTO v_sub FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' LIMIT 1;
  IF v_sub IS NULL THEN RAISE EXCEPTION 'FAIL T53 setup: tenant A has no submission to index'; END IF;
  INSERT INTO ingest.search_text (tenant_id, submission_id, unit_kind, unit_index, body, created_at, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', v_sub, 'prompt_body', 0,
          'expired prompt text', now() - interval '2 days', now() - interval '1 day'),
         ('11111111-1111-7111-8111-111111111111', v_sub, 'attachment_name', 0,
          'live-attachment.pdf', now(), now() + interval '30 days');
END $$;

SET ROLE sac_ops;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE n int;
BEGIN
  BEGIN
    PERFORM body FROM ingest.search_text;
    RAISE EXCEPTION 'FAIL T53 the expire job can read indexed prompt text';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;

  DELETE FROM ingest.search_text
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND expires_at <= now();
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T53 the index sweep removed % entries, expected 1', n; END IF;
  RAISE NOTICE 'PASS T53 the expire job deletes expired index entries without being able to read them';
END $$;

RESET ROLE;


-- =====================================================================================
-- T54-T67  Identity, provisioning and deployment
-- =====================================================================================
-- Fixtures are written in both tenants, so every isolation assertion has rows on both sides; each
-- assertion runs as a runtime role. The cross-tenant lookups run with NO tenant set, which is the
-- state they exist for, through sac_resolver, which is not a superuser.

INSERT INTO ops.identity_connection (connection_id, tenant_id, provider, entra_tenant_id, issuer, client_id,
                                     status, activated_at, activated_by)
VALUES
  ('f0000000-0000-4000-8000-00000000a001', :ta, 'entra', 'a0a0a0a0-0000-4000-8000-0000000000a1', NULL, NULL,
   'active', now(), 'first-admin@a.example'),
  ('f0000000-0000-4000-8000-00000000a002', :ta, 'oidc', NULL, 'https://idp.a.example/disabled', 'client-a',
   'disabled', now(), 'first-admin@a.example'),
  ('f0000000-0000-4000-8000-00000000b001', :tb, 'oidc', NULL, 'https://idp.b.example', 'client-b',
   'active', now(), 'first-admin@b.example'),
  ('f0000000-0000-4000-8000-00000000b002', :tb, 'entra', 'b0b0b0b0-0000-4000-8000-0000000000b1', NULL, NULL,
   'pending', NULL, NULL);

INSERT INTO ops.tenant_email_domain (domain, tenant_id, created_by)
VALUES ('a.example', :ta, 'vendor'), ('b.example', :tb, 'vendor');

INSERT INTO ops.onboarding_invite (tenant_id, token_hash, created_by, created_at, expires_at, used_at, used_by)
VALUES
  (:ta, 'sha256:' || repeat('a1', 32), 'vendor', now(), now() + interval '7 days', NULL, NULL),
  (:ta, 'sha256:' || repeat('a2', 32), 'vendor', now(), now() + interval '7 days', now(), 'first-admin@a.example'),
  (:ta, 'sha256:' || repeat('a3', 32), 'vendor', now() - interval '9 days', now() - interval '2 days', NULL, NULL),
  (:tb, 'sha256:' || repeat('b1', 32), 'vendor', now(), now() + interval '7 days', NULL, NULL);

INSERT INTO ops.role_grant (tenant_id, connection_id, subject, role, granted_by)
VALUES (:ta, 'f0000000-0000-4000-8000-00000000a001', 'oid-a', 'admin', 'onboarding'),
       (:tb, 'f0000000-0000-4000-8000-00000000b001', 'sub-b', 'viewer', 'onboarding');

INSERT INTO ops.auth_session (session_hash, tenant_id, connection_id, subject, actor, roles,
                              created_at, expires_at, revoked_at)
VALUES
  (sha256('\x73657373612d6c697665'::bytea), :ta, 'f0000000-0000-4000-8000-00000000a001', 'oid-a', 'ada@a.example',
   ARRAY['admin'], now(), now() + interval '8 hours', NULL),
  (sha256('\x73657373612d7265766f6b6564'::bytea), :ta, 'f0000000-0000-4000-8000-00000000a001', 'oid-a', 'ada@a.example',
   ARRAY['admin'], now(), now() + interval '8 hours', now()),
  (sha256('\x73657373612d65787069726564'::bytea), :ta, 'f0000000-0000-4000-8000-00000000a001', 'oid-a', 'ada@a.example',
   ARRAY['viewer'], now() - interval '9 hours', now() - interval '1 hour', NULL),
  (sha256('\x73657373622d6c697665'::bytea), :tb, 'f0000000-0000-4000-8000-00000000b001', 'sub-b', 'bo@b.example',
   ARRAY['viewer','analyst'], now(), now() + interval '8 hours', NULL);

INSERT INTO ops.scim_token (tenant_id, token_hash, label, created_by, revoked_at)
VALUES (:ta, 'sha256:' || repeat('c1', 32), 'entra provisioning', 'ada@a.example', NULL),
       (:ta, 'sha256:' || repeat('c2', 32), 'old', 'ada@a.example', now()),
       (:tb, 'sha256:' || repeat('c3', 32), 'okta provisioning', 'bo@b.example', NULL);

INSERT INTO ops.scim_user (tenant_id, scim_id, user_name_hash, resource_enc, user_ref)
VALUES (:ta, 'c0000000-0000-4000-8000-00000000000a', sha256('\x61'::bytea), '\x00'::bytea, 'u_' || repeat('a', 32)),
       (:tb, 'c0000000-0000-4000-8000-00000000000b', sha256('\x62'::bytea), '\x00'::bytea, 'u_' || repeat('b', 32));

INSERT INTO ops.scim_group (tenant_id, scim_id, display_name)
VALUES (:ta, 'd0000000-0000-4000-8000-00000000000a', 'Finance'),
       (:tb, 'd0000000-0000-4000-8000-00000000000b', 'Engineering');

INSERT INTO ops.scim_group_member (tenant_id, group_id, user_id)
VALUES (:ta, 'd0000000-0000-4000-8000-00000000000a', 'c0000000-0000-4000-8000-00000000000a'),
       (:tb, 'd0000000-0000-4000-8000-00000000000b', 'c0000000-0000-4000-8000-00000000000b');

-- The same device-derived ref is an alias in both tenants, to different people, so T67 can show the
-- resolution never crosses tenants.
INSERT INTO ops.user_ref_alias (tenant_id, alias_ref, user_ref)
VALUES (:ta, 'u_' || repeat('1', 32), 'u_' || repeat('a', 32)),
       (:tb, 'u_' || repeat('1', 32), 'u_' || repeat('b', 32));

INSERT INTO ops.deployment_key (tenant_id, key_hash, label, created_by)
VALUES (:ta, 'sha256:' || repeat('d1', 32), 'intune package', 'ada@a.example'),
       (:tb, 'sha256:' || repeat('d2', 32), 'zip package', 'bo@b.example');

SET ROLE sac_control;
SET app.tenant_id = '22222222-2222-7222-8222-222222222222';

DO $$
DECLARE
  t text;
  n_other int;
  n_own int;
  n_none int;
  tables text[] := ARRAY['ops.identity_connection', 'ops.tenant_email_domain', 'ops.onboarding_invite',
                         'ops.role_grant', 'ops.auth_session', 'ops.scim_token', 'ops.scim_user',
                         'ops.scim_group', 'ops.scim_group_member', 'ops.user_ref_alias',
                         'ops.deployment_key'];
BEGIN
  -- Tenant B, holding sac_control's full grant, sees its own rows (so the check is not vacuous) and
  -- none of tenant A's; with no tenant set it sees nothing.
  FOREACH t IN ARRAY tables LOOP
    EXECUTE format('SELECT count(*) FROM %s WHERE tenant_id = %L', t, '11111111-1111-7111-8111-111111111111') INTO n_other;
    EXECUTE format('SELECT count(*) FROM %s', t) INTO n_own;
    IF n_other <> 0 THEN
      RAISE EXCEPTION 'FAIL T54 tenant B sees % of tenant A''s rows in %', n_other, t;
    END IF;
    IF n_own = 0 THEN
      RAISE EXCEPTION 'FAIL T54 tenant B sees none of its own rows in %; the isolation check would be vacuous', t;
    END IF;
  END LOOP;

  PERFORM set_config('app.tenant_id', '', false);
  FOREACH t IN ARRAY tables LOOP
    EXECUTE format('SELECT count(*) FROM %s', t) INTO n_none;
    IF n_none <> 0 THEN
      RAISE EXCEPTION 'FAIL T54 with no tenant set, % rows of % are visible', n_none, t;
    END IF;
  END LOOP;

  RAISE NOTICE 'PASS T54 all 11 identity/provisioning/deployment tables are isolated by forced RLS, and fail closed with no tenant';
END $$;

SET app.tenant_id = '';

DO $$
DECLARE
  n int;
  v_tenant uuid;
BEGIN
  -- An Entra tid resolves to its connection only while that connection is active, matched
  -- case-insensitively.
  SELECT count(*) INTO n FROM ops.identity_connection_for_entra('A0A0A0A0-0000-4000-8000-0000000000A1');
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T55 the active Entra connection did not resolve (% rows)', n; END IF;
  SELECT tenant_id INTO v_tenant FROM ops.identity_connection_for_entra('a0a0a0a0-0000-4000-8000-0000000000a1');
  IF v_tenant <> '11111111-1111-7111-8111-111111111111' THEN
    RAISE EXCEPTION 'FAIL T55 the Entra tid resolved to tenant %', v_tenant;
  END IF;
  SELECT count(*) INTO n FROM ops.identity_connection_for_entra('b0b0b0b0-0000-4000-8000-0000000000b1');
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T55 a PENDING Entra connection resolved for sign-in'; END IF;
  RAISE NOTICE 'PASS T55 identity_connection_for_entra returns only an active connection, with no tenant set';

  -- An OIDC issuer resolves exactly and only while active; by id returns any status.
  SELECT count(*) INTO n FROM ops.identity_connection_for_issuer('https://idp.b.example');
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T56 the active issuer did not resolve (% rows)', n; END IF;
  SELECT count(*) INTO n FROM ops.identity_connection_for_issuer('https://idp.b.example/');
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T56 a different spelling of the issuer resolved; the comparison must be exact'; END IF;
  SELECT count(*) INTO n FROM ops.identity_connection_for_issuer('https://idp.a.example/disabled');
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T56 a DISABLED OIDC connection resolved for sign-in'; END IF;
  SELECT count(*) INTO n FROM ops.identity_connection_by_id('f0000000-0000-4000-8000-00000000a002');
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T56 identity_connection_by_id did not return a disabled connection'; END IF;
  RAISE NOTICE 'PASS T56 identity_connection_for_issuer is exact and active-only; identity_connection_by_id returns any status';

  IF ops.tenant_for_email_domain('A.Example') IS DISTINCT FROM '11111111-1111-7111-8111-111111111111'::uuid THEN
    RAISE EXCEPTION 'FAIL T57 a.example did not resolve to tenant A';
  END IF;
  IF ops.tenant_for_email_domain('unknown.example') IS NOT NULL THEN
    RAISE EXCEPTION 'FAIL T57 an unknown domain resolved to a tenant';
  END IF;
  RAISE NOTICE 'PASS T57 tenant_for_email_domain maps a domain to its one tenant and an unknown domain to none';

  SELECT count(*) INTO n FROM ops.onboarding_invite_by_hash('sha256:' || repeat('a1', 32));
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T58 an unused, unexpired invite did not resolve'; END IF;
  SELECT count(*) INTO n FROM ops.onboarding_invite_by_hash('sha256:' || repeat('a2', 32));
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T58 a USED invite resolved; it could onboard a second admin'; END IF;
  SELECT count(*) INTO n FROM ops.onboarding_invite_by_hash('sha256:' || repeat('a3', 32));
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T58 an EXPIRED invite resolved'; END IF;
  RAISE NOTICE 'PASS T58 onboarding_invite_by_hash returns only an unused, unexpired invite';

  SELECT count(*) INTO n FROM ops.auth_session_by_hash(sha256('\x73657373612d6c697665'::bytea));
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T59 a live session did not resolve'; END IF;
  SELECT count(*) INTO n FROM ops.auth_session_by_hash(sha256('\x73657373612d7265766f6b6564'::bytea));
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T59 a REVOKED session resolved'; END IF;
  SELECT count(*) INTO n FROM ops.auth_session_by_hash(sha256('\x73657373612d65787069726564'::bytea));
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T59 an EXPIRED session resolved'; END IF;
  RAISE NOTICE 'PASS T59 auth_session_by_hash returns only an unrevoked, unexpired session';

  IF ops.tenant_for_scim_token('sha256:' || repeat('c1', 32)) IS DISTINCT FROM '11111111-1111-7111-8111-111111111111'::uuid THEN
    RAISE EXCEPTION 'FAIL T60 a live SCIM token did not resolve to tenant A';
  END IF;
  IF ops.tenant_for_scim_token('sha256:' || repeat('c2', 32)) IS NOT NULL THEN
    RAISE EXCEPTION 'FAIL T60 a REVOKED SCIM token still names a tenant';
  END IF;
  RAISE NOTICE 'PASS T60 tenant_for_scim_token resolves only an unrevoked token';
END $$;

-- The cross-tenant lookups are callable only by the role that needs each one, and the sign-in table
-- is sac_control's alone.
SET ROLE sac_query;

DO $$
DECLARE
  fn text;
  calls text[] := ARRAY[
    'SELECT ops.tenant_for_email_domain(''a.example'')',
    'SELECT ops.tenant_for_scim_token(''x'')',
    'SELECT count(*) FROM ops.identity_connection_for_entra(''x'')',
    'SELECT count(*) FROM ops.identity_connection_for_issuer(''x'')',
    'SELECT count(*) FROM ops.identity_connection_by_id(gen_random_uuid())',
    'SELECT count(*) FROM ops.onboarding_invite_by_hash(''x'')',
    'SELECT count(*) FROM ops.auth_session_by_hash(''\x00''::bytea)',
    'SELECT count(*) FROM ops.tenant_ids()'];
BEGIN
  FOREACH fn IN ARRAY calls LOOP
    BEGIN
      EXECUTE fn;
      RAISE EXCEPTION 'FAIL T61 sac_query could run a cross-tenant lookup: %', fn;
    EXCEPTION WHEN insufficient_privilege THEN NULL;
    END;
  END LOOP;
  BEGIN
    PERFORM count(*) FROM ops.auth_signin;
    RAISE EXCEPTION 'FAIL T61 sac_query could read ops.auth_signin';
  EXCEPTION WHEN insufficient_privilege THEN NULL;
  END;
  RAISE NOTICE 'PASS T61 the eight cross-tenant lookups and ops.auth_signin are refused to sac_query';
END $$;

SET ROLE sac_control;
SET app.tenant_id = '';

DO $$
DECLARE
  n int;
BEGIN
  INSERT INTO ops.auth_signin (attempt_hash, state, nonce, redirect_uri, expires_at)
  VALUES (sha256('\x617474656d7074'::bytea), 'state', 'nonce', 'https://app.example/callback', now() + interval '10 minutes');
  SELECT count(*) INTO n FROM ops.auth_signin WHERE attempt_hash = sha256('\x617474656d7074'::bytea);
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T61 sac_control cannot read back its own sign-in attempt'; END IF;
  DELETE FROM ops.auth_signin WHERE attempt_hash = sha256('\x617474656d7074'::bytea);
  RAISE NOTICE 'PASS T61 (cont.) sac_control writes, reads and sweeps ops.auth_signin with no tenant set';
END $$;

SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
BEGIN
  -- Each refusal is a mapping that would let the wrong party decide a tenant, or a grant that
  -- silently grants nothing.
  BEGIN
    INSERT INTO ops.identity_connection (tenant_id, provider, entra_tenant_id)
    VALUES ('11111111-1111-7111-8111-111111111111', 'entra', 'A0A0A0A0-0000-4000-8000-0000000000FF');
    RAISE EXCEPTION 'FAIL T62 an upper-case Entra tid was accepted; the mapping key has one spelling';
  EXCEPTION WHEN check_violation THEN NULL; END;

  BEGIN
    INSERT INTO ops.identity_connection (tenant_id, provider, issuer)
    VALUES ('11111111-1111-7111-8111-111111111111', 'oidc', 'https://idp.a.example/no-client');
    RAISE EXCEPTION 'FAIL T62 an OIDC connection without a client id was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  BEGIN
    INSERT INTO ops.identity_connection (tenant_id, provider, issuer, client_id, status)
    VALUES ('11111111-1111-7111-8111-111111111111', 'oidc', 'https://idp.a.example/unattributed', 'c', 'active');
    RAISE EXCEPTION 'FAIL T62 an active connection with no activated_at/activated_by was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  BEGIN
    INSERT INTO ops.identity_connection (tenant_id, provider, issuer, client_id, role_map)
    VALUES ('11111111-1111-7111-8111-111111111111', 'oidc', 'https://idp.a.example/badmap', 'c',
            '{"SAC-Admins": "superuser"}'::jsonb);
    RAISE EXCEPTION 'FAIL T62 a role_map onto a role the product does not have was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  BEGIN
    INSERT INTO ops.identity_connection (tenant_id, provider, entra_tenant_id, client_id)
    VALUES ('11111111-1111-7111-8111-111111111111', 'entra', 'a0a0a0a0-0000-4000-8000-0000000000fe', 'own-app');
    RAISE EXCEPTION 'FAIL T62 an Entra connection carrying its own client id was accepted; Entra uses the vendor app';
  EXCEPTION WHEN check_violation THEN NULL; END;

  -- The issuer and the Entra tid are unique across the database.
  BEGIN
    INSERT INTO ops.identity_connection (tenant_id, provider, issuer, client_id)
    VALUES ('11111111-1111-7111-8111-111111111111', 'oidc', 'https://idp.b.example', 'client-a2');
    RAISE EXCEPTION 'FAIL T62 a second tenant claimed an issuer that already maps to tenant B';
  EXCEPTION WHEN unique_violation THEN NULL; END;

  INSERT INTO ops.identity_connection (tenant_id, provider, issuer, client_id, role_map)
  VALUES ('11111111-1111-7111-8111-111111111111', 'oidc', 'https://idp.a.example/ok', 'c',
          '{"SAC-Admins": "admin", "SAC-Readers": "content_reader"}'::jsonb);

  RAISE NOTICE 'PASS T62 a connection''s mapping keys are canonical and unique, activation is attributed, and role_map names only product roles';
END $$;

DO $$
BEGIN
  -- A session always carries at least one product role.
  BEGIN
    INSERT INTO ops.auth_session (session_hash, tenant_id, connection_id, subject, actor, roles, expires_at)
    VALUES (sha256('\x6e6f2d726f6c65'::bytea), '11111111-1111-7111-8111-111111111111',
            'f0000000-0000-4000-8000-00000000a001', 'oid-x', 'x@a.example', ARRAY[]::text[], now() + interval '1 hour');
    RAISE EXCEPTION 'FAIL T63 a session with no role was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  BEGIN
    INSERT INTO ops.auth_session (session_hash, tenant_id, connection_id, subject, actor, roles, expires_at)
    VALUES (sha256('\x6465762d726f6c65'::bytea), '11111111-1111-7111-8111-111111111111',
            'f0000000-0000-4000-8000-00000000a001', 'oid-x', 'x@a.example', ARRAY['dev'], now() + interval '1 hour');
    RAISE EXCEPTION 'FAIL T63 a session carrying a role the product does not have was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  BEGIN
    INSERT INTO ops.auth_session (session_hash, tenant_id, connection_id, subject, actor, roles, expires_at)
    VALUES ('\x00112233'::bytea, '11111111-1111-7111-8111-111111111111',
            'f0000000-0000-4000-8000-00000000a001', 'oid-x', 'x@a.example', ARRAY['viewer'], now() + interval '1 hour');
    RAISE EXCEPTION 'FAIL T63 a session keyed by something other than a sha256 was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  -- A session cannot point at another tenant's connection.
  BEGIN
    INSERT INTO ops.auth_session (session_hash, tenant_id, connection_id, subject, actor, roles, expires_at)
    VALUES (sha256('\x63726f7373'::bytea), '11111111-1111-7111-8111-111111111111',
            'f0000000-0000-4000-8000-00000000b001', 'sub-b', 'bo@b.example', ARRAY['viewer'], now() + interval '1 hour');
    RAISE EXCEPTION 'FAIL T63 a tenant A session referencing tenant B''s connection was accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;

  RAISE NOTICE 'PASS T63 a session holds one or more product roles, is keyed by a sha256, and names its own tenant''s connection';
END $$;

DO $$
DECLARE
  v_env bytea := convert_to('{"key_id":"policy-key-1","algorithm":"ed25519","payload":{},"signature":""}', 'UTF8');
BEGIN
  -- The policy bundle's digest is the sha256 of the bytes served, when they are stored.
  BEGIN
    INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix,
                                   retention_class, signature_kid, signed_digest, created_by, signed_envelope)
    VALUES ('11111111-1111-7111-8111-111111111111', 100, '{"default": "m1"}'::jsonb,
            'standard', 'policy-key-1', 'sha256:' || repeat('00', 32), 'control-api', v_env);
    RAISE EXCEPTION 'FAIL T64 a bundle whose digest does not name its envelope was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix,
                                 retention_class, signature_kid, signed_digest, created_by, signed_envelope)
  VALUES ('11111111-1111-7111-8111-111111111111', 100, '{"default": "m1"}'::jsonb,
          'standard', 'policy-key-1', 'sha256:' || encode(sha256(v_env), 'hex'), 'control-api', v_env);

  RAISE NOTICE 'PASS T64 a stored policy envelope and its signed_digest cannot disagree';
END $$;

DO $$
BEGIN
  -- A sign-in email domain is lower-case and maps to one tenant.
  BEGIN
    INSERT INTO ops.tenant_email_domain (domain, tenant_id) VALUES ('Upper.Example', '11111111-1111-7111-8111-111111111111');
    RAISE EXCEPTION 'FAIL T65 an upper-case email domain was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO ops.tenant_email_domain (domain, tenant_id) VALUES ('b.example', '11111111-1111-7111-8111-111111111111');
    RAISE EXCEPTION 'FAIL T65 tenant A claimed tenant B''s email domain';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  RAISE NOTICE 'PASS T65 an email domain is lower-case and maps to exactly one tenant';
END $$;

DO $$
BEGIN
  -- One Intune device is one product device in a tenant.
  UPDATE ops.device SET intune_device_id = 'intune-0001'
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND device_id = 'aaaaaaaa-0000-7000-8000-000000000001';
  INSERT INTO ops.device (tenant_id, device_id, os, managed_state)
  VALUES ('11111111-1111-7111-8111-111111111111', 'aaaaaaaa-0000-7000-8000-0000000000f7', 'windows', 'managed');
  BEGIN
    UPDATE ops.device SET intune_device_id = 'intune-0001'
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND device_id = 'aaaaaaaa-0000-7000-8000-0000000000f7';
    RAISE EXCEPTION 'FAIL T66 two devices in one tenant were bound to the same Intune device id';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  RAISE NOTICE 'PASS T66 an Intune device id binds to at most one device per tenant';
END $$;

-- record_event stores the canonical person. Run as sac_ingest, so the alias read is the grant and
-- the policy.
SET ROLE sac_ingest;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  r record;
  v_obs text;
  v_sub text;
  v_dev text;
  v_alias constant text := 'u_' || repeat('1', 32);
  v_canonical constant text := 'u_' || repeat('a', 32);
  v_unknown constant text := 'u_' || repeat('2', 32);
BEGIN
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version', '1.0',
    'event_id', 'e0000000-0000-7000-8000-0000000000a1',
    'tenant_id', '11111111-1111-7111-8111-111111111111',
    'device_id', 'aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref', v_alias,
    'tool_fingerprint', 'probe-alias-1',
    'direction', 'none', 'kind', 'model_detection',
    'occurred_at', '2026-10-05T10:00:00Z',
    'monotonic_offset_ms', 1,
    'source', 'proc.detect', 'collection_mode', 'm0',
    'detection_basis', 'process_scan',
    'dedup_key', 'sha256:' || repeat('e1', 32)
  ), now() + interval '1 hour');

  SELECT o.user_ref INTO v_obs FROM ingest.observation o
   WHERE o.tenant_id = '11111111-1111-7111-8111-111111111111' AND o.event_id = 'e0000000-0000-7000-8000-0000000000a1';
  SELECT s.user_ref INTO v_sub FROM ingest.submission s
   WHERE s.tenant_id = '11111111-1111-7111-8111-111111111111' AND s.submission_id = r.event_submission_id;
  SELECT d.last_user_ref INTO v_dev FROM ops.device d
   WHERE d.tenant_id = '11111111-1111-7111-8111-111111111111' AND d.device_id = 'aaaaaaaa-0000-7000-8000-000000000001';
  IF v_obs IS DISTINCT FROM v_canonical OR v_sub IS DISTINCT FROM v_canonical OR v_dev IS DISTINCT FROM v_canonical THEN
    RAISE EXCEPTION 'FAIL T67 an aliased ref was stored as observation=% submission=% device=%, expected % (tenant A''s canonical, not tenant B''s)',
      v_obs, v_sub, v_dev, v_canonical;
  END IF;

  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version', '1.0',
    'event_id', 'e0000000-0000-7000-8000-0000000000a2',
    'tenant_id', '11111111-1111-7111-8111-111111111111',
    'device_id', 'aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref', v_unknown,
    'tool_fingerprint', 'probe-alias-2',
    'direction', 'none', 'kind', 'model_detection',
    'occurred_at', '2026-10-05T10:00:00Z',
    'monotonic_offset_ms', 2,
    'source', 'proc.detect', 'collection_mode', 'm0',
    'detection_basis', 'process_scan',
    'dedup_key', 'sha256:' || repeat('e2', 32)
  ), now() + interval '2 hours');

  SELECT o.user_ref INTO v_obs FROM ingest.observation o
   WHERE o.tenant_id = '11111111-1111-7111-8111-111111111111' AND o.event_id = 'e0000000-0000-7000-8000-0000000000a2';
  SELECT s.user_ref INTO v_sub FROM ingest.submission s
   WHERE s.tenant_id = '11111111-1111-7111-8111-111111111111' AND s.submission_id = r.event_submission_id;
  IF v_obs IS DISTINCT FROM v_unknown OR v_sub IS DISTINCT FROM v_unknown THEN
    RAISE EXCEPTION 'FAIL T67 a ref with no alias was rewritten: observation=% submission=%, expected %', v_obs, v_sub, v_unknown;
  END IF;

  RAISE NOTICE 'PASS T67 record_event stores an aliased ref as the tenant''s canonical ref and an unknown ref as sent';
END $$;

RESET ROLE;


-- =====================================================================================
-- T68  The jobs list every tenant through ops.tenant_ids()
-- =====================================================================================

DO $$ BEGIN PERFORM set_config('sac_test.n_tenants', (SELECT count(*) FROM ops.tenant)::text, false); END $$;

SET ROLE sac_ops;
SET app.tenant_id = '';

DO $$
DECLARE
  n_direct int;
  n_listed int;
BEGIN
  -- Forced row-level security hides ops.tenant from a session with no tenant...
  SELECT count(*) INTO n_direct FROM ops.tenant;
  IF n_direct <> 0 THEN RAISE EXCEPTION 'FAIL T68 sac_ops with no tenant set read % tenant rows directly', n_direct; END IF;
  -- ...so the jobs list tenants through the definer function, which returns every id.
  SELECT count(*) INTO n_listed FROM ops.tenant_ids();
  IF n_listed <> current_setting('sac_test.n_tenants')::int THEN
    RAISE EXCEPTION 'FAIL T68 ops.tenant_ids() returned % ids, expected %', n_listed, current_setting('sac_test.n_tenants');
  END IF;
  RAISE NOTICE 'PASS T68 ops.tenant_ids() lists all % tenants for sac_ops, which cannot read ops.tenant unscoped', n_listed;
END $$;

RESET ROLE;


-- =====================================================================================
-- T69  Expiring a submission takes its findings and reviews with it
-- =====================================================================================
-- Findings are derived and a review judges one submission, so neither outlives it. The expire job
-- holds no grant on either table: the cascade runs as the table owner.

DO $$
DECLARE v_sub uuid;
BEGIN
  SELECT submission_id INTO v_sub FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND tool_fingerprint = 'probe-alias-1';
  IF v_sub IS NULL THEN RAISE EXCEPTION 'FAIL T69 setup: the submission T67 recorded is missing'; END IF;
  PERFORM set_config('sac_test.doomed_submission', v_sub::text, false);

  INSERT INTO mart.finding (tenant_id, submission_id, rule_id, detected_at, decided_locally, collection_mode)
  VALUES ('11111111-1111-7111-8111-111111111111', v_sub, 'PAYMENT_CARD_PAN', now(), true, 'm1');
  INSERT INTO ops.finding_review (tenant_id, submission_id, rule_id, review_state, reviewed_by, reviewed_at)
  VALUES ('11111111-1111-7111-8111-111111111111', v_sub, 'PAYMENT_CARD_PAN', 'confirmed', 'analyst@example.test', now());
END $$;

SET ROLE sac_ops;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE n int;
BEGIN
  DELETE FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND submission_id = current_setting('sac_test.doomed_submission')::uuid;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T69 the expiry delete removed % submissions, expected 1', n; END IF;
END $$;

RESET ROLE;

DO $$
DECLARE n_finding int; n_review int;
BEGIN
  SELECT count(*) INTO n_finding FROM mart.finding
   WHERE submission_id = current_setting('sac_test.doomed_submission')::uuid;
  SELECT count(*) INTO n_review FROM ops.finding_review
   WHERE submission_id = current_setting('sac_test.doomed_submission')::uuid;
  IF n_finding <> 0 OR n_review <> 0 THEN
    RAISE EXCEPTION 'FAIL T69 % finding(s) and % review(s) outlived their submission', n_finding, n_review;
  END IF;
  RAISE NOTICE 'PASS T69 deleting a submission as sac_ops removes its findings and reviews';
END $$;


-- =====================================================================================
-- T70  content-vault reads what storing and retaining content needs, and no more
-- =====================================================================================

SET ROLE sac_vault;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
  v_ttl int;
BEGIN
  -- The submission's prompt kind and labels decide indexing and retention.
  SELECT count(*) INTO n FROM (SELECT submission_id, prompt_kind, labels FROM ingest.submission) s;
  IF n = 0 THEN RAISE EXCEPTION 'FAIL T70 content-vault sees no submissions to read prompt_kind and labels from'; END IF;

  -- The severity order and the 'content' default come from ref.
  SELECT count(*) INTO n FROM ref.data_class;
  SELECT default_ttl_days INTO v_ttl FROM ref.retention_class WHERE retention_class = 'content';
  IF n = 0 OR v_ttl IS NULL THEN RAISE EXCEPTION 'FAIL T70 content-vault cannot read the data classes or the content retention default'; END IF;

  -- Nothing else from the submission: not the excerpt, not the policy decision.
  BEGIN
    PERFORM policy_action FROM ingest.submission;
    RAISE EXCEPTION 'FAIL T70 content-vault can read submission policy decisions';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  BEGIN
    PERFORM content_excerpt FROM ingest.observation;
    RAISE EXCEPTION 'FAIL T70 content-vault can read observation excerpts';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;

  -- A retrieval grant for content stored before its event joined a submission.
  INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                   principal, case_reference, second_approver, issued_at, expires_at,
                                   raw_digest)
  VALUES ('11111111-1111-7111-8111-111111111111', 'cccccccc-0000-7000-8000-000000000071',
          'e0000000-0000-7000-8000-0000000000c1', '0b000000-0000-7000-8000-000000000001', NULL,
          'analyst@example.test', 'CASE-71', 'approver@example.test', now(), now() + interval '1 hour',
          'sha256:' || repeat('c1', 32));
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T70 a retrieval grant without a submission was refused'; END IF;

  RAISE NOTICE 'PASS T70 content-vault reads prompt kind, labels and retention reference data, nothing more, and may grant retrieval of content with no submission';
END $$;

RESET ROLE;


-- =====================================================================================
-- T71-T73  Who moves a submission's content_state
-- =====================================================================================
-- record_event marks an M3 prompt local_only (the device holds its content); content-vault marks it
-- uploaded when it stores the content; the expire job marks it shredded when the content expires.

SET ROLE sac_ingest;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  r record;
  v_state text;
BEGIN
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version','1.0',
    'event_id','e0000000-0000-7000-8000-0000000000d1',
    'tenant_id','11111111-1111-7111-8111-111111111111',
    'device_id','aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref','u_test','tool_fingerprint','genai.web.chat.v1:content-state',
    'direction','egress','kind','prompt',
    'occurred_at','2026-11-03T09:00:00Z','monotonic_offset_ms',1,
    'source','ext.page_context','collection_mode','m3',
    'dedup_key','sha256:' || repeat('f1', 32),'size_bytes',100,
    'content_digest','sha256:' || repeat('f2', 32),
    'labels', jsonb_build_array(jsonb_build_object('class','health','score',0.9)),
    'classifier_version','test-2026.01','confidence','high',
    'policy_decision', jsonb_build_object('rule_id','R','action','logged','decided_locally',true)
  ), now());
  SELECT content_state INTO v_state FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND submission_id = r.event_submission_id;
  IF v_state IS DISTINCT FROM 'local_only' THEN
    RAISE EXCEPTION 'FAIL T71 an M3 prompt was recorded as %, expected local_only', v_state;
  END IF;
  PERFORM set_config('sac_test.m3_submission', r.event_submission_id::text, false);

  SELECT content_state INTO v_state FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND tool_fingerprint = 'genai.web.chat.v1:adopt-equal';
  IF v_state IS DISTINCT FROM 'not_captured' THEN
    RAISE EXCEPTION 'FAIL T71 an M1 prompt was recorded as %, expected not_captured', v_state;
  END IF;
  RAISE NOTICE 'PASS T71 record_event marks an M3 prompt local_only and leaves an M1 prompt not_captured';
END $$;

RESET ROLE;

-- control-api grants the upload.
DO $$
BEGIN
  INSERT INTO ops.grant (tenant_id, grant_id, event_id, submission_id, device_id, decided_at, decision, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', '9a000000-0000-7000-8000-0000000000d1',
          'e0000000-0000-7000-8000-0000000000d1', current_setting('sac_test.m3_submission')::uuid,
          'aaaaaaaa-0000-7000-8000-000000000001', now(), 'granted', now() + interval '10 minutes');
END $$;

SET ROLE sac_vault;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
  v_sub uuid := current_setting('sac_test.m3_submission')::uuid;
BEGIN
  -- content-vault's upload transaction: claim, store, mark uploaded. The object is stored already
  -- expired so the expire job finds it in T73.
  UPDATE ops.grant SET used_at = now()
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND grant_id = '9a000000-0000-7000-8000-0000000000d1'
     AND used_at IS NULL AND decision = 'granted' AND expires_at > now();
  INSERT INTO ops.content (tenant_id, object_id, event_id, submission_id, grant_id, key_version, ciphertext,
                           plaintext_size_bytes, raw_digest, retention_class, created_at, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111', '0b000000-0000-7000-8000-0000000000d1',
          'e0000000-0000-7000-8000-0000000000d1', v_sub, '9a000000-0000-7000-8000-0000000000d1', 'v1',
          decode(repeat('ab', 128), 'hex'), 100, 'sha256:' || repeat('f2', 32), 'content',
          now() - interval '2 days', now() - interval '1 day');
  UPDATE ingest.submission SET content_state = 'uploaded'
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND submission_id = v_sub
     AND content_state IN ('not_captured', 'local_only');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T72 content-vault could not mark the submission uploaded (% rows)', n; END IF;

  -- content_state and nothing else.
  BEGIN
    UPDATE ingest.submission SET size_bytes = 1
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND submission_id = v_sub;
    RAISE EXCEPTION 'FAIL T72 content-vault changed a submission column other than content_state';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  BEGIN
    UPDATE ingest.submission SET shredded_reason = 'erasure'
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND submission_id = v_sub;
    RAISE EXCEPTION 'FAIL T72 content-vault set a shredded reason';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  RAISE NOTICE 'PASS T72 content-vault marks a submission uploaded and can change no other submission column';
END $$;

RESET ROLE;
SET ROLE sac_ops;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
  v_state text;
  v_reason text;
BEGIN
  -- The expire job deletes expired content and marks each still-existing submission shredded.
  WITH gone AS (
    DELETE FROM ops.content
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND expires_at <= now()
    RETURNING submission_id
  )
  UPDATE ingest.submission s
     SET content_state = 'shredded', shredded_reason = 'retention_expired'
    FROM gone
   WHERE s.tenant_id = '11111111-1111-7111-8111-111111111111'
     AND s.submission_id = gone.submission_id
     AND s.content_state = 'uploaded';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T73 the expire job marked % submissions shredded, expected 1', n; END IF;

  SELECT content_state, shredded_reason INTO v_state, v_reason FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND submission_id = current_setting('sac_test.m3_submission')::uuid;
  IF v_state <> 'shredded' OR v_reason <> 'retention_expired' THEN
    RAISE EXCEPTION 'FAIL T73 the submission is %/% after expiry, expected shredded/retention_expired', v_state, v_reason;
  END IF;

  BEGIN
    PERFORM ciphertext FROM ops.content;
    RAISE EXCEPTION 'FAIL T73 the expire job can read ciphertext';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  RAISE NOTICE 'PASS T73 the expire job marks expired content shredded with its reason and still cannot read ciphertext';
END $$;

RESET ROLE;


-- =====================================================================================
-- T74  control-api decides a content grant from metadata and counts stored bytes
-- =====================================================================================

SET ROLE sac_control;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  v_kind text;
  v_sub uuid;
  v_before bigint;
  v_after bigint;
BEGIN
  -- The device's own observation of the event, and the submission it belongs to.
  SELECT o.kind, s.submission_id INTO v_kind, v_sub
    FROM ingest.observation o
    LEFT JOIN ingest.submission s ON s.tenant_id = o.tenant_id AND s.dedup_key = o.dedup_key
   WHERE o.tenant_id = '11111111-1111-7111-8111-111111111111'
     AND o.event_id = 'e0000000-0000-7000-8000-0000000000d1'
     AND o.device_id = 'aaaaaaaa-0000-7000-8000-000000000001';
  IF v_kind IS DISTINCT FROM 'prompt' OR v_sub IS NULL THEN
    RAISE EXCEPTION 'FAIL T74 control-api could not read the event context (kind %, submission %)', v_kind, v_sub;
  END IF;

  -- Nothing derived from the prompt.
  BEGIN
    PERFORM labels FROM ingest.observation;
    RAISE EXCEPTION 'FAIL T74 control-api can read observation labels';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  BEGIN
    PERFORM content_excerpt FROM ingest.observation;
    RAISE EXCEPTION 'FAIL T74 control-api can read observation excerpts';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  BEGIN
    PERFORM content_digest FROM ingest.submission;
    RAISE EXCEPTION 'FAIL T74 control-api can read submission content digests';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;

  -- Counting a stored upload: the content counter and nothing else.
  SELECT coalesce(content_bytes_added, 0) INTO v_before FROM ops.usage_daily
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND usage_day = current_date;
  INSERT INTO ops.usage_daily (tenant_id, usage_day, content_bytes_added)
  VALUES ('11111111-1111-7111-8111-111111111111', current_date, 100)
  ON CONFLICT (tenant_id, usage_day) DO UPDATE
     SET content_bytes_added = ops.usage_daily.content_bytes_added + EXCLUDED.content_bytes_added;
  SELECT content_bytes_added INTO v_after FROM ops.usage_daily
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND usage_day = current_date;
  IF v_after IS DISTINCT FROM coalesce(v_before, 0) + 100 THEN
    RAISE EXCEPTION 'FAIL T74 content_bytes_added went from % to %, expected +100', v_before, v_after;
  END IF;

  BEGIN
    UPDATE ops.usage_daily SET events_accepted = events_accepted + 1
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND usage_day = current_date;
    RAISE EXCEPTION 'FAIL T74 control-api changed the accepted-event counter';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  BEGIN
    INSERT INTO ops.usage_daily (tenant_id, usage_day, events_accepted)
    VALUES ('11111111-1111-7111-8111-111111111111', current_date + 1, 1);
    RAISE EXCEPTION 'FAIL T74 control-api wrote an accepted-event count';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;

  RAISE NOTICE 'PASS T74 control-api reads only the event metadata a grant decision needs and can add only content bytes to the ledger';
END $$;

RESET ROLE;


-- =====================================================================================
-- Report
-- =====================================================================================

DO $$
DECLARE
  n_sub int; n_obs int; n_audit int;
BEGIN
  SELECT count(*) INTO n_sub FROM ingest.submission;
  SELECT count(*) INTO n_obs FROM ingest.observation;
  SELECT count(*) INTO n_audit FROM ops.audit;
  RAISE NOTICE 'summary: % submissions, % observations, % audit rows across % tenants',
    n_sub, n_obs, n_audit, (SELECT count(*) FROM ops.tenant);
  RAISE NOTICE 'all invariant tests completed';
END $$;
