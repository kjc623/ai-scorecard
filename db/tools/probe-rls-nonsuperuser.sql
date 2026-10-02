-- =====================================================================================
-- Independent RLS verification: a genuinely non-superuser session_user
-- =====================================================================================
-- Written by dbuilder. This does NOT replace db/invariants.test.sql; it is a second,
-- independent probe of the same property, built to close one specific hole.
--
-- The hole: db/invariants.test.sql asserts row-level security at lines 460-510 under
-- `SET ROLE sac_ingest`, but the *session_user* at that moment is still `postgres`, a
-- superuser. `SET ROLE` does drop the effective privileges -- RLS bypass is decided from
-- the current user id, not the session user, so sac_ingest (NOSUPERUSER, NOBYPASSRLS)
-- really is subject to the policy. That reasoning is correct, but it is reasoning, and the
-- task asks for proof. This file proves it two ways:
--
--   1. POSITIVE: the isolation assertions hold when session_user is a real login role that
--      is neither superuser nor BYPASSRLS.
--   2. DISCRIMINATING: the identical query as a superuser returns ALL 8 rows instead of 5.
--      If the RLS assertions had been vacuous, this control would not differ. A test that
--      cannot fail proves nothing; this one demonstrably can.
--
-- Run as the bootstrap role (superuser), which is legitimate here for the same reason
-- db/schema.sql itself needs it: ops.tenant's own policy forbids a runtime role from
-- creating another tenant. Every assertion below the \connect runs as sac_probe.
-- =====================================================================================

\set ON_ERROR_STOP on
SET client_min_messages = notice;

\set ta '''33333333-3333-7333-8333-333333333333'''
\set tb '''44444444-4444-7444-8444-444444444444'''
\set dev_a '''aaaaaaaa-0000-7000-8000-000000000031'''
\set dev_b '''bbbbbbbb-0000-7000-8000-000000000032'''

-- -------------------------------------------------------------------------------------
-- Fixtures, created by the bootstrap session (RLS bypassed: this is the migration path)
-- -------------------------------------------------------------------------------------

SET sac.retention_delete = on;
DELETE FROM ingest.observation WHERE tenant_id IN (:ta, :tb);
RESET sac.retention_delete;
DELETE FROM ops.device WHERE tenant_id IN (:ta, :tb);
DELETE FROM ops.tenant WHERE tenant_id IN (:ta, :tb);

INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id,
                        ceiling_mode, content_budget_bytes_per_day)
VALUES (:ta, 'Probe tenant A', 'active', 'eastus', 'vendor', NULL, 'm1', 0),
       (:tb, 'Probe tenant B', 'active', 'eastus', 'vendor', NULL, 'm1', 0);

INSERT INTO ops.device (tenant_id, device_id, os, managed_state)
VALUES (:ta, :dev_a, 'windows', 'managed'),
       (:tb, :dev_b, 'macos', 'managed');

-- Five observations for A, three for B: distinct counts, so a leak is visible as a number
-- rather than only as an error.
INSERT INTO ingest.observation (tenant_id, event_id, device_id, user_ref, tool_fingerprint,
                                direction, kind, occurred_at, received_at, monotonic_offset_ms,
                                source, confidence, collection_mode, size_bytes, content_digest,
                                labels, classifier_version, dedup_key, schema_version, expires_at,
                                policy_decision)
SELECT t.tenant, gen_random_uuid(), t.dev, 'u_probe', 'genai.web.chat.v1:chatgpt',
       'egress', 'prompt', now(), now(), g.i, 'ext.page_context', 'high', 'm1', 1024,
       'sha256:' || repeat('aa', 32),
       '[{"class": "payment_card", "score": 0.9}]'::jsonb,
       'test-2026.01',
       'sha256:' || encode(sha256((t.tenant::text || ':' || g.i::text)::bytea), 'hex'), '1.0',
       now() + interval '30 days',
       jsonb_build_object('rule_id', 'POL-PROBE', 'action', 'logged', 'decided_locally', true)
  FROM (VALUES (:ta::uuid, :dev_a::uuid, 5), (:tb::uuid, :dev_b::uuid, 3)) AS t(tenant, dev, n)
  CROSS JOIN LATERAL generate_series(1, t.n) AS g(i);

DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM ingest.observation
   WHERE tenant_id IN ('33333333-3333-7333-8333-333333333333',
                       '44444444-4444-7444-8444-444444444444');
  IF n <> 8 THEN RAISE EXCEPTION 'PROBE FAIL setup: expected 8 rows, found %', n; END IF;
  RAISE NOTICE 'setup: 8 probe observations committed (5 for A, 3 for B)';
END $$;

-- A real login role with neither SUPERUSER nor BYPASSRLS -- the actual runtime posture.
DROP ROLE IF EXISTS sac_probe;
CREATE ROLE sac_probe LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT sac_ingest TO sac_probe;

-- -------------------------------------------------------------------------------------
-- From here on, session_user is sac_probe.
-- -------------------------------------------------------------------------------------

\connect - sac_probe

DO $$
DECLARE
  u text; s boolean; b boolean; n int;
