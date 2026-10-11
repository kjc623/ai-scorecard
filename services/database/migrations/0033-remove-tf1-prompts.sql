-- Everything recorded under a tf1 fingerprint is removed. A tf1 fingerprint hashes a request's shape
-- together with its destination, so neither the site nor the tool can be recovered from it, and no
-- extension sends one any more. In every tenant this removes the tf1 submissions and observations,
-- their search text, stored content, findings and finding reviews, and the tf1 rows of every
-- per-tool aggregate; each person's and each rule's totals drop by the prompts removed, at both
-- bucket sizes. Each tenant that loses anything gets an erasure receipt with the counts. Content
-- grants and the audit trail are kept, as subject erasure keeps them.
--
-- A submission keeps the fingerprint of the observation that created it, and observations from two
-- routes merge on the prompt's content digest. A prompt seen both under tf1 and under another
-- fingerprint is therefore not hash-only, and the migration stops, removing nothing, if one exists.
--
-- The migrator also applies this file over a database built from the current schema.sql, where
-- there is nothing to remove. It runs as the tables' owner, which forced row-level security binds
-- like any other role, so it lifts FORCE for this transaction and restores it. Observations are
-- append-only; the deletion declares itself on the retention path, as the erasure job does.

SET LOCAL sac.retention_delete = on;

DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'ingest.observation', 'ingest.submission', 'ingest.search_text', 'ops.content',
    'mart.finding', 'ops.finding_review', 'ops.erasure_receipt',
    'mart.agg_user_period', 'mart.agg_rule_period',
    'mart.agg_tool_period', 'mart.agg_tool_user_period', 'mart.agg_class_period',
    'mart.agg_org_period', 'mart.agg_team_period', 'mart.agg_discovery_period'
  ] LOOP
    EXECUTE format('ALTER TABLE %s NO FORCE ROW LEVEL SECURITY', t);
  END LOOP;

  IF EXISTS (SELECT 1
               FROM ingest.submission s
               JOIN ingest.observation o ON o.tenant_id = s.tenant_id AND o.dedup_key = s.dedup_key
              WHERE s.tool_fingerprint LIKE 'tf1:%'
                AND o.kind = 'prompt' AND o.content_digest IS NOT NULL
                AND o.tool_fingerprint NOT LIKE 'tf1:%')
     OR EXISTS (SELECT 1
                  FROM ingest.observation o
                  JOIN ingest.submission s ON s.tenant_id = o.tenant_id AND s.dedup_key = o.dedup_key
                 WHERE o.tool_fingerprint LIKE 'tf1:%'
                   AND o.kind = 'prompt' AND o.content_digest IS NOT NULL
                   AND s.tool_fingerprint NOT LIKE 'tf1:%') THEN
    RAISE EXCEPTION 'a prompt was recorded under a tf1 fingerprint and under another fingerprint; nothing was removed';
  END IF;
END $$;

CREATE TEMP TABLE tf1_submission ON COMMIT DROP AS
SELECT s.tenant_id, s.submission_id, s.kind, s.user_ref, s.received_at, s.size_bytes,
       s.policy_action, s.policy_rule_id, s.tool_fingerprint
  FROM ingest.submission s
 WHERE s.tool_fingerprint LIKE 'tf1:%';

CREATE TEMP TABLE tf1_event ON COMMIT DROP AS
SELECT o.tenant_id, o.event_id
  FROM ingest.observation o
 WHERE o.tool_fingerprint LIKE 'tf1:%';

-- The tf1 prompts once per bucket size, in the bucket the rollup counts each in.
CREATE TEMP TABLE tf1_prompt ON COMMIT DROP AS
SELECT s.tenant_id, b.bucket_size, date_trunc(b.bucket_size, s.received_at) AS bucket_start,
       s.user_ref, s.tool_fingerprint, s.size_bytes, s.policy_action, s.policy_rule_id
  FROM tf1_submission s
 CROSS JOIN (VALUES ('hour'), ('day')) AS b(bucket_size)
 WHERE s.kind = 'prompt';

CREATE TEMP TABLE tf1_removed (tenant_id uuid NOT NULL, tbl text NOT NULL, n bigint NOT NULL) ON COMMIT DROP;

-- A person's totals in a bucket lose the tf1 prompts. Each tf1 fingerprint is a tool of its own, so
-- tools_used loses one per tf1 fingerprint the person used in the bucket. A bucket left with no
-- prompts has no row, as the rollup writes none.
WITH removed AS (
  SELECT p.tenant_id, p.bucket_size, p.bucket_start, p.user_ref,
         count(*) AS submissions,
         coalesce(sum(p.size_bytes), 0) AS bytes_total,
         count(DISTINCT p.tool_fingerprint) AS tools_used,
         count(*) FILTER (WHERE p.policy_action = 'blocked') AS block_events
    FROM tf1_prompt p
   GROUP BY 1, 2, 3, 4
)
UPDATE mart.agg_user_period u
   SET submissions  = u.submissions  - least(u.submissions,  r.submissions),
       bytes_total  = u.bytes_total  - least(u.bytes_total,  r.bytes_total),
       tools_used   = u.tools_used   - least(u.tools_used,   r.tools_used),
       block_events = u.block_events - least(u.block_events, r.block_events)
  FROM removed r
 WHERE u.tenant_id = r.tenant_id AND u.bucket_size = r.bucket_size
   AND u.bucket_start = r.bucket_start AND u.user_ref = r.user_ref;

WITH d AS (
  DELETE FROM mart.agg_user_period u
   USING (SELECT DISTINCT tenant_id, bucket_size, bucket_start, user_ref FROM tf1_prompt) r
   WHERE u.tenant_id = r.tenant_id AND u.bucket_size = r.bucket_size
     AND u.bucket_start = r.bucket_start AND u.user_ref = r.user_ref
     AND u.submissions = 0
  RETURNING u.tenant_id
)
INSERT INTO tf1_removed SELECT tenant_id, 'mart.agg_user_period', count(*) FROM d GROUP BY tenant_id;

