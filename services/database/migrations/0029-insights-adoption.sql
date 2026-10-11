-- The Insights page's adoption figures.
--
-- mart.v_tool_first_seen is the day each tool first sent a prompt, for the applications new in a
-- window. The index lets a new tool's people be counted from its own rows.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

CREATE INDEX IF NOT EXISTS agg_tool_user_period_by_tool ON mart.agg_tool_user_period (tenant_id, tool_fingerprint, bucket_size, bucket_start)
  INCLUDE (user_ref);

CREATE OR REPLACE VIEW mart.v_tool_first_seen
WITH (security_invoker = true) AS
SELECT a.tenant_id,
       c.app_key                                                              AS tool_key,
       (array_agg(a.tool_fingerprint ORDER BY a.bucket_start, a.tool_fingerprint))[1] AS tool_fingerprint,
       array_agg(DISTINCT a.tool_fingerprint)                                 AS fingerprints,
       min(a.bucket_start)                                                    AS first_seen
  FROM mart.agg_tool_period a
  LEFT JOIN ref.tool_catalogue c
    ON c.tool_fingerprint = a.tool_fingerprint
 WHERE a.bucket_size = 'day' AND a.submissions > 0
 GROUP BY a.tenant_id, c.app_key, CASE WHEN c.app_key IS NULL THEN a.tool_fingerprint END;

GRANT SELECT ON mart.v_tool_first_seen TO sac_query;
