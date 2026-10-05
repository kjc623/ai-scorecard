-- =====================================================================================
-- Shadow AI Capture — schema invariant tests
-- =====================================================================================
-- Run after db/schema.sql against a real PostgreSQL server:
--
--   psql -v ON_ERROR_STOP=1 -f db/schema.sql
--   psql -v ON_ERROR_STOP=1 -f db/invariants.test.sql
--
-- Every block either prints PASS or raises, which with ON_ERROR_STOP=1 fails the run. The
-- point is that the properties the design claims are properties the database enforces, not
-- intentions in a document: the collection-mode boundary, the tenant-isolation guarantee, the
-- dedup tie-break, the policy ceiling, and the tamper-evidence of the audit log.
--
-- The tests deliberately run as the runtime roles rather than as a superuser, because a
-- superuser bypasses row-level security and would therefore prove nothing about it.
-- =====================================================================================

\set ON_ERROR_STOP on
\set QUIET on
SET client_min_messages = notice;

-- Terms used throughout.
\set ta '''11111111-1111-7111-8111-111111111111'''
\set tb '''22222222-2222-7222-8222-222222222222'''
\set dev_a '''aaaaaaaa-0000-7000-8000-000000000001'''
\set dev_b '''bbbbbbbb-0000-7000-8000-000000000002'''
\set ev1 '''e0000000-0000-7000-8000-000000000001'''
\set ev2 '''e0000000-0000-7000-8000-000000000002'''
\set ev3 '''e0000000-0000-7000-8000-000000000003'''

-- =====================================================================================
-- Fixtures
-- =====================================================================================

INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                        ceiling_mode, content_budget_bytes_per_day)
VALUES
  (:ta, 'Tenant A (M3 ceiling, vendor-held key)', 'active', 'eastus', 'vendor',
   'https://kv.example/keys/ta', 'm3', 107374182400),
  (:tb, 'Tenant B (M1 ceiling, no key at all)', 'active', 'eastus', 'vendor',
   NULL, 'm1', 0);

INSERT INTO ops.device (tenant_id, device_id, os, managed_state)
VALUES (:ta, :dev_a, 'windows', 'managed'),
       (:tb, :dev_b, 'macos', 'managed');

INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, public_key_thumbprint, expires_at)
VALUES (:ta, gen_random_uuid(), :dev_a, 'thumb-a', now() + interval '30 days'),
       (:tb, gen_random_uuid(), :dev_b, 'thumb-b', now() + interval '30 days');

-- A retention policy that differs from the standard class, so materialisation is observable.
INSERT INTO ops.retention_policy (tenant_id, applies_to, data_class, collection_mode, ttl_days, updated_by)
VALUES (:ta, 'event', 'payment_card', 'm1', 30, 'test');

INSERT INTO ref.classifier_release (release_version, ruleset_version, model_version,
                                    artifact_digest, state, promoted_at)
VALUES ('test-2026.01', 'rules-test', 'model-test', 'sha256:' || repeat('ab', 32), 'enforcing', now());

DO $$ BEGIN RAISE NOTICE 'fixtures loaded'; END $$;


-- =====================================================================================
-- T1-T4  Policy ceiling (brief C3)
-- =====================================================================================
-- C3: "the mode in force is applied before a policy bundle is signed, so a device can never
-- exceed its tenant's ceiling". Enforced by the ops.enforce_policy_ceiling trigger.

DO $$
BEGIN
  INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix, classifier_release,
                                 retention_class, signature_kid, signed_digest, created_by)
  VALUES ('11111111-1111-7111-8111-111111111111', 1,
          '{"tools": {"chatgpt": "m2", "claude": "m1"}, "default": "m1"}'::jsonb,
          'test-2026.01', 'standard', 'kid-test', 'sha256:' || repeat('cd', 32), 'test');
  RAISE NOTICE 'PASS T1 bundle within ceiling accepted';
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix, classifier_release,
                                   retention_class, signature_kid, signed_digest, created_by)
    VALUES ('22222222-2222-7222-8222-222222222222', 1,
            '{"tools": {"chatgpt": "m3"}, "default": "m1"}'::jsonb,
            'test-2026.01', 'standard', 'kid-test', 'sha256:' || repeat('ce', 32), 'test');
    RAISE EXCEPTION 'FAIL T2 bundle exceeding the M1 ceiling was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T2 bundle exceeding ceiling rejected (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix, classifier_release,
                                   retention_class, signature_kid, signed_digest, created_by)
    VALUES ('11111111-1111-7111-8111-111111111111', 2,
            '{"tools": {"chatgpt": "m9"}, "default": "m1"}'::jsonb,
            'test-2026.01', 'standard', 'kid-test', 'sha256:' || repeat('cf', 32), 'test');
    RAISE EXCEPTION 'FAIL T3 bundle with an unrecognised mode was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T3 unrecognised mode rejected (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.policy_bundle (tenant_id, bundle_version, scope_matrix, classifier_release,
                                   retention_class, signature_kid, signed_digest, created_by)
    VALUES ('11111111-1111-7111-8111-111111111111', 3, '{}'::jsonb,
            'test-2026.01', 'standard', 'kid-test', 'sha256:' || repeat('d0', 32), 'test');
    RAISE EXCEPTION 'FAIL T4 empty scope matrix was accepted as unrestricted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T4 empty scope matrix rejected (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T5-T8  Idempotency and the dedup tie-break (brief C12, R9)
-- =====================================================================================
-- Three observations of one logical submission: a low-fidelity route first, then the same
-- event replayed, then a high-fidelity route. Expected: 3 observation rows, 1 submission,
-- the submission upgraded to the better route, and the replay counted as a duplicate.

SET ROLE sac_ingest;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  r record;
  n int;
BEGIN
  -- Route 1: the egress proxy, rank 50. Low fidelity.
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

  -- Route 1 replayed verbatim. A retry must be free (brief C12).
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

  -- The higher-fidelity route must have taken over the content fields.
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

-- T9: retention was materialised from the tenant policy (payment_card, m1 -> 30 days).
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
-- T25-T26  The two-tier dedup ladder (brief R9 under the M0 constraint)
-- =====================================================================================
-- Brief §4.1 derives the dedup key from tenant, device, tool, a normalised content digest and
-- a time bucket. Brief §1.1 forbids the collector from reading content at M0, so that formula
-- cannot be evaluated there. The case must not silently double-count, so the server derives a
-- weaker key from the same formula minus the digest, and an observation that later supplies a
-- real digest adopts the row rather than creating a twin.

DO $$
DECLARE
  r record;
  n int;
  k text;
