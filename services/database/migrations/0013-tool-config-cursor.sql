-- The Cursor configuration writer reports its health as collector tool_config_cursor. The row
-- already exists in a database built from the current schema.sql, which the migrator also applies
-- this file over.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('tool_config_cursor', 'capture_core', ARRAY[]::text[], 'Writes the agent''s hooks into Cursor''s enterprise hooks file so its prompts and MCP calls reach the hook relay. Not a collection path; it reads nothing.')
ON CONFLICT (collector_code) DO NOTHING;
