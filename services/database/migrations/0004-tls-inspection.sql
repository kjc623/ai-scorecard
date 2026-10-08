-- TLS inspection becomes a tenant setting, off by default: existing tenants get the default and
-- nothing turns it on. The desktop-app PAC reports its own health, so it gets a collector.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the column is added only when missing, and the collector row
-- only when absent.

ALTER TABLE ops.tenant ADD COLUMN IF NOT EXISTS tls_inspection boolean NOT NULL DEFAULT false;

INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('desktop_proxy', 'capture_core', ARRAY['m0','m1','m2','m3'], 'Per-user proxy auto-config (PAC) that routes the AI traffic of Windows desktop apps through the local TLS proxy.')
ON CONFLICT (collector_code) DO NOTHING;