BEGIN
  -- Two M0 observations of one submission, from two different routes. Distinct event_ids and
  -- distinct device-computed dedup_keys, but the same device, tool, bucket and size.
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
    RAISE EXCEPTION 'FAIL T25 first M0 observation expected inserted, got %', r.event_outcome;
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
    RAISE EXCEPTION 'FAIL T25 two M0 routes did not collapse, got %', r.event_outcome;
  END IF;

  SELECT count(*) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T25 expected 2 submissions total, found % (M0 routes double-counted)', n;
  END IF;

  SELECT dedup_key, merge_confidence, observation_count INTO k, r.event_outcome, n
    FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:claude';
  IF k IS NOT NULL THEN
    RAISE EXCEPTION 'FAIL T25 a weak-only submission should have no exact key, got %', k;
  END IF;
  IF r.event_outcome <> 'low' THEN
    RAISE EXCEPTION 'FAIL T25 weak-merged row should be flagged low confidence, got %', r.event_outcome;
  END IF;
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T25 expected 2 observations on the merged row, got %', n;
  END IF;
  RAISE NOTICE 'PASS T25 two M0 routes collapsed to one submission, flagged low confidence';

  -- Now the same submission is seen by a route that CAN read content, in the same bucket and
  -- at the same size. It must adopt the weak-only row rather than create a second one.
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
    RAISE EXCEPTION 'FAIL T26 an exact observation created a twin instead of adopting, now % submissions', n;
  END IF;

  SELECT dedup_key INTO k FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:claude';
  IF k <> 'sha256:' || repeat('b1', 32) THEN
    RAISE EXCEPTION 'FAIL T26 weak-only row was not adopted and given the exact key, got %', k;
  END IF;
  RAISE NOTICE 'PASS T26 exact observation adopted the weak-only row instead of double-counting';
END $$;

-- T27: a rollup and a detection for the same tool in the same bucket must stay two records.
-- Neither carries a size, so without `kind` in the weak key they would collapse into one --
-- a fabrication rather than a merge, and one that would erase mode I evidence entirely.
DO $$
DECLARE
  r record;
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
    RAISE EXCEPTION 'FAIL T27 expected a rollup and a detection to stay separate, found %', n;
  END IF;
  RAISE NOTICE 'PASS T27 rollup and detection kept separate (kind is part of the weak key)';
END $$;


-- =====================================================================================
-- T10-T11  The collection-mode boundary (brief C2, §1.1)
-- =====================================================================================
-- M0 is a permission boundary: at M0 the collector must not have read content at all, so a
-- content-derived field on an M0 row is evidence of a defect. The database refuses it.

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
    RAISE EXCEPTION 'FAIL T10 an M0 observation carrying a content digest was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T10 M0 row with content rejected (%)', SQLERRM;
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
    RAISE EXCEPTION 'FAIL T11 an M1 prompt with no labels was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T11 M1 prompt without classifier output rejected (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T12-T14  Tenant isolation (brief C32)
-- =====================================================================================
-- C32 requires a cross-tenant read to be "structurally impossible, not merely unauthorised".
-- Still running as sac_ingest, which is a non-owner without BYPASSRLS, so these assertions
-- are about the storage layer rather than about application code.

DO $$
DECLARE n int;
BEGIN
  -- Tenant A can see its own rows (two prompt submissions, one rollup, one detection).
  SELECT count(*) INTO n FROM ingest.submission;
  IF n <> 4 THEN
    RAISE EXCEPTION 'FAIL T12 tenant A expected 4 own submissions, saw %', n;
  END IF;
  RAISE NOTICE 'PASS T12 tenant A sees its own data (%)', n;

  -- Tenant B sees nothing of A's.
  PERFORM set_config('app.tenant_id', '22222222-2222-7222-8222-222222222222', false);
  SELECT count(*) INTO n FROM ingest.submission;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T13 tenant B saw % of tenant A rows', n;
  END IF;
  SELECT count(*) INTO n FROM ingest.observation;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T13 tenant B saw % of tenant A observations', n;
  END IF;
  RAISE NOTICE 'PASS T13 tenant B sees zero of tenant A rows';

  -- No tenant set at all: fail closed, zero rows rather than all rows.
  PERFORM set_config('app.tenant_id', '', false);
  SELECT count(*) INTO n FROM ingest.submission;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T14 a session with no tenant saw % rows', n;
  END IF;
  RAISE NOTICE 'PASS T14 unset tenant reads zero rows (fails closed)';
END $$;

-- Back to tenant A, then attempt a cross-tenant write. The WITH CHECK clause must refuse it.
DO $$
BEGIN
  PERFORM set_config('app.tenant_id', '11111111-1111-7111-8111-111111111111', false);
  BEGIN
    INSERT INTO ingest.observation (
      tenant_id, event_id, device_id, user_ref, tool_fingerprint, direction, kind,
      occurred_at, received_at, monotonic_offset_ms, source, collection_mode,
      size_bytes, dedup_key, schema_version, expires_at)
    VALUES ('22222222-2222-7222-8222-222222222222',       -- a different tenant
            'e0000000-0000-7000-8000-00000000000c',
            'aaaaaaaa-0000-7000-8000-000000000001', 'u_test', 'x', 'egress', 'prompt',
            now(), now(), 1, 'ext.web_request', 'm0', 10, 'sha256:' || repeat('66', 32),
            '1.0', now());
    RAISE EXCEPTION 'FAIL T15 a write into another tenant was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T15 cross-tenant write refused (%)', SQLERRM;
  END;
END $$;

RESET ROLE;


-- =====================================================================================
-- T16-T17  Append-only audit with a hash chain (brief §3.2, §3.6, §4.4)
-- =====================================================================================

INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id, detail)
VALUES (:ta, 'user', 'analyst@example.com', 'event.read', 'submission', 'sub-1',
        jsonb_build_object('rows', 25, 'reason', 'investigation')),
       (:ta, 'user', 'analyst@example.com', 'content.reveal', 'content_object', 'obj-1',
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
    RAISE EXCEPTION 'FAIL T16 audit rows have no hash';
  END IF;
  IF p2 <> h1 THEN
    RAISE EXCEPTION 'FAIL T16 hash chain broken: row 2 prev_hash % <> row 1 row_hash %', p2, h1;
  END IF;
  RAISE NOTICE 'PASS T16 audit hash chain links row 2 to row 1';
END $$;

DO $$
BEGIN
  BEGIN
    UPDATE ops.audit SET detail = '{"tampered": true}'::jsonb
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T17 an audit row was updated';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T17 audit UPDATE refused (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    DELETE FROM ops.audit WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T18 an audit row was deleted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T18 audit DELETE refused (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T19-T20  Immutability of observations, and no content in quarantine
-- =====================================================================================

DO $$
BEGIN
  BEGIN
    UPDATE ingest.observation SET size_bytes = 1
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T19 an observation was updated in place';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T19 in-place observation UPDATE refused (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    DELETE FROM ingest.observation
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T20 an observation was deleted outside the retention path';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T20 observation DELETE outside retention refused (%)', SQLERRM;
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
    RAISE EXCEPTION 'FAIL T21 quarantine accepted a content excerpt';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T21 quarantine refuses stored content (%)', SQLERRM;
  END;
END $$;


-- =====================================================================================
-- T22-T23  The states that must never be merged (brief §3.2)
-- =====================================================================================

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.content_object (
      tenant_id, object_id, blob_path, ciphertext_sha256, plaintext_size_bytes,
      wrapped_dek, kek_id, kek_version, retention_class, state)
    VALUES ('11111111-1111-7111-8111-111111111111', gen_random_uuid(),
            'ta/2026/10/obj1', 'sha256:' || repeat('77', 32), 1024,
            '\x00'::bytea, 'https://kv.example/keys/ta', 'v1', 'content', 'shredded');
    RAISE EXCEPTION 'FAIL T22 shredded without a reason was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T22 shredded-without-reason refused (%)', SQLERRM;
  END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.tool (tenant_id, tool_fingerprint, sanctioned_state, decided_by, decided_at)
    VALUES ('11111111-1111-7111-8111-111111111111', 'x', 'unsanctioned', NULL, NULL);
    RAISE EXCEPTION 'FAIL T23 an unsanctioned decision with no actor was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T23 unattributed sanction decision refused (%)', SQLERRM;
  END;
END $$;

DO $$
DECLARE n int;
BEGIN
  -- A new tool defaults to `unknown`, which brief C8 requires to stay distinct from
  -- `unsanctioned`. A default of `unsanctioned` would report every undiscovered tool as
  -- prohibited, which is a different and much more alarming claim.
  INSERT INTO ops.tool (tenant_id, tool_fingerprint)
  VALUES ('11111111-1111-7111-8111-111111111111', 'genai.web.chat.v1:newtool');

  SELECT count(*) INTO n FROM ops.tool
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:newtool'
     AND sanctioned_state = 'unknown';
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T24 a newly discovered tool did not default to unknown';
  END IF;
  RAISE NOTICE 'PASS T24 newly discovered tool defaults to unknown, not unsanctioned';
END $$;


-- =====================================================================================
-- T28-T31  Content search: the key/search exclusivity, and the index dying with its row
-- =====================================================================================
-- Brief §3.5 says customer-held keys and server-side content search cannot both exist.
-- ADR 0014 turns that from a policy note into a check constraint.

DO $$
BEGIN
  BEGIN
    INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                            ceiling_mode, content_search)
    VALUES ('33333333-3333-7333-8333-333333333333', 'Impossible tenant', 'active', 'eusth',
            'customer_held', 'https://kv.example/keys/tc', 'm3', 'full_text');
    RAISE EXCEPTION 'FAIL T28 full_text search with customer-held keys was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T28 customer-held keys with full_text search refused (%)', SQLERRM;
  END;
END $$;

DO $$
DECLARE n int;
BEGIN
  -- The two combinations that must work: filename search with the strongest custody, and
  -- prompt-text search with a vendor-readable key.
  INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                          ceiling_mode, content_search)
  VALUES ('44444444-4444-7444-8444-444444444444', 'Filename-search tenant', 'active', 'eusth',
          'customer_held', NULL, 'm1', 'attachment_names');

  INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                          ceiling_mode, content_search)
  VALUES ('55555555-5555-7555-8555-555555555555', 'Full-text tenant', 'active', 'eusth',
          'vendor', 'https://kv.example/keys/td', 'm3', 'full_text');

  SELECT count(*) INTO n FROM ops.tenant WHERE content_search <> 'disabled';
  IF n <> 2 THEN
    RAISE EXCEPTION 'FAIL T29 expected 2 search-enabled tenants, found %', n;
  END IF;
  RAISE NOTICE 'PASS T29 attachment_names works with customer-held keys, full_text with vendor keys';
END $$;

-- T32: a search tier over data the tenant cannot collect is refused. Filenames cross at M1 and
-- prompt text only at M3, so these two rows would each grant a capability over nothing.
DO $$
BEGIN
  BEGIN
    INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                            ceiling_mode, content_search)
    VALUES ('66666666-6666-7666-8666-666666666666', 'Full-text without M3', 'active', 'eusth',
            'vendor', 'https://kv.example/keys/te', 'm1', 'full_text');
    RAISE EXCEPTION 'FAIL T32 full_text search on an M1 ceiling was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T32 full_text search without an M3 ceiling refused (%)', SQLERRM;
  END;

  BEGIN
    INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                            ceiling_mode, content_search)
    VALUES ('77777777-7777-7777-8777-777777777777', 'Filenames without M1', 'active', 'eusth',
            'vendor', NULL, 'm0', 'attachment_names');
    RAISE EXCEPTION 'FAIL T32 attachment_names search on an M0 ceiling was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T32 attachment_names search on an M0 ceiling refused (%)', SQLERRM;
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

  -- The generated column must actually be populated and matchable; a generated tsvector that
  -- silently produced an empty vector would make search look implemented while returning nothing.
  SELECT count(*) INTO n FROM ingest.search_text
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tsv @@ to_tsquery('simple', 'contract');
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T30 the generated tsvector did not index the body (matched %)', n;
  END IF;

  -- Filename matching is substring, not token, which is what an investigator actually wants.
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
-- T33-T35  Commercial state: a human decides, so the decision must be informed and recorded
-- =====================================================================================
-- Suspension is not automated. That moves the risk out of the system and into the process, so
-- what the schema owes is: an unattributed closure is impossible, and the operator can see what
-- a closure would cost before making it.

DO $$
DECLARE n int;
BEGIN
  BEGIN
    UPDATE ops.tenant SET read_enabled = false
     WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
    RAISE EXCEPTION 'FAIL T33 a gate was closed with no recorded actor or reason';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS T33 closing a gate without attribution refused (%)', SQLERRM;
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
    RAISE EXCEPTION 'FAIL T33 an attributed gate closure did not apply';
  END IF;
  RAISE NOTICE 'PASS T33 gate closed with a recorded actor, time and reason';

  UPDATE ops.tenant
     SET read_enabled = true, status_reason = NULL,
         status_changed_by = NULL, status_changed_at = NULL
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
END $$;

-- T34: the bill must not move when data is erased. This is the whole reason the ledger is
-- written forward and never derived from ingest -- brief §3.4 requires erasure to remove data,
-- and docs/03-data-platform.md §7 makes that the argument for replace-the-bucket aggregates.
-- Billing pulls the other way: the tenant consumed the service.
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
    RAISE EXCEPTION 'FAIL T34 the ledger did not record the accepted count (got %)', before_events;
  END IF;

  SELECT count(*) INTO subs_before FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF subs_before = 0 THEN
    RAISE EXCEPTION 'FAIL T34 nothing to erase; the test proves nothing';
  END IF;

  -- Erase, exactly as the reconciler does: submissions, then observations through the
  -- retention path, and the search index by cascade.
  DELETE FROM ingest.submission WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  PERFORM set_config('sac.retention_delete', 'on', false);
  DELETE FROM ingest.observation WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  PERFORM set_config('sac.retention_delete', 'off', false);

  SELECT count(*) INTO subs_after FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF subs_after <> 0 THEN
    RAISE EXCEPTION 'FAIL T34 erasure did not remove the submissions';
  END IF;

  SELECT events_accepted INTO after_events FROM ops.usage_daily
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111' AND usage_day = current_date;
  IF after_events <> before_events THEN
    RAISE EXCEPTION 'FAIL T34 the bill moved when data was erased (% -> %)', before_events, after_events;
  END IF;
  RAISE NOTICE 'PASS T34 % submissions erased, usage ledger unchanged at % events',
    subs_before, after_events;
