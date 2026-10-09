-- Local model capture: Ollama joins the endpoint tool settings with one switch, loopback, which has
-- the device move Ollama to another port and hold Ollama's own port with the loopback broker.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the column is added only when missing, and the tool key check
-- is dropped when present and created again under the name PostgreSQL gives the column's check.

ALTER TABLE ops.endpoint_tool_setting ADD COLUMN IF NOT EXISTS loopback boolean NOT NULL DEFAULT false;

ALTER TABLE ops.endpoint_tool_setting DROP CONSTRAINT IF EXISTS endpoint_tool_setting_tool_key_check;
ALTER TABLE ops.endpoint_tool_setting ADD CONSTRAINT endpoint_tool_setting_tool_key_check
  CHECK (tool_key IN ('claude_code','codex','copilot','cursor','ollama'));
