-- One person is one row in every listing and count, however many refs their devices derived for
-- them. A device derives a person's ref from what it can resolve (a UPN, an Entra object id), and
-- ingest stores the canonical ref only once the directory has written the alias; rows recorded
-- before that kept the device's ref. The rollup moves those submissions, and each device's last
-- user, to the canonical ref and recomputes the buckets they fall in; the people list, and a team's
-- chosen people, read any ref as its canonical one.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

CREATE INDEX IF NOT EXISTS submission_by_user ON ingest.submission (tenant_id, user_ref);

CREATE OR REPLACE VIEW ops.v_team_member
WITH (security_invoker = true) AS
WITH RECURSIVE reach (tenant_id, team_id, part_id) AS (
  SELECT t.tenant_id, t.team_id, t.team_id FROM ops.team t
  UNION
  SELECT r.tenant_id, r.team_id, i.included_team_id
    FROM reach r
    JOIN ops.team_include i ON i.tenant_id = r.tenant_id AND i.team_id = r.part_id
   WHERE i.kind = 'team'
)
SELECT r.tenant_id, r.team_id, coalesce(ma.user_ref, m.user_ref) AS user_ref
  FROM reach r
  JOIN ops.team_member m ON m.tenant_id = r.tenant_id AND m.team_id = r.part_id
  LEFT JOIN ops.user_ref_alias ma ON ma.tenant_id = m.tenant_id AND ma.alias_ref = m.user_ref
UNION
SELECT r.tenant_id, r.team_id, u.user_ref
  FROM reach r
  JOIN ops.team_include i ON i.tenant_id = r.tenant_id AND i.team_id = r.part_id AND i.kind = 'group'
  JOIN ops.scim_group_member gm ON gm.tenant_id = i.tenant_id AND gm.group_id = i.group_id
  JOIN ops.scim_user u ON u.tenant_id = gm.tenant_id AND u.scim_id = gm.user_id
 WHERE u.active
UNION
SELECT r.tenant_id, r.team_id, ud.user_ref
  FROM reach r
  JOIN ops.team_include i ON i.tenant_id = r.tenant_id AND i.team_id = r.part_id
  JOIN ops.user_dim ud ON ud.tenant_id = i.tenant_id
 WHERE (i.kind = 'department' AND lower(ud.department) = lower(i.match_value))
    OR (i.kind = 'org_unit'
        AND (lower(ud.org_unit) = lower(i.match_value)
             OR right(lower(ud.org_unit), length(i.match_value) + 1) = ',' || lower(i.match_value)));

CREATE OR REPLACE VIEW mart.v_person
WITH (security_invoker = true) AS
SELECT coalesce(ud.tenant_id, a.tenant_id) AS tenant_id,
       coalesce(ud.user_ref, a.user_ref)   AS user_ref,
       ud.display_name                     AS directory_name,
       dn.subject_name,
       coalesce(ud.display_name, dn.subject_name, ud.user_ref, a.user_ref) AS name,
       ud.department,
       ud.org_unit,
       ud.status                           AS directory_status,
       a.last_active_day
  FROM ops.user_dim ud
  FULL JOIN (SELECT u.tenant_id, coalesce(ua.user_ref, u.user_ref) AS user_ref,
                    max(u.bucket_start) AS last_active_day
               FROM mart.agg_user_period u
               LEFT JOIN ops.user_ref_alias ua ON ua.tenant_id = u.tenant_id AND ua.alias_ref = u.user_ref
              WHERE u.bucket_size = 'day'
              GROUP BY 1, 2) a
    ON a.tenant_id = ud.tenant_id AND a.user_ref = ud.user_ref
  LEFT JOIN LATERAL (
    SELECT d.last_subject_name AS subject_name
      FROM ops.device d
      LEFT JOIN ops.user_ref_alias da ON da.tenant_id = d.tenant_id AND da.alias_ref = d.last_user_ref
     WHERE d.tenant_id = coalesce(ud.tenant_id, a.tenant_id)
       AND coalesce(da.user_ref, d.last_user_ref) = coalesce(ud.user_ref, a.user_ref)
       AND d.last_subject_name IS NOT NULL
     ORDER BY d.last_seen_at DESC NULLS LAST
     LIMIT 1) dn ON true;

CREATE OR REPLACE VIEW mart.v_team_member_period
WITH (security_invoker = true) AS
SELECT u.tenant_id, u.bucket_start, u.bucket_size, tm.team_id, tm.user_ref, u.submissions
  FROM mart.agg_user_period u
  LEFT JOIN ops.user_ref_alias ua ON ua.tenant_id = u.tenant_id AND ua.alias_ref = u.user_ref
  JOIN ops.v_team_member tm ON tm.tenant_id = u.tenant_id AND tm.user_ref = coalesce(ua.user_ref, u.user_ref);

GRANT SELECT ON ops.user_ref_alias TO sac_query, sac_ops;
GRANT UPDATE (last_user_ref) ON ops.device TO sac_ops;
