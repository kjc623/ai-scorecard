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
