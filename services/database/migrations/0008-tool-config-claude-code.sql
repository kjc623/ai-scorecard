-- The Claude Code configuration writer reports its health as collector tool_config_claude_code. The
-- row already exists in a database built from the current schema.sql, which the migrator also
-- applies this file over.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('tool_config_claude_code', 'capture_core', ARRAY[]::text[], 'Writes Claude Code''s machine-wide managed settings so it exports its telemetry to the OTLP receiver. Not a collection path; it reads nothing.')
ON CONFLICT (collector_code) DO NOTHING;
