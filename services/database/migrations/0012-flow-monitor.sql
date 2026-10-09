-- The flow monitor reports its health as collector flow_monitor. The row already exists in a
-- database built from the current schema.sql, which the migrator also applies this file over.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('flow_monitor', 'capture_core', ARRAY['m0','m1','m2','m3'], 'The operating system''s DNS answers and TCP connect events: which process connected to a catalog inference domain. Connection metadata only; reads no payload.')
ON CONFLICT (collector_code) DO NOTHING;