END $$;

-- T35: the operator can see what closing a gate would strand, before closing it.
DO $$
DECLARE r record;
BEGIN
  SELECT * INTO r FROM mart.v_tenant_suspension_impact
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF r.tenant_id IS NULL THEN
    RAISE EXCEPTION 'FAIL T35 the suspension impact view returned no row for an existing tenant';
  END IF;
  IF r.ingest_enabled IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'FAIL T35 the view reports ingest as closed for an open tenant';
  END IF;
  RAISE NOTICE 'PASS T35 suspension impact view reports % enrolled device(s), % event(s) spooled',
    r.devices_enrolled, r.events_spooled_on_devices;
END $$;


-- =====================================================================================
-- T36-T37  The adopt path at EQUAL fidelity (regression; added after the original T1-T35)
-- =====================================================================================
-- T25-T26 exercise adoption only when the exact observation arrives on a strictly better
-- ranked route (ext.page_context, rank 10, against a proxy.tls winner, rank 50). The equal and
-- worse ranked cases were therefore untested, and the equal one was broken: the adopt UPDATE
-- took the exact key with `coalesce(s.dedup_key, ...)` but wrote content_digest only when the
-- route strictly outranked the row's winner, so the row ended up with a non-NULL dedup_key and
-- a NULL content_digest and CHECK submission_exact_key_implies_digest rejected the UPDATE. The
-- transaction aborted, which docs/02 section 6 makes a permanently retrying batch.
--
-- Reported by task-3 (ingestor) with a live reproduction; reproduced independently, fixed in
-- db/schema.sql. Both observations below use ext.web_request, whose ref.route_fidelity rank is
-- 40 -- equal rank is the entire point. T37 additionally pins the two consequences the fix
-- asserts: the adopted row must carry the digest with the key, and must stop being reported as
-- a submission that could not be merged.
--
-- These run as sac_ingest, like the rest of the dedup ladder, so they hold for the runtime role
-- the ingest API actually uses.

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
  -- E1: first sighting, M0, so this route could not read content: a weak-only row, no exact key.
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
    RAISE EXCEPTION 'FAIL T36 first M0 observation expected inserted, got %', r.event_outcome;
  END IF;

  -- E2: the same submission seen by a route that CAN read content, on a route of the SAME rank
  -- and in the same 300-second bucket. It must adopt the weak-only row: not raise, not twin.
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
    RAISE EXCEPTION 'FAIL T36 equal-rank exact observation did not adopt the weak-only row, got %', r.event_outcome;
  END IF;

  SELECT count(*) INTO n FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:adopt-equal';
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T36 equal-rank adoption double-counted: % submissions', n;
  END IF;
  RAISE NOTICE 'PASS T36 equal-rank exact observation adopted the weak-only row without raising or double-counting';

  -- T37: the pair must arrive together, and an adopted row is no longer one we could not merge.
  SELECT dedup_key, content_digest, merge_confidence INTO k, d, c
    FROM ingest.submission
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111'
     AND tool_fingerprint = 'genai.web.chat.v1:adopt-equal';
  IF k <> 'sha256:' || repeat('2b', 32) THEN
    RAISE EXCEPTION 'FAIL T37 adopted row did not take the exact key, got %', k;
  END IF;
  IF d <> 'sha256:' || repeat('3c', 32) THEN
    RAISE EXCEPTION 'FAIL T37 adopted row took the exact key without the digest it was derived from, got %', d;
  END IF;
  IF c <> 'high' THEN
    RAISE EXCEPTION 'FAIL T37 adopted row holds an exact key but is still flagged %, so it would be counted among the submissions we could not merge', c;
  END IF;
  RAISE NOTICE 'PASS T37 adopted row carries the exact key with its digest and is no longer flagged low';
END $$;


-- =====================================================================================
-- T38  The quarantine reason-code vocabulary is pinned
-- =====================================================================================
-- The Lead's ruling: ingest.rejected.reason_code carries the docs/02 §7 wire vocabulary,
-- spelled exactly as device/protocol.ReasonCode spells it, plus three storage-only codes that
-- have no per-event wire outcome. Three codes used to be spelled differently here for the same
-- facts (device_revoked, batch_oversize, schema_version_unsupported); §7 is the closed contract,
-- so the quarantine surface follows it rather than translating to it.
--
-- This pins the vocabulary from the database's side, so the Go mapping and the CHECK cannot
-- drift apart silently in either direction. It runs as sac_ingest, the role the ingest API
-- actually uses, and it exercises the real INSERT rather than reading the catalog: a constraint
-- that exists but is not enforced is not a constraint.

DO $$
DECLARE
  accepted text[] := ARRAY[
    'schema_violation','unsupported_schema_version','unknown_kind','unknown_tenant',
    'revoked_device','region_mismatch','mode_violation','oversize',
    'malformed_json','dedup_key_mismatch','internal_error'];
  forbidden text[] := ARRAY[
    -- wire-only: deliberately unrepresentable in quarantine, see the table comment
    'tenant_mismatch','duplicate_batch',
    -- the pre-alignment spellings of three codes above; their return would be a regression
    'device_revoked','batch_oversize','schema_version_unsupported'];
  c text;
  n int;
  inserted int := 0;
BEGIN
  FOREACH c IN ARRAY accepted LOOP
    BEGIN
      INSERT INTO ingest.rejected (tenant_id, device_id, reason_code, expires_at)
      VALUES ('11111111-1111-7111-8111-111111111111',
              'aaaaaaaa-0000-7000-8000-000000000001', c, now() + interval '7 days');
      -- Count with ROW_COUNT rather than SELECT: sac_ingest holds INSERT but deliberately not
      -- SELECT on ingest.rejected, and widening the grant to make a test convenient would
      -- weaken the least-privilege property this suite exists to protect.
      GET DIAGNOSTICS n = ROW_COUNT;
      inserted := inserted + n;
    EXCEPTION WHEN others THEN
      RAISE EXCEPTION 'FAIL T38 the CHECK rejects %, which is part of the agreed vocabulary (%)', c, SQLERRM;
    END;
  END LOOP;

  FOREACH c IN ARRAY forbidden LOOP
    BEGIN
      INSERT INTO ingest.rejected (tenant_id, device_id, reason_code, expires_at)
      VALUES ('11111111-1111-7111-8111-111111111111',
              'aaaaaaaa-0000-7000-8000-000000000001', c, now() + interval '7 days');
      RAISE EXCEPTION 'FAIL T38 the CHECK accepts %, which is not part of the vocabulary', c;
    EXCEPTION WHEN check_violation THEN
      NULL;  -- expected: the vocabulary is enforced, not merely documented
    END;
  END LOOP;

  IF inserted <> array_length(accepted, 1) THEN
    RAISE EXCEPTION 'FAIL T38 expected % accepted codes to be written, % were', array_length(accepted, 1), inserted;
  END IF;
  RAISE NOTICE 'PASS T38 quarantine vocabulary pinned: % codes written, % rejected (2 wire-only, 3 pre-alignment spellings)',
    inserted, array_length(forbidden, 1);
