-- The directory reaches the product two ways: SCIM, pushed by the customer's identity provider,
-- and a pull from Microsoft Graph (ops.directory_sync). People gain their organisational unit
-- (ops.user_dim.org_unit). Teams are created in the console or follow a directory group, a
-- department or an organisational unit (ops.team, ops.team_member, ops.v_team_member), and usage
-- is rolled up per team (mart.agg_team_period). mart.v_person lists the people an analyst can open.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

ALTER TABLE ops.user_dim ADD COLUMN IF NOT EXISTS org_unit text;

CREATE TABLE IF NOT EXISTS ops.directory_sync (
  tenant_id          uuid PRIMARY KEY REFERENCES ops.tenant(tenant_id),
  enabled_by         text NOT NULL,
  enabled_at         timestamptz NOT NULL DEFAULT now(),
  last_started_at    timestamptz,
  last_completed_at  timestamptz,
  last_status        text CHECK (last_status IN ('ok','failed')),
  last_error         text CONSTRAINT directory_sync_error_is_code CHECK (last_error ~ '^[a-z_]{1,64}$'),
  users_synced       integer CHECK (users_synced >= 0),
  groups_synced      integer CHECK (groups_synced >= 0),
  CONSTRAINT directory_sync_failure_has_code CHECK ((last_status = 'failed') = (last_error IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS ops.team (
  tenant_id    uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  team_id      uuid NOT NULL DEFAULT gen_random_uuid(),
  name         text NOT NULL CONSTRAINT team_name_present CHECK (btrim(name) <> '' AND length(name) <= 120),
  source       text NOT NULL CHECK (source IN ('console','group','department','org_unit')),
  group_id     uuid,
  -- The department name or the unit's distinguished name, compared case-insensitively.
  match_value  text CONSTRAINT team_match_value_bounded CHECK (length(match_value) <= 1024),
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, team_id),
  -- A group the identity provider stops provisioning takes its team with it.
  FOREIGN KEY (tenant_id, group_id) REFERENCES ops.scim_group(tenant_id, scim_id) ON DELETE CASCADE,
  CONSTRAINT team_group_source CHECK ((source = 'group') = (group_id IS NOT NULL)),
  CONSTRAINT team_match_source CHECK ((source IN ('department','org_unit')) = (match_value IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS team_name_unique ON ops.team (tenant_id, lower(name));

CREATE TABLE IF NOT EXISTS ops.team_member (
  tenant_id  uuid NOT NULL,
  team_id    uuid NOT NULL,
  user_ref   text NOT NULL CONSTRAINT team_member_ref_is_derived CHECK (user_ref ~ '^u_[0-9a-f]{32}$'),
  added_by   text NOT NULL,
  added_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, team_id, user_ref),
  FOREIGN KEY (tenant_id, team_id) REFERENCES ops.team(tenant_id, team_id) ON DELETE CASCADE
);

CREATE OR REPLACE VIEW ops.v_team_member
WITH (security_invoker = true) AS
SELECT t.tenant_id, t.team_id, m.user_ref
  FROM ops.team t
  JOIN ops.team_member m ON m.tenant_id = t.tenant_id AND m.team_id = t.team_id
 WHERE t.source = 'console'
UNION
SELECT t.tenant_id, t.team_id, u.user_ref
  FROM ops.team t
  JOIN ops.scim_group_member gm ON gm.tenant_id = t.tenant_id AND gm.group_id = t.group_id
  JOIN ops.scim_user u ON u.tenant_id = gm.tenant_id AND u.scim_id = gm.user_id
 WHERE t.source = 'group' AND u.active
UNION
SELECT t.tenant_id, t.team_id, ud.user_ref
  FROM ops.team t
  JOIN ops.user_dim ud ON ud.tenant_id = t.tenant_id
 WHERE (t.source = 'department' AND lower(ud.department) = lower(t.match_value))
    OR (t.source = 'org_unit'
        AND (lower(ud.org_unit) = lower(t.match_value)
             OR right(lower(ud.org_unit), length(t.match_value) + 1) = ',' || lower(t.match_value)));

CREATE TABLE IF NOT EXISTS mart.agg_team_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  team_id          uuid NOT NULL,
  tool_fingerprint text NOT NULL,
  submissions      bigint NOT NULL CHECK (submissions >= 0),
  users            bigint NOT NULL CHECK (users >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, team_id, tool_fingerprint)
);

CREATE INDEX IF NOT EXISTS agg_team_period_by_team ON mart.agg_team_period (tenant_id, team_id, bucket_start DESC);

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
  FULL JOIN (SELECT u.tenant_id, u.user_ref, max(u.bucket_start) AS last_active_day
               FROM mart.agg_user_period u
              WHERE u.bucket_size = 'day'
              GROUP BY u.tenant_id, u.user_ref) a
    ON a.tenant_id = ud.tenant_id AND a.user_ref = ud.user_ref
  LEFT JOIN LATERAL (
    SELECT d.last_subject_name AS subject_name
      FROM ops.device d
     WHERE d.tenant_id = coalesce(ud.tenant_id, a.tenant_id)
       AND d.last_user_ref = coalesce(ud.user_ref, a.user_ref)
       AND d.last_subject_name IS NOT NULL
     ORDER BY d.last_seen_at DESC NULLS LAST
     LIMIT 1) dn ON true;

ALTER TABLE ops.directory_sync ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.directory_sync FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.directory_sync;
CREATE POLICY tenant_isolation ON ops.directory_sync
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

ALTER TABLE ops.team ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.team FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.team;
CREATE POLICY tenant_isolation ON ops.team
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

ALTER TABLE ops.team_member ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.team_member FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.team_member;
CREATE POLICY tenant_isolation ON ops.team_member
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

ALTER TABLE mart.agg_team_period ENABLE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_team_period FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mart.agg_team_period;
CREATE POLICY tenant_isolation ON mart.agg_team_period
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT, INSERT, UPDATE, DELETE ON ops.directory_sync, ops.team, ops.team_member TO sac_control;
GRANT SELECT ON ops.v_team_member TO sac_control;
-- ops.tenant_ids() belongs to sac_resolver, so the grant is made as its owner.
GRANT sac_resolver TO CURRENT_USER;
SET ROLE sac_resolver;
GRANT EXECUTE ON FUNCTION ops.tenant_ids() TO sac_control;
RESET ROLE;
REVOKE sac_resolver FROM CURRENT_USER;

GRANT SELECT ON mart.agg_team_period, mart.v_person TO sac_query;
GRANT SELECT ON ops.team, ops.team_member, ops.v_team_member, ops.scim_group,
      ops.scim_group_member TO sac_query;
GRANT SELECT (tenant_id, scim_id, user_ref, active) ON ops.scim_user TO sac_query;

GRANT SELECT, INSERT, UPDATE, DELETE ON mart.agg_team_period TO sac_ops;
GRANT SELECT ON ops.team, ops.team_member, ops.v_team_member, ops.scim_group_member TO sac_ops;
GRANT SELECT (tenant_id, scim_id, user_ref, active) ON ops.scim_user TO sac_ops;