-- A rule's totals in a bucket lose the tf1 prompts it decided.
WITH removed AS (
  SELECT p.tenant_id, p.bucket_size, p.bucket_start, p.policy_rule_id AS rule_id,
         count(*) AS matches,
         count(*) FILTER (WHERE p.policy_action = 'blocked') AS blocked,
         count(*) FILTER (WHERE p.policy_action = 'warned')  AS warned,
         count(*) FILTER (WHERE p.policy_action = 'logged')  AS logged
    FROM tf1_prompt p
   WHERE p.policy_rule_id IS NOT NULL AND p.policy_rule_id <> 'policy.default'
   GROUP BY 1, 2, 3, 4
)
UPDATE mart.agg_rule_period a
   SET matches = a.matches - least(a.matches, r.matches),
       blocked = a.blocked - least(a.blocked, r.blocked),
       warned  = a.warned  - least(a.warned,  r.warned),
       logged  = a.logged  - least(a.logged,  r.logged)
  FROM removed r
 WHERE a.tenant_id = r.tenant_id AND a.bucket_size = r.bucket_size
   AND a.bucket_start = r.bucket_start AND a.rule_id = r.rule_id;

WITH d AS (
  DELETE FROM mart.agg_rule_period a
   USING (SELECT DISTINCT tenant_id, bucket_size, bucket_start, policy_rule_id FROM tf1_prompt) r
   WHERE a.tenant_id = r.tenant_id AND a.bucket_size = r.bucket_size
     AND a.bucket_start = r.bucket_start AND a.rule_id = r.policy_rule_id
     AND a.matches = 0
  RETURNING a.tenant_id
)
INSERT INTO tf1_removed SELECT tenant_id, 'mart.agg_rule_period', count(*) FROM d GROUP BY tenant_id;

DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'mart.agg_tool_period', 'mart.agg_tool_user_period', 'mart.agg_class_period',
    'mart.agg_org_period', 'mart.agg_team_period', 'mart.agg_discovery_period'
  ] LOOP
    EXECUTE format(
      'WITH d AS (DELETE FROM %s WHERE tool_fingerprint LIKE ''tf1:%%'' RETURNING tenant_id)
       INSERT INTO tf1_removed SELECT tenant_id, %L, count(*) FROM d GROUP BY tenant_id', t, t);
  END LOOP;
END $$;

-- Findings and their reviews go with their submission; counted first so the receipt states them.
INSERT INTO tf1_removed
SELECT f.tenant_id, 'mart.finding', count(*)
  FROM mart.finding f JOIN tf1_submission s USING (tenant_id, submission_id)
 GROUP BY f.tenant_id;

INSERT INTO tf1_removed
SELECT r.tenant_id, 'ops.finding_review', count(*)
  FROM ops.finding_review r JOIN tf1_submission s USING (tenant_id, submission_id)
 GROUP BY r.tenant_id;

WITH d AS (
  DELETE FROM ingest.search_text x
   USING tf1_submission s
   WHERE x.tenant_id = s.tenant_id AND x.submission_id = s.submission_id
  RETURNING x.tenant_id
)
INSERT INTO tf1_removed SELECT tenant_id, 'ingest.search_text', count(*) FROM d GROUP BY tenant_id;

-- Stored content by submission or by event, so an object uploaded before its submission was known
-- goes too.
WITH d AS (
  DELETE FROM ops.content c
   WHERE EXISTS (SELECT 1 FROM tf1_submission s
                  WHERE s.tenant_id = c.tenant_id AND s.submission_id = c.submission_id)
      OR EXISTS (SELECT 1 FROM tf1_event e
                  WHERE e.tenant_id = c.tenant_id AND e.event_id = c.event_id)
  RETURNING c.tenant_id
)
INSERT INTO tf1_removed SELECT tenant_id, 'ops.content', count(*) FROM d GROUP BY tenant_id;

WITH d AS (
  DELETE FROM ingest.observation o
   USING tf1_event e
   WHERE o.tenant_id = e.tenant_id AND o.event_id = e.event_id
  RETURNING o.tenant_id
)
INSERT INTO tf1_removed SELECT tenant_id, 'ingest.observation', count(*) FROM d GROUP BY tenant_id;

WITH d AS (
  DELETE FROM ingest.submission x
   USING tf1_submission s
   WHERE x.tenant_id = s.tenant_id AND x.submission_id = s.submission_id
  RETURNING x.tenant_id
)
INSERT INTO tf1_removed SELECT tenant_id, 'ingest.submission', count(*) FROM d GROUP BY tenant_id;

INSERT INTO ops.erasure_receipt
  (tenant_id, receipt_id, scope_kind, requested_by, requested_at, completed_at, mechanisms, removed_counts)
SELECT tenant_id, gen_random_uuid(), 'tenant', 'migrate:0033-remove-tf1-prompts', now(), clock_timestamp(),
       '{database}', jsonb_object_agg(tbl, n)
  FROM tf1_removed
 GROUP BY tenant_id;

DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'ingest.observation', 'ingest.submission', 'ingest.search_text', 'ops.content',
    'mart.finding', 'ops.finding_review', 'ops.erasure_receipt',
    'mart.agg_user_period', 'mart.agg_rule_period',
    'mart.agg_tool_period', 'mart.agg_tool_user_period', 'mart.agg_class_period',
    'mart.agg_org_period', 'mart.agg_team_period', 'mart.agg_discovery_period'
  ] LOOP
    EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', t);
  END LOOP;
END $$;
