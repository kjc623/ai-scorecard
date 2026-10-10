-- One device, one row: mart.v_device_liveness sums each device's collector states beside it, so a
-- read of the devices list no longer fans out per collector. The existing columns keep their
-- places; the collector summary is appended.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

CREATE OR REPLACE VIEW mart.v_device_liveness
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
       c.collectors_reporting,
       c.collectors_healthy,
       c.collectors_degraded,
       c.collectors_absent,
       c.collectors_tampered,
       c.collectors,
       c.collector_states,
       c.spool_depth,
       c.spool_dropped_total,
       c.last_report_at
  FROM ops.device d
  LEFT JOIN LATERAL (
    SELECT count(*)::int                                            AS collectors_reporting,
           count(*) FILTER (WHERE cs.state = 'healthy')::int        AS collectors_healthy,
           count(*) FILTER (WHERE cs.state = 'degraded')::int       AS collectors_degraded,
           count(*) FILTER (WHERE cs.state = 'absent')::int         AS collectors_absent,
           count(*) FILTER (WHERE cs.state = 'tampered')::int       AS collectors_tampered,
           coalesce(array_agg(cs.collector ORDER BY cs.collector), ARRAY[]::text[]) AS collectors,
           coalesce(array_agg(DISTINCT cs.state), ARRAY[]::text[])  AS collector_states,
           sum(cs.spool_depth)::bigint                               AS spool_depth,
           sum(cs.spool_dropped_total)::bigint                       AS spool_dropped_total,
           max(cs.last_report_at)                                    AS last_report_at
      FROM ops.collector_state cs
     WHERE cs.tenant_id = d.tenant_id AND cs.device_id = d.device_id) c ON true;
