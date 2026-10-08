-- The OTLP receiver's collector row, so control-api accepts its health reports.
--
-- The migrator also applies this file over a database built from the current schema.sql, which
-- already holds the row, so the insert does nothing when it is present.

INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('otel_receiver', 'capture_core', ARRAY['m0','m1','m2','m3'], 'Loopback OTLP receiver for the telemetry AI tools export about themselves, authenticated by a per-device token.')
ON CONFLICT (collector_code) DO NOTHING;
