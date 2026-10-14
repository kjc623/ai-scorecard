-- A device's diagnostics, asked for by an admin in the console and answered by the device with a zip
-- of its health snapshot and its own logs. The expire job sweeps a request past expires_at.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the table and its indexes are created only when missing, and
-- the policy is dropped when present and created again.

CREATE TABLE IF NOT EXISTS ops.device_diagnostics (
  tenant_id     uuid NOT NULL,
  request_id    uuid NOT NULL,
  device_id     uuid NOT NULL,
  requested_by  text NOT NULL,
  requested_at  timestamptz NOT NULL,
  received_at   timestamptz,
  agent_version text,
  size_bytes    bigint CHECK (size_bytes >= 0),
  sha256        text CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  bundle        bytea,
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, request_id),
  FOREIGN KEY (tenant_id, device_id) REFERENCES ops.device(tenant_id, device_id),
  CONSTRAINT device_diagnostics_received CHECK (
    (received_at IS NULL AND bundle IS NULL AND size_bytes IS NULL AND sha256 IS NULL)
    OR (received_at IS NOT NULL AND bundle IS NOT NULL AND size_bytes IS NOT NULL AND sha256 IS NOT NULL)),
  CONSTRAINT device_diagnostics_expiry_after_request CHECK (expires_at > requested_at)
);

CREATE INDEX IF NOT EXISTS device_diagnostics_by_device ON ops.device_diagnostics (tenant_id, device_id, requested_at);
CREATE INDEX IF NOT EXISTS device_diagnostics_expiry ON ops.device_diagnostics (tenant_id, expires_at);

ALTER TABLE ops.device_diagnostics ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.device_diagnostics FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.device_diagnostics;
CREATE POLICY tenant_isolation ON ops.device_diagnostics
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT, INSERT, UPDATE ON ops.device_diagnostics TO sac_control;
GRANT SELECT (tenant_id, request_id, expires_at), DELETE ON ops.device_diagnostics TO sac_ops;