END $$;


-- =====================================================================================
-- T39-T42  The store's kind and mode boundaries match the contract, field for field
-- =====================================================================================
-- These cover a systematic asymmetry found by the verifier while checking ADR 0018: the store's
-- per-kind CHECK constraints constrained only some of the fields the contract forbids for that
-- kind. A record arriving by any path other than the ingest gate -- a migration, a restore, a
-- support query, a future service -- was accepted carrying a field its kind is defined not to
-- have. The database is the last line that does not depend on application code being correct, so
-- a half-enforced boundary is worse than none: it reads as a guarantee.
--
-- Two halves, deliberately split, because either alone can pass while the property is broken:
--   * db/tools/check-schema.mjs proves each forbid-list is EQUAL to the contract's not.anyOf, so
--     nothing is missing (a list could be enforced but incomplete);
--   * these assertions prove the fields are actually enforced against the live server, so
--     nothing in the list is decorative (a list could be complete but unenforced).
-- Each assertion ends with a POSITIVE CONTROL: a well-formed row of the same kind must still be
-- accepted. Without it the assertion would pass just as well if the constraint rejected
-- everything, which is the failure mode a one-directional test cannot see.
--
-- `attachments` appears in every contract forbid-list and in none of these, because it has no
-- column on ingest.observation -- attachment descriptors are envelope-level and their store-side
-- home is ingest.search_text. A CHECK cannot see another table; the checker expects its absence.

DO $$
DECLARE
  -- the contract's model_detection not.anyOf, minus attachments (no column on this table)
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
      RAISE EXCEPTION 'FAIL T39 model_detection accepted %, which the contract forbids for this kind', f;
    EXCEPTION WHEN check_violation THEN
      NULL;
    END;
  END LOOP;

  -- Positive control: the well-formed detection must still be accepted.
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
    RAISE EXCEPTION 'FAIL T39 the well-formed model_detection was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T39 model_detection refuses all % contract-forbidden fields and still accepts a valid one', array_length(forbids, 1);
END $$;

DO $$
DECLARE
  -- the contract's usage_rollup not.anyOf, minus attachments
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
      RAISE EXCEPTION 'FAIL T40 usage_rollup accepted %, which the contract forbids for this kind', f;
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
    RAISE EXCEPTION 'FAIL T40 the well-formed usage_rollup was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T40 usage_rollup refuses all % contract-forbidden fields and still accepts a valid one', array_length(forbids, 1);
END $$;

DO $$
DECLARE
  -- the contract's prompt not.anyOf: the four window fields and detection_basis
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
      RAISE EXCEPTION 'FAIL T41 prompt accepted %, which the contract forbids for this kind', f;
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
    RAISE EXCEPTION 'FAIL T41 the well-formed prompt was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T41 prompt refuses all % contract-forbidden fields and still accepts a valid one', array_length(forbids, 1);
END $$;

DO $$
DECLARE
  -- the contract's M0 prompt not.anyOf, minus attachments. prompt_kind is the newest: it is
  -- request-shape metadata, not content, but M0's closed list has no room for it because a
  -- metadata-only device read no body from which to decide one.
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
      RAISE EXCEPTION 'FAIL T42 an M0 record accepted %, which is evidence the collector read content it was not permitted to read', f;
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
    RAISE EXCEPTION 'FAIL T42 the well-formed M0 record was not accepted, so the refusals above prove nothing';
  END IF;
  RAISE NOTICE 'PASS T42 an M0 record refuses all % content-derived fields and a clean M0 record is still accepted', array_length(forbids, 1);
END $$;

-- T43: M0 must refuse `confidence`, on its own and by name.
--
-- T42 covers this inside a loop, but this is the single field the schema owner most wants to see
-- fail if someone reverts the constraint, so it gets an assertion whose PASS line and FAIL message
-- both name it. `confidence` was the field the store was missing when this was written: it is a
-- classifier output, so an M0 record carrying it is evidence the collector read content it was not
-- permitted to read, which is precisely what that constraint exists to catch. (`prompt_kind` was
-- added to the same constraint since, and is covered by T42.)
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
            'high',   -- the field under test: a classifier output on a mode that reads no content
            jsonb_build_object('rule_id','R','action','logged','decided_locally',true),
            'sha256:' || repeat('d5', 32), '1.0', now() + interval '30 days');
    RAISE EXCEPTION 'FAIL T43 an M0 record was accepted carrying `confidence`, which is a classifier output and therefore evidence the collector read content it was not permitted to read';
  EXCEPTION WHEN check_violation THEN
    NULL;  -- expected
  END;

  -- Positive control: the same record without `confidence` must still be accepted.
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
    RAISE EXCEPTION 'FAIL T43 the clean M0 record was not accepted, so the refusal above proves nothing';
  END IF;
  RAISE NOTICE 'PASS T43 an M0 record carrying `confidence` is refused, and a clean M0 record is accepted';
END $$;


