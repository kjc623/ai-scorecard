-- The Copilot configuration writer reports its health as collector tool_config_copilot. The row
-- already exists in a database built from the current schema.sql, which the migrator also applies
-- this file over.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('tool_config_copilot', 'capture_core', ARRAY[]::text[], 'Writes VS Code''s machine policies for the Copilot extension and the Copilot CLI''s machine environment variables so both export their telemetry to the OTLP receiver. Not a collection path; it reads nothing.')
ON CONFLICT (collector_code) DO NOTHING;
