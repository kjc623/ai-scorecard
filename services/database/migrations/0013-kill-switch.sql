-- The tenant's kill switches: one row per tripped interception route, set on the Settings page and
-- delivered in the policy bundle's kill_switches.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the table is created only when missing, and the policy is
-- dropped when present and created again.

CREATE TABLE IF NOT EXISTS ops.kill_switch (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  route         text NOT NULL CHECK (route IN ('proxy.tls','proxy.loopback')),
  reason_code   text NOT NULL CHECK (reason_code ~ '^[a-z][a-z0-9_.-]{0,63}$'),
  effective_at  timestamptz NOT NULL DEFAULT now(),
  set_by        text NOT NULL,
  PRIMARY KEY (tenant_id, route)
);

ALTER TABLE ops.kill_switch ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.kill_switch FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.kill_switch;
CREATE POLICY tenant_isolation ON ops.kill_switch
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT, INSERT, UPDATE, DELETE ON ops.kill_switch TO sac_control;