-- =====================================================================================
-- T44-T45  The single-use retrieval grant (ops.retrieval_grant)
-- =====================================================================================
-- The content vault states the whole guarantee as a conditional UPDATE:
--
--   UPDATE ops.retrieval_grant SET used_at = ..., used_by = ...
--    WHERE tenant_id = ... AND grant_id = ... AND used_at IS NULL
--   RETURNING ...
--
-- and treats an UPDATE that returns nothing as the refusal. These assertions run that statement as
-- `sac_vault` -- the actual runtime role -- so they exercise the table, the row-level policy and
-- the grants together. Running them as a superuser would prove the shape of the table and nothing
-- about whether the vault can reach it.
--
-- The single-use property is tested at three levels, because only the third is structural:
--   1. the first claim redeems the grant and sets both halves of the claim;
--   2. a second claim through the guarded statement affects zero rows and leaves the first
--      redemption intact -- this is the vault's refusal mechanism, and it is evidence the
--      statement is right, not that the store would catch a wrong one;
--   3. an UPDATE that has *forgotten* the guard is refused by the trigger. This is the one that
--      does not depend on the caller being correct, and a CHECK cannot provide it: a CHECK sees
--      only the new row and cannot tell a first claim from a second.

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
  -- put_retrieval_grant: the vault's INSERT, so the INSERT grant and the policy are both exercised
  INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                   principal, case_reference, second_approver, issued_at, expires_at,
                                   raw_digest)
  VALUES (v_tenant, v_grant, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
          'analyst@example.test', 'CASE-1', 'approver@example.test', now(),
          now() + interval '1 hour', 'sha256:' || repeat('e1', 32));

  -- read_retrieval_grant: a fresh grant is unclaimed
  SELECT used_at, used_by INTO l_used_at, l_used_by
    FROM ops.retrieval_grant WHERE tenant_id = v_tenant AND grant_id = v_grant;
  IF l_used_at IS NOT NULL OR l_used_by IS NOT NULL THEN
    RAISE EXCEPTION 'FAIL T44 a freshly issued grant is already marked as redeemed';
  END IF;

  -- claim, exactly as the vault claims it
  UPDATE ops.retrieval_grant
     SET used_at = now(), used_by = 'analyst@example.test'
   WHERE tenant_id = v_tenant AND grant_id = v_grant AND used_at IS NULL
  RETURNING used_at, used_by INTO l_used_at, l_used_by;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T44 the first claim affected % rows, expected exactly 1', n;
  END IF;
  IF l_used_at IS NULL OR l_used_by IS NULL THEN
    RAISE EXCEPTION 'FAIL T44 the first claim left a half-claimed row (used_at=%, used_by=%)', l_used_at, l_used_by;
  END IF;

  -- the SAME guarded statement a second time: this is the vault's refusal path
  UPDATE ops.retrieval_grant
     SET used_at = now(), used_by = 'attacker@example.test'
   WHERE tenant_id = v_tenant AND grant_id = v_grant AND used_at IS NULL;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T44 a SECOND claim affected % rows; the grant is not single-use', n;
  END IF;

  -- and the first redemption must be untouched
  SELECT used_by INTO l_used_by FROM ops.retrieval_grant
   WHERE tenant_id = v_tenant AND grant_id = v_grant;
  IF l_used_by <> 'analyst@example.test' THEN
    RAISE EXCEPTION 'FAIL T44 the second claim overwrote the first redemption (used_by is now %)', l_used_by;
  END IF;

  RAISE NOTICE 'PASS T44 first claim redeems the grant; a second claim affects 0 rows and cannot overwrite it';
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
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T45 setup: the first claim affected % rows', n; END IF;

  -- 3. The UPDATE that forgot the guard. The row matches, so the trigger must refuse it. Asserting
  --    on the message keeps this from passing for the wrong reason: if some other constraint
  --    happened to fire, this would not contain 'single-use'.
  BEGIN
    UPDATE ops.retrieval_grant
       SET used_at = now(), used_by = 'attacker@example.test'
     WHERE tenant_id = v_tenant AND grant_id = v_grant;   -- no `used_at IS NULL`
    RAISE EXCEPTION 'FAIL T45 an UPDATE without the `used_at IS NULL` guard re-redeemed a claimed grant; single use depends on the caller remembering the guard';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('single-use' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T45 the unguarded UPDATE was refused, but not by the single-use trigger (%)', SQLERRM;
    END IF;
    RAISE NOTICE '  (unguarded UPDATE refused by the store: %)', SQLERRM;
  END;

  -- The first redemption must still be intact after the refused attempt.
  SELECT used_by INTO l_used_by FROM ops.retrieval_grant
   WHERE tenant_id = v_tenant AND grant_id = v_grant;
  IF l_used_by <> 'analyst@example.test' THEN
    RAISE EXCEPTION 'FAIL T45 the refused update still changed the row (used_by is now %)', l_used_by;
  END IF;
  RAISE NOTICE 'PASS T45 the store refuses a redemption that bypasses the `used_at IS NULL` guard';

  -- A half-claimed row is unrepresentable. Tested by INSERT rather than UPDATE on purpose: an
  -- UPDATE of a claimed row would be refused by the trigger first, and the assertion would pass
  -- without the CHECK ever firing -- which is exactly the "right result, wrong reason" failure.
  BEGIN
    INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                     principal, case_reference, second_approver, issued_at, expires_at,
                                     used_at, used_by, raw_digest)
    VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000003', gen_random_uuid(), gen_random_uuid(),
            gen_random_uuid(), 'analyst@example.test', 'CASE-3', 'approver@example.test',
            now(), now() + interval '1 hour', now(), NULL, 'sha256:' || repeat('e3', 32));
    RAISE EXCEPTION 'FAIL T45 a half-claimed row (used_at set, used_by NULL) was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_claim_is_whole' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T45 the half-claimed row was refused, but not by retrieval_grant_claim_is_whole (%)', SQLERRM;
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
    RAISE EXCEPTION 'FAIL T45 a grant whose approver is also the requester was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_second_approver_distinct' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T45 the self-approved grant was refused, but not by retrieval_grant_second_approver_distinct (%)', SQLERRM;
    END IF;
  END;

  RAISE NOTICE 'PASS T45 a half-claimed row and a self-approved grant are both unrepresentable';
END $$;

-- T46: a grant's raw_digest must be a sha256 digest.
--
-- The value is what the analyst verifies the returned bytes against (docs/02 §11), and
-- content-vault copies it verbatim from ops.content_object.ciphertext_sha256. That column already
-- constrains the shape, so this pins the shape on the copy too: a grant cannot promise a digest it
-- could not have been derived from, and the vault's own statement and this table cannot disagree
-- about what a digest looks like. The lowercase requirement is part of the pattern, so uppercase
-- hex is refused rather than quietly accepted as a second spelling of the same value.
DO $$
DECLARE
  v_tenant constant uuid := '11111111-1111-7111-8111-111111111111';
  n int;