BEGIN
  SELECT current_user, rolsuper, rolbypassrls INTO u, s, b
    FROM pg_roles WHERE rolname = session_user;
  IF s THEN RAISE EXCEPTION 'PROBE FAIL session_user is a superuser; the probe proves nothing'; END IF;
  IF b THEN RAISE EXCEPTION 'PROBE FAIL session_user has BYPASSRLS; the probe proves nothing'; END IF;
  RAISE NOTICE 'PASS P0 session_user=% is NOSUPERUSER and NOBYPASSRLS', session_user;
END $$;

SET ROLE sac_ingest;

-- P1: fail closed. No tenant set -> zero rows, not all rows.
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM ingest.observation;
  IF n <> 0 THEN RAISE EXCEPTION 'PROBE FAIL P1 expected 0 rows with no tenant set, found %', n; END IF;
  RAISE NOTICE 'PASS P1 no tenant set reads 0 rows (fails closed)';
END $$;

-- P2: tenant A sees exactly its own 5.
SET app.tenant_id = '33333333-3333-7333-8333-333333333333';
DO $$
DECLARE n int; foreign_rows int;
BEGIN
  SELECT count(*) INTO n FROM ingest.observation;
  IF n <> 5 THEN RAISE EXCEPTION 'PROBE FAIL P2 tenant A expected 5 rows, found %', n; END IF;
  SELECT count(*) INTO foreign_rows FROM ingest.observation
   WHERE tenant_id = '44444444-4444-7444-8444-444444444444';
  IF foreign_rows <> 0 THEN
    RAISE EXCEPTION 'PROBE FAIL P2 tenant A can see % of tenant B rows', foreign_rows;
  END IF;
  RAISE NOTICE 'PASS P2 tenant A sees 5/5 of its own rows and 0 of tenant B''s';
END $$;

-- P3: tenant B sees exactly its own 3, and none of A's.
SET app.tenant_id = '44444444-4444-7444-8444-444444444444';
DO $$
DECLARE n int; foreign_rows int;
BEGIN
  SELECT count(*) INTO n FROM ingest.observation;
  IF n <> 3 THEN RAISE EXCEPTION 'PROBE FAIL P3 tenant B expected 3 rows, found %', n; END IF;
  SELECT count(*) INTO foreign_rows FROM ingest.observation
   WHERE tenant_id = '33333333-3333-7333-8333-333333333333';
  IF foreign_rows <> 0 THEN
    RAISE EXCEPTION 'PROBE FAIL P3 tenant B can see % of tenant A rows', foreign_rows;
  END IF;
  RAISE NOTICE 'PASS P3 tenant B sees 3/3 of its own rows and 0 of tenant A''s';
END $$;

-- P4: a cross-tenant write is refused, not silently accepted.
DO $$
BEGIN
  BEGIN
    INSERT INTO ingest.observation (tenant_id, event_id, device_id, user_ref, tool_fingerprint,
                                    direction, kind, occurred_at, received_at,
                                    monotonic_offset_ms, source, confidence, collection_mode,
                                    size_bytes, content_digest, labels, classifier_version,
                                    dedup_key, schema_version, expires_at, policy_decision)
    VALUES ('33333333-3333-7333-8333-333333333333', gen_random_uuid(),
            'aaaaaaaa-0000-7000-8000-000000000031', 'u_probe', 'genai.web.chat.v1:chatgpt',
            'egress', 'prompt', now(), now(), 99, 'ext.page_context', 'high', 'm1', 1024,
            'sha256:' || repeat('aa', 32),
            '[{"class": "payment_card", "score": 0.9}]'::jsonb, 'test-2026.01',
            'sha256:' || repeat('cc', 32), '1.0', now() + interval '30 days',
            jsonb_build_object('rule_id', 'POL-PROBE', 'action', 'logged', 'decided_locally', true));
    RAISE EXCEPTION 'PROBE FAIL P4 a cross-tenant INSERT was accepted';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE 'PROBE FAIL%' THEN RAISE; END IF;
    RAISE NOTICE 'PASS P4 cross-tenant write refused (%)', SQLERRM;
  END;
END $$;

RESET ROLE;

-- -------------------------------------------------------------------------------------
-- The discriminating control: the same query, same rows, as a superuser.
-- -------------------------------------------------------------------------------------

\connect - postgres

DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM ingest.observation
   WHERE tenant_id IN ('33333333-3333-7333-8333-333333333333',
                       '44444444-4444-7444-8444-444444444444');
  IF n <> 8 THEN
    RAISE EXCEPTION 'PROBE FAIL C1 control expected 8 rows as superuser, found %', n;
  END IF;
  RAISE NOTICE 'PASS C1 control: the same SELECT as superuser returns 8 rows, not 5';
  RAISE NOTICE 'CONTROL the RLS assertions above can fail; they are not vacuous';
END $$;

-- -------------------------------------------------------------------------------------
-- Cleanup, so the probe is repeatable and leaves the cluster as it found it.
-- ingest.observation is append-only, so removal has to go through the documented
-- retention path: the sac.retention_delete session flag the trigger checks.
-- -------------------------------------------------------------------------------------

SET sac.retention_delete = on;
DELETE FROM ingest.observation WHERE tenant_id IN (:ta, :tb);
RESET sac.retention_delete;
DELETE FROM ops.device WHERE tenant_id IN (:ta, :tb);
DELETE FROM ops.tenant WHERE tenant_id IN (:ta, :tb);
DROP ROLE sac_probe;

DO $$ BEGIN RAISE NOTICE 'probe complete: all independent RLS assertions held'; END $$;
