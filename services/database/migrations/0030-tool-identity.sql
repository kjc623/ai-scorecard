-- A tool is counted once however many fingerprints it was seen under. ops.tool_app names the app a
-- fingerprint belongs to, a recorded host by the inference domain that covers it now; ops.tool_id
-- is the tool a fingerprint counts as; mart.v_tool_host resolves a tenant's hosts once each; and
-- ops.tool_fingerprints widens a filter on a tool to every fingerprint of it. The usage and
-- first-seen views count by tool, a fingerprint is named after its tool, and a person's tools_used
-- is recounted by tool where the person used one tool under several fingerprints.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema. It runs as the tables' owner, which forced row-level security
-- binds like any other role, so the recount lifts FORCE for this transaction and restores it.

CREATE INDEX IF NOT EXISTS agg_tool_period_hosts ON mart.agg_tool_period (tenant_id, tool_fingerprint)
  WHERE bucket_size = 'day' AND tool_fingerprint LIKE 'host:%';

-- The app a tool fingerprint belongs to, NULL for a tool outside the catalogue. A catalogued
-- fingerprint names its app. `host:<host>` is a destination no app claimed when it was recorded; it
-- belongs to the app the browser extension names that host after now: of the apps known by their
-- inference domains alone, the one listing the host itself, else the longest domain covering it,
-- the first app_key on a tie. So a host recorded before an app's domain covered it is that app.
CREATE OR REPLACE FUNCTION ops.tool_app(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce(
    (SELECT c.app_key
       FROM ref.tool_catalogue c
      WHERE c.tool_fingerprint = p_tool_fingerprint),
    (SELECT d.app_key
       FROM (SELECT s.app_key, regexp_replace(lower(btrim(s.value)), '^\*', '') AS pattern
               FROM ref.app_signal s
              WHERE s.kind = 'inference_domain'
                AND NOT EXISTS (SELECT 1 FROM ref.app_signal o
                                 WHERE o.app_key = s.app_key AND o.kind <> 'inference_domain')) d
      WHERE starts_with(p_tool_fingerprint, 'host:') AND length(p_tool_fingerprint) > 5
        AND (d.pattern = substr(p_tool_fingerprint, 6)
             OR (starts_with(d.pattern, '.')
                 AND (substr(p_tool_fingerprint, 6) = substr(d.pattern, 2)
                      OR right(substr(p_tool_fingerprint, 6), length(d.pattern)) = d.pattern)))
      ORDER BY CASE WHEN starts_with(d.pattern, '.') THEN length(d.pattern) ELSE 2147483647 END DESC,
               d.app_key COLLATE "C"
      LIMIT 1))
$$;

-- The tool a fingerprint counts as: its app's own fingerprint, `app:<app_key>`, when an app claims
-- it, else the fingerprint itself. Every list of tools groups by it, so a tool seen under several
-- fingerprints is one row.
CREATE OR REPLACE FUNCTION ops.tool_id(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce('app:' || ops.tool_app(p_tool_fingerprint), p_tool_fingerprint)
$$;

-- Each host a tenant recorded, with the app that claims it now, if any. A read joins it beside
-- ref.tool_catalogue to count rows as ops.tool_id does, resolving a host once rather than once per
-- row. Every aggregate row's tool has a day row here: a pass writes them together.
CREATE OR REPLACE VIEW mart.v_tool_host
WITH (security_invoker = true) AS
SELECT a.tenant_id,
       a.tool_fingerprint,
       ops.tool_app(a.tool_fingerprint) AS app_key
  FROM mart.agg_tool_period a
 WHERE a.bucket_size = 'day' AND a.tool_fingerprint LIKE 'host:%'
 GROUP BY a.tenant_id, a.tool_fingerprint;

-- Usage with present-tense sanction state: the decision on the tool the fingerprint belongs to.
-- LEFT JOINs, so a fingerprint outside the catalogue, or a tool with no decision, still appears with
-- a NULL state, which the read layer renders as `unknown`. tool_id is the tool the row counts as.
CREATE OR REPLACE VIEW mart.v_tool_usage
WITH (security_invoker = true) AS
SELECT a.tenant_id,
       a.bucket_start,
       a.bucket_size,
       a.tool_fingerprint,
       coalesce(c.display_name, a.tool_fingerprint) AS display_name,
       s.sanctioned_state,
       a.submissions,
       a.users,
       a.bytes_total,
       a.blocked,
       a.warned,
       a.logged,
       a.sensitive,
       a.sensitive_blocked,
       a.sensitive_warned,
       a.sensitive_logged,
       a.attachments,
       a.content_prompts,
       coalesce('app:' || coalesce(c.app_key, h.app_key), a.tool_fingerprint) AS tool_id
  FROM mart.agg_tool_period a
  LEFT JOIN ref.tool_catalogue c
    ON c.tool_fingerprint = a.tool_fingerprint
  LEFT JOIN mart.v_tool_host h
    ON h.tenant_id = a.tenant_id
   AND h.tool_fingerprint = a.tool_fingerprint
  LEFT JOIN ops.tool_sanction s
    ON s.tenant_id = a.tenant_id
   AND s.tool_key = coalesce(c.app_key, h.app_key);

-- The day each tool first sent a prompt, a tool being what every list counts (ops.tool_id): its
-- tool_fingerprint is its app's own, or the one fingerprint of a tool no app claims. It counts
-- from the first of its fingerprints to appear and carries every one it has been seen under.
CREATE OR REPLACE VIEW mart.v_tool_first_seen
WITH (security_invoker = true) AS
SELECT a.tenant_id,
       coalesce(c.app_key, h.app_key)                                          AS tool_key,
       coalesce('app:' || coalesce(c.app_key, h.app_key), a.tool_fingerprint) AS tool_fingerprint,
       array_agg(DISTINCT a.tool_fingerprint)                                  AS fingerprints,
       min(a.bucket_start)                                                     AS first_seen
  FROM mart.agg_tool_period a
  LEFT JOIN ref.tool_catalogue c
    ON c.tool_fingerprint = a.tool_fingerprint
  LEFT JOIN mart.v_tool_host h
    ON h.tenant_id = a.tenant_id
   AND h.tool_fingerprint = a.tool_fingerprint
 WHERE a.bucket_size = 'day' AND a.submissions > 0
 GROUP BY 1, 2, 3;

-- Every fingerprint of the given tools, so a filter on a tool matches each of them through the
-- fingerprint's own index. A value stands for the tool it counts as (ops.tool_id); a tool's
-- fingerprints are that id, its app's catalogue rows and the hosts this tenant recorded under it.
CREATE OR REPLACE FUNCTION ops.tool_fingerprints(p_tools text[])
RETURNS text[]
LANGUAGE sql STABLE AS $$
  WITH tool AS (
    SELECT DISTINCT ops.tool_id(v) AS tool_id FROM unnest(p_tools) AS v WHERE v IS NOT NULL
  )
  SELECT coalesce(array_agg(DISTINCT f.tool_fingerprint), '{}')
    FROM (SELECT t.tool_id AS tool_fingerprint FROM tool t
          UNION ALL
          SELECT c.tool_fingerprint FROM ref.tool_catalogue c JOIN tool t ON t.tool_id = 'app:' || c.app_key
          UNION ALL
          SELECT h.tool_fingerprint FROM mart.v_tool_host h JOIN tool t ON t.tool_id = 'app:' || h.app_key) f
$$;

-- A fingerprint's display name: its tool's, from the shared catalogue; for `host:<host>`, a
-- destination no app claims, the host; else "Unrecognised tool". Any other raw fingerprint is never
-- shown as a name, because it would read like a known tool with a bad label; it travels separately
-- on every row.
CREATE OR REPLACE FUNCTION ops.tool_display_name(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce(
    (SELECT c.display_name
       FROM ref.tool_catalogue c
      WHERE c.tool_fingerprint = ops.tool_id(p_tool_fingerprint)),
    nullif(substring(p_tool_fingerprint FROM '^host:(.*)$'), ''),
    'Unrecognised tool')
$$;

GRANT SELECT ON mart.v_tool_host TO sac_query;
GRANT SELECT ON ref.tool_catalogue, ref.app_signal TO sac_vault, sac_ops;
GRANT EXECUTE ON FUNCTION ops.tool_app(text), ops.tool_id(text) TO sac_vault, sac_query, sac_ops;
GRANT EXECUTE ON FUNCTION ops.tool_fingerprints(text[]) TO sac_query;

-- A person's tools in a bucket, recounted by tool from their usage per fingerprint, where the stored
-- count is that bucket's count of fingerprints.
ALTER TABLE mart.agg_user_period NO FORCE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_tool_user_period NO FORCE ROW LEVEL SECURITY;

WITH tool AS MATERIALIZED (
  SELECT a.tool_fingerprint, ops.tool_id(a.tool_fingerprint) AS tool_id
    FROM mart.agg_tool_user_period a
   GROUP BY a.tool_fingerprint
),
counted AS (
  SELECT a.tenant_id, a.bucket_start, a.bucket_size, a.user_ref,
         count(*) AS fingerprints, count(DISTINCT t.tool_id) AS tools
    FROM mart.agg_tool_user_period a
    JOIN tool t ON t.tool_fingerprint = a.tool_fingerprint
   GROUP BY a.tenant_id, a.bucket_start, a.bucket_size, a.user_ref
)
UPDATE mart.agg_user_period u
   SET tools_used = c.tools
  FROM counted c
 WHERE u.tenant_id = c.tenant_id AND u.bucket_start = c.bucket_start
   AND u.bucket_size = c.bucket_size AND u.user_ref = c.user_ref
   AND u.tools_used = c.fingerprints AND c.tools < c.fingerprints;

ALTER TABLE mart.agg_tool_user_period FORCE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_user_period FORCE ROW LEVEL SECURITY;