BEGIN
  -- Positive control: the real shape is accepted.
  INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                   principal, case_reference, second_approver, issued_at, expires_at,
                                   raw_digest)
  VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000005', gen_random_uuid(), gen_random_uuid(),
          gen_random_uuid(), 'analyst@example.test', 'CASE-5', 'approver@example.test',
          now(), now() + interval '1 hour', 'sha256:' || repeat('a5', 32));
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T46 a well-formed raw_digest was not accepted'; END IF;

  -- Negative: a digest that is not one. Asserting the constraint name keeps this from passing
  -- because something else refused the row.
  BEGIN
    INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                     principal, case_reference, second_approver, issued_at, expires_at,
                                     raw_digest)
    VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000006', gen_random_uuid(), gen_random_uuid(),
            gen_random_uuid(), 'analyst@example.test', 'CASE-6', 'approver@example.test',
            now(), now() + interval '1 hour', 'not-a-digest');
    RAISE EXCEPTION 'FAIL T46 a raw_digest that is not a sha256 digest was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_raw_digest_is_sha256' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T46 the malformed digest was refused, but not by retrieval_grant_raw_digest_is_sha256 (%)', SQLERRM;
    END IF;
  END;

  -- Negative: uppercase hex is a second spelling, not the same value.
  BEGIN
    INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id,
                                     principal, case_reference, second_approver, issued_at, expires_at,
                                     raw_digest)
    VALUES (v_tenant, 'cccccccc-0000-7000-8000-000000000007', gen_random_uuid(), gen_random_uuid(),
            gen_random_uuid(), 'analyst@example.test', 'CASE-7', 'approver@example.test',
            now(), now() + interval '1 hour', 'sha256:' || repeat('A5', 32));
    RAISE EXCEPTION 'FAIL T46 an uppercase-hex raw_digest was accepted; the digest has one spelling, not two';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('retrieval_grant_raw_digest_is_sha256' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T46 the uppercase digest was refused, but not by retrieval_grant_raw_digest_is_sha256 (%)', SQLERRM;
    END IF;
  END;

  RAISE NOTICE 'PASS T46 a grant carries a lowercase sha256 digest or it is refused';
END $$;

RESET ROLE;

-- T47: ref.classifier_release.artifact_digest must be a sha256 digest too.
--
-- The value identifies the signed release artefact, and its producer is device/classifier-host:
-- `digestOf` builds every digest as "sha256:" + hex.EncodeToString(sum), which is lowercase by
-- construction, and the seed row as well as every fixture already uses that shape. T46 pins the
-- same rule on the grant's copy; this pins it on the source.
--
-- Runs as the bootstrap role because ref.* is shared reference data that no runtime role may
-- write. The one divergence worth recording: the producer's manifest check uses
-- strings.EqualFold, so it would accept a hand-written uppercase digest that this column then
-- refuses. That is the safe direction -- a loud refusal at the store rather than a silently
-- stored second spelling -- and the producer has flagged the one-line fix on their side.
DO $$
DECLARE
  n int;
BEGIN
  INSERT INTO ref.classifier_release (release_version, ruleset_version, model_version,
                                      artifact_digest, state, promoted_at)
  VALUES ('test-digest-format', 'rules-digest-test', 'model-digest-test',
          'sha256:' || repeat('7f', 32), 'enforcing', now());
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T47 a well-formed artifact_digest was not accepted'; END IF;

  BEGIN
    INSERT INTO ref.classifier_release (release_version, ruleset_version, model_version,
                                        artifact_digest, state, promoted_at)
    VALUES ('test-digest-upper', 'rules-digest-test', 'model-digest-test',
            'sha256:' || repeat('7F', 32), 'enforcing', now());
    RAISE EXCEPTION 'FAIL T47 an uppercase-hex artifact_digest was accepted; the digest has one spelling, not two';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'FAIL%' THEN RAISE; END IF;
    IF position('classifier_release_digest_is_sha256' in SQLERRM) = 0 THEN
      RAISE EXCEPTION 'FAIL T47 the uppercase digest was refused, but not by classifier_release_digest_is_sha256 (%)', SQLERRM;
    END IF;
  END;

  -- Leave the reference table as it was found.
  DELETE FROM ref.classifier_release WHERE release_version = 'test-digest-format';

  RAISE NOTICE 'PASS T47 a classifier release carries a lowercase sha256 artifact digest or it is refused';
END $$;


-- =====================================================================================
-- T48-T51  Device credentials: X.509 or DPoP, and the jti replay window (ADR 0020 §4)
-- =====================================================================================
-- ADR 0020 §4 makes the credential mode a property of the row -- an X.509 certificate or an
-- RFC 9449 DPoP key -- and makes hardware_identity_hash the per-tenant enrolment idempotency
-- key. These assertions run as the runtime roles that write each table: sac_control for
-- enrolment, sac_ingest for the replay window. Running them as a superuser would exercise the
-- shape of the tables and nothing about whether the runtime can reach them.

SET ROLE sac_control;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
BEGIN
  -- T48: a hardware identity is unique per tenant, and the partial index is what enforces it.
  -- The first insert must succeed, the duplicate must be refused, and a NULL identity must stay
  -- unconstrained -- a plain UNIQUE would have passed the first two but failed the third.
  INSERT INTO ops.device (tenant_id, device_id, os, hardware_identity_hash)
  VALUES ('11111111-1111-7111-8111-111111111111',
          'aaaaaaaa-0000-7000-8000-0000000000f1', 'windows', 'hwid-t48');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T48 the first device with a hardware identity hash was not accepted';
  END IF;

  BEGIN
    INSERT INTO ops.device (tenant_id, device_id, os, hardware_identity_hash)
    VALUES ('11111111-1111-7111-8111-111111111111',
            'aaaaaaaa-0000-7000-8000-0000000000f2', 'windows', 'hwid-t48');
    RAISE EXCEPTION 'FAIL T48 a duplicate (tenant_id, hardware_identity_hash) was accepted';
  EXCEPTION WHEN unique_violation THEN
    NULL;  -- expected: device_hardware_identity_uniq refuses the duplicate
  END;

  -- Positive control for the partial predicate: a second device with no hardware identity is
  -- still allowed, so uniqueness was not smuggled in as a blanket rule over NULLs.
  INSERT INTO ops.device (tenant_id, device_id, os, hardware_identity_hash)
  VALUES ('11111111-1111-7111-8111-111111111111',
          'aaaaaaaa-0000-7000-8000-0000000000f3', 'windows', NULL);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T48 a device with a NULL hardware identity was refused, so the partial predicate is wrong';
  END IF;

  RAISE NOTICE 'PASS T48 duplicate (tenant_id, hardware_identity_hash) refused; NULL identities remain unconstrained';
END $$;

DO $$
DECLARE
  n int;
BEGIN
  -- T49 negative: a dpop credential without its JWK is unverifiable, so the CHECK refuses it.
  BEGIN
    INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, credential_type,
                                       public_key_thumbprint, expires_at)
    VALUES ('11111111-1111-7111-8111-111111111111',
            'dddddddd-0000-7000-8000-000000000001',
            'aaaaaaaa-0000-7000-8000-000000000001',
            'dpop', 'thumb-dpop-no-jwk', now() + interval '30 days');
    RAISE EXCEPTION 'FAIL T49 a dpop credential with NULL public_key_jwk was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;  -- expected: credential_dpop_requires_jwk
  END;

  -- Positive control: the same dpop row WITH a JWK is accepted, so the CHECK refuses the missing
  -- key rather than the mode.
  INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, credential_type,
                                     public_key_thumbprint, public_key_jwk, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111',
          'dddddddd-0000-7000-8000-000000000002',
          'aaaaaaaa-0000-7000-8000-000000000001',
          'dpop', 'thumb-dpop-with-jwk',
          '{"kty":"EC","crv":"P-256","x":"test-x","y":"test-y"}'::jsonb,
          now() + interval '30 days');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T49 a dpop credential carrying a JWK was refused, so the CHECK rejects the mode rather than the missing key';
  END IF;
  RAISE NOTICE 'PASS T49 dpop credential without a JWK is refused; with a JWK it is accepted';
END $$;

DO $$
DECLARE
  n int;
