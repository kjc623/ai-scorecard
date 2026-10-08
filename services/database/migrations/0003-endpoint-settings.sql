-- The endpoint collector settings: the tenant's switches for each endpoint collector and, per tool,
-- its native collectors. A tenant without a row is served the defaults by the policy read.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the tables are created only when missing, and the policy is
-- dropped when present and created again.

CREATE TABLE IF NOT EXISTS ops.endpoint_setting (
  tenant_id          uuid PRIMARY KEY REFERENCES ops.tenant(tenant_id),
  inventory          boolean NOT NULL DEFAULT true,
  processes          boolean NOT NULL DEFAULT true,
  flows              boolean NOT NULL DEFAULT true,
  otel               boolean NOT NULL DEFAULT true,
  hooks              boolean NOT NULL DEFAULT true,
  hooks_managed_only boolean NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS ops.endpoint_tool_setting (
  tenant_id  uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  tool_key   text NOT NULL CHECK (tool_key IN ('claude_code','codex','copilot','cursor')),
  otel       boolean NOT NULL,
  hooks      boolean NOT NULL,
  PRIMARY KEY (tenant_id, tool_key)
);

ALTER TABLE ops.endpoint_setting ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.endpoint_setting FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.endpoint_setting;
CREATE POLICY tenant_isolation ON ops.endpoint_setting
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

ALTER TABLE ops.endpoint_tool_setting ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.endpoint_tool_setting FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.endpoint_tool_setting;
CREATE POLICY tenant_isolation ON ops.endpoint_tool_setting
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT, INSERT, UPDATE ON ops.endpoint_setting, ops.endpoint_tool_setting TO sac_control;
