-- The hook relay's collector row, so control-api accepts its health reports.
--
-- The migrator also applies this file over a database built from the current schema.sql, which
-- already holds the row, so the insert does nothing when it is present.

INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('hook_relay', 'capture_core', ARRAY['m0','m1','m2','m3'], 'Relay for the hooks AI tools run before sending a prompt: decides allow, warn or block on the device and records the prompt.')
ON CONFLICT (collector_code) DO NOTHING;