BEGIN
  -- T50: an x509 credential need not carry a JWK -- the certificate carries the key -- so a NULL
  -- public_key_jwk is accepted. This is the positive half of the mode-dependent CHECK.
  INSERT INTO ops.device_credential (tenant_id, credential_id, device_id, credential_type,
                                     public_key_thumbprint, expires_at)
  VALUES ('11111111-1111-7111-8111-111111111111',
          'dddddddd-0000-7000-8000-000000000003',
          'aaaaaaaa-0000-7000-8000-000000000001',
          'x509', 'thumb-x509-no-jwk', now() + interval '30 days');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T50 an x509 credential with NULL public_key_jwk was not accepted';
  END IF;
  RAISE NOTICE 'PASS T50 x509 credential with NULL public_key_jwk is accepted';
END $$;

SET ROLE sac_ingest;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
  v_tenant constant uuid := '11111111-1111-7111-8111-111111111111';
BEGIN
  -- T51: the jti replay window is keyed (tenant_id, jti), so the same proof cannot be recorded
  -- twice. The first insert is recorded; a second, plain insert is refused by the primary key;
  -- and the ingest path's INSERT .. ON CONFLICT DO NOTHING turns that refusal into a no-op, which
  -- is what lets a replay be rejected without aborting the batch.
  INSERT INTO ops.dpop_replay (tenant_id, jti, expires_at)
  VALUES (v_tenant, 'jti-t51', now() + interval '5 minutes');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T51 the first jti was not recorded (% rows)', n;
  END IF;

  BEGIN
    INSERT INTO ops.dpop_replay (tenant_id, jti, expires_at)
    VALUES (v_tenant, 'jti-t51', now() + interval '5 minutes');
    RAISE EXCEPTION 'FAIL T51 the same (tenant_id, jti) was accepted a second time';
  EXCEPTION WHEN unique_violation THEN
    NULL;  -- expected: the tenant-leading primary key refuses the replay
  END;

  INSERT INTO ops.dpop_replay (tenant_id, jti, expires_at)
  VALUES (v_tenant, 'jti-t51', now() + interval '5 minutes')
  ON CONFLICT (tenant_id, jti) DO NOTHING;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 0 THEN
    RAISE EXCEPTION 'FAIL T51 ON CONFLICT DO NOTHING inserted the replay (% rows)', n;
  END IF;

  -- The sweep is a granted capability: the runtime role must be able to delete an expired row.
  -- seen_at is set explicitly so the row is a genuinely expired window (seen before it expired),
  -- not an inverted one that the expensive CHECK would rightly refuse.
  INSERT INTO ops.dpop_replay (tenant_id, jti, seen_at, expires_at)
  VALUES (v_tenant, 'jti-t51-expired', now() - interval '2 minutes', now() - interval '1 minute');
  DELETE FROM ops.dpop_replay
   WHERE tenant_id = v_tenant AND expires_at < now();
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'FAIL T51 the expired-jti sweep deleted % rows, expected 1', n;
  END IF;

  RAISE NOTICE 'PASS T51 a repeated (tenant_id, jti) is refused, is a no-op under ON CONFLICT DO NOTHING, and the expired row can be swept';
END $$;

-- =====================================================================================
-- T52-T54  Enrolment tokens: single-use, sha256-hashed, tenant-isolated (ADR 0020 §3, §5.1)
-- =====================================================================================
-- The bootstrap credential the MDM delivers. Only its SHA-256 hash is stored; it is single-use
-- (used_at closes it) and per-tenant unique. These run as sac_control, the role that writes it,
-- so the grants are exercised alongside the shape.

SET ROLE sac_control;
SET app.tenant_id = '11111111-1111-7111-8111-111111111111';

DO $$
DECLARE
  n int;
  v_tenant constant uuid := '11111111-1111-7111-8111-111111111111';
  v_hash constant text := 'sha256:' || repeat('ab', 32);
BEGIN
  -- T52: the hash has exactly one spelling (sha256: + 64 lowercase hex), and a token is unique
  -- per tenant. A malformed hash is refused; the same (tenant_id, token_hash) cannot be stored
  -- twice. A token with no tenant or no expiry is refused by NOT NULL.
  INSERT INTO ops.enrolment_token (tenant_id, token_hash, hardware_identity_hash, expires_at)
  VALUES (v_tenant, v_hash, 'hwid-t52', now() + interval '1 hour');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T52 a well-formed enrolment token was not accepted'; END IF;

  BEGIN
    INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
    VALUES (v_tenant, 'sha256:' || repeat('AB', 32), now() + interval '1 hour');
    RAISE EXCEPTION 'FAIL T52 an uppercase-hex token hash was accepted; the hash has one spelling';
  EXCEPTION WHEN check_violation THEN NULL; END;

  BEGIN
    INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
    VALUES (v_tenant, v_hash, now() + interval '1 hour');
    RAISE EXCEPTION 'FAIL T52 a duplicate (tenant_id, token_hash) was accepted';
  EXCEPTION WHEN unique_violation THEN NULL; END;

  RAISE NOTICE 'PASS T52 an enrolment token hash is sha256-shaped and unique per tenant';
END $$;

DO $$
DECLARE
  n int;
  v_tenant constant uuid := '11111111-1111-7111-8111-111111111111';
BEGIN
  -- T53: a token cannot expire before it was issued, and it is single-use: used_at is the only
  -- mutation the request path performs and it settles the token.
  BEGIN
    INSERT INTO ops.enrolment_token (tenant_id, token_hash, issued_at, expires_at)
    VALUES (v_tenant, 'sha256:' || repeat('cd', 32), now(), now() - interval '1 minute');
    RAISE EXCEPTION 'FAIL T53 a token expiring before it was issued was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;

  INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
  VALUES (v_tenant, 'sha256:' || repeat('ef', 32), now() + interval '1 hour');
  UPDATE ops.enrolment_token SET used_at = now()
   WHERE tenant_id = v_tenant AND token_hash = 'sha256:' || repeat('ef', 32);
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN RAISE EXCEPTION 'FAIL T53 marking a token used updated % rows, expected 1', n; END IF;

  RAISE NOTICE 'PASS T53 a token cannot expire before issue and can be marked used once';
END $$;

SET ROLE sac_control;
SET app.tenant_id = '22222222-2222-7222-8222-222222222222';

DO $$
DECLARE
  n int;
BEGIN
  -- T54: forced RLS isolates tokens. Tenant B holds the same sac_control privilege and still sees
  -- none of tenant A's tokens; the policy is the same fail-closed one every tenant table uses.
  SELECT count(*) INTO n FROM ops.enrolment_token
   WHERE tenant_id = '11111111-1111-7111-8111-111111111111';
  IF n <> 0 THEN RAISE EXCEPTION 'FAIL T54 tenant B sees % of tenant A''s enrolment tokens', n; END IF;
  RAISE NOTICE 'PASS T54 enrolment tokens are isolated by forced row-level security';
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
