-- A team mixes people, groups, departments, units and other teams. What a team followed alone
-- (ops.team.source with group_id or match_value) becomes one row of ops.team_include, and the
-- people chosen for a team stay in ops.team_member whatever else it includes.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

-- What a team includes and follows as the directory changes: a provisioned group's members,
-- everyone in a department, everyone in an organisational unit and the units below it, or another
-- team's members. A group the identity provider stops provisioning, or a team deleted, drops out.
CREATE TABLE IF NOT EXISTS ops.team_include (
  tenant_id         uuid NOT NULL,
  team_id           uuid NOT NULL,
  include_id        uuid NOT NULL DEFAULT gen_random_uuid(),
  kind              text NOT NULL CHECK (kind IN ('group','department','org_unit','team')),
  group_id          uuid,
  -- The department name or the unit's distinguished name, compared case-insensitively.
  match_value       text CONSTRAINT team_include_match_value_bounded CHECK (length(match_value) <= 1024),
  included_team_id  uuid,
  added_by          text NOT NULL,
  added_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, team_id, include_id),
  FOREIGN KEY (tenant_id, team_id) REFERENCES ops.team(tenant_id, team_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, group_id) REFERENCES ops.scim_group(tenant_id, scim_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, included_team_id) REFERENCES ops.team(tenant_id, team_id) ON DELETE CASCADE,
  CONSTRAINT team_include_group CHECK ((kind = 'group') = (group_id IS NOT NULL)),
  CONSTRAINT team_include_match CHECK ((kind IN ('department','org_unit')) = (match_value IS NOT NULL)),
  CONSTRAINT team_include_team CHECK ((kind = 'team') = (included_team_id IS NOT NULL)),
  CONSTRAINT team_include_not_itself CHECK (included_team_id <> team_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS team_include_group_once ON ops.team_include (tenant_id, team_id, group_id);
CREATE UNIQUE INDEX IF NOT EXISTS team_include_team_once ON ops.team_include (tenant_id, team_id, included_team_id);
CREATE UNIQUE INDEX IF NOT EXISTS team_include_match_once ON ops.team_include (tenant_id, team_id, kind, lower(match_value));
CREATE INDEX IF NOT EXISTS team_include_by_group ON ops.team_include (tenant_id, group_id) WHERE group_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS team_include_by_team ON ops.team_include (tenant_id, included_team_id) WHERE included_team_id IS NOT NULL;

ALTER TABLE ops.team_include ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.team_include FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.team_include;
CREATE POLICY tenant_isolation ON ops.team_include
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
              WHERE table_schema = 'ops' AND table_name = 'team' AND column_name = 'source') THEN
    ALTER TABLE ops.team NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE ops.team_include NO FORCE ROW LEVEL SECURITY;
    INSERT INTO ops.team_include (tenant_id, team_id, kind, group_id, match_value, added_by, added_at)
    SELECT tenant_id, team_id, source, group_id, match_value, created_by, created_at
      FROM ops.team
     WHERE source <> 'console'
    ON CONFLICT DO NOTHING;
    ALTER TABLE ops.team FORCE ROW LEVEL SECURITY;
    ALTER TABLE ops.team_include FORCE ROW LEVEL SECURITY;
  END IF;
END $$;

-- Every team's members as of now. A team reaches the teams it includes, and theirs in turn; UNION
-- stops the recursion at a team already reached, so a cycle cannot loop. A person may be in
-- several teams.
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
SELECT r.tenant_id, r.team_id, m.user_ref
  FROM reach r
  JOIN ops.team_member m ON m.tenant_id = r.tenant_id AND m.team_id = r.part_id
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

ALTER TABLE ops.team DROP CONSTRAINT IF EXISTS team_group_source;
ALTER TABLE ops.team DROP CONSTRAINT IF EXISTS team_match_source;
ALTER TABLE ops.team DROP COLUMN IF EXISTS group_id;
ALTER TABLE ops.team DROP COLUMN IF EXISTS match_value;
ALTER TABLE ops.team DROP COLUMN IF EXISTS source;

GRANT SELECT, INSERT, UPDATE, DELETE ON ops.team_include TO sac_control;
GRANT SELECT ON ops.team_include TO sac_query, sac_ops;
