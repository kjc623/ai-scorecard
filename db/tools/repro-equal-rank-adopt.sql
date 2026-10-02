-- =====================================================================================
-- Regression reproduction: the equal-fidelity adopt path in ingest.record_event()
-- =====================================================================================
-- Reported by task-3 (ingestor). Reproduced here independently before any fix.
--
-- An exact observation that lands on an existing weak-only row must ADOPT that row
-- (docs/02-ingest-and-transport.md 4.5). The adopt path sets dedup_key with a coalesce, so
-- it takes the exact key whenever the observation is exact -- but it writes content_digest
-- only when v_fidelity < s.winning_fidelity. When the exact observation arrives on an
-- equal- or worse-ranked route, dedup_key becomes NOT NULL while content_digest stays NULL,
-- and CHECK submission_exact_key_implies_digest rejects the UPDATE.
--
-- Both events below use ext.web_request, whose ref.route_fidelity rank is 40. Equal rank is
-- the whole point: the strictly-better-rank path is the one T26 already covers, and it is
-- why this defect survived the original suite.
--
-- Runs in a transaction and rolls back, so it is safe against a populated database.
-- =====================================================================================

\set ON_ERROR_STOP on

BEGIN;

SET LOCAL app.tenant_id = '11111111-1111-7111-8111-111111111111';
SET LOCAL client_min_messages = notice;
SET ROLE sac_ingest;

-- E1: first sighting, route cannot read content (M0), rank 40. Produces a weak-only row.
DO $$
DECLARE r record;
BEGIN
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version', '1.0',
    'event_id', 'e0000000-0000-7000-8000-0000000000b1',
    'tenant_id', '11111111-1111-7111-8111-111111111111',
    'device_id', 'aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref', 'u_repro', 'tool_fingerprint', 'genai.web.chat.v1:tool-c',
    'direction', 'egress', 'kind', 'prompt',
    'occurred_at', '2026-10-02T16:00:00Z', 'monotonic_offset_ms', 1000,
    'source', 'ext.web_request', 'collection_mode', 'm0',
    'dedup_key', 'sha256:' || repeat('b1', 32), 'size_bytes', 100,
    'policy_decision', jsonb_build_object('rule_id', 'POL-R', 'action', 'logged', 'decided_locally', true)
  ), now());
  RAISE NOTICE 'E1 outcome=% (expected inserted: weak-only row)', r.event_outcome;
END $$;

-- E2: same submission, exact key, same route hence SAME fidelity rank, now with content.
DO $$
DECLARE r record;
BEGIN
  SELECT * INTO r FROM ingest.record_event(jsonb_build_object(
    'schema_version', '1.0',
    'event_id', 'e0000000-0000-7000-8000-0000000000b2',
    'tenant_id', '11111111-1111-7111-8111-111111111111',
    'device_id', 'aaaaaaaa-0000-7000-8000-000000000001',
    'user_ref', 'u_repro', 'tool_fingerprint', 'genai.web.chat.v1:tool-c',
    'direction', 'egress', 'kind', 'prompt',
    'occurred_at', '2026-10-02T16:01:00Z', 'monotonic_offset_ms', 61000,
    'source', 'ext.web_request', 'collection_mode', 'm1',
    'dedup_key', 'sha256:' || repeat('ee', 32), 'size_bytes', 100,
    'content_digest', 'sha256:' || repeat('dd', 32),
    'labels', jsonb_build_array(jsonb_build_object('class', 'payment_card', 'score', 0.9)),
    'classifier_version', 'test-2026.01', 'confidence', 'high',
    'policy_decision', jsonb_build_object('rule_id', 'POL-R', 'action', 'logged', 'decided_locally', true)
  ), now());
  RAISE NOTICE 'E2 outcome=% (expected merged: adopted the weak-only row)', r.event_outcome;
END $$;

ROLLBACK;
