-- Claude's web app is its own app, named by the domain claude.ai and every host under it, as
-- ChatGPT's is. A browser prompt to claude.ai is Claude (web), and prompts already stored under
-- `host:claude.ai` count as Claude (web) too. Claude Desktop keeps claude.ai among its own domains.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

INSERT INTO ref.app (app_key, display_name, vendor, category, source_url) VALUES
  ('claude_web', 'Claude (web)', 'anthropic', 'chat_assistant', 'https://claude.ai')
ON CONFLICT (app_key) DO NOTHING;

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('claude_web', 'any', 'inference_domain', '.claude.ai')
ON CONFLICT (app_key, platform, kind, value) DO NOTHING;

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key) VALUES
  ('app:claude_web', 'Claude (web)', 'anthropic', 'endpoint', '{}', 'claude_web')
ON CONFLICT (tool_fingerprint) DO NOTHING;
