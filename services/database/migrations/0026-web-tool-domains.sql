-- A web tool is named by its domain rather than by a list of its hosts: ChatGPT's and Perplexity's
-- inference domains cover every host under chatgpt.com and perplexity.ai, Gemini's every host under
-- gemini.google.com, and the browser extension names a request after the app whose domain covers
-- it. Prompts stored under m.chatgpt.com's tf2 fingerprint are ChatGPT (web), and a prompt to a
-- destination no app claims, `host:<host>`, is shown as its host.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

DELETE FROM ref.app_signal
 WHERE (app_key, platform, kind, value) IN (
   ('chatgpt_web', 'any', 'inference_domain', 'chatgpt.com'),
   ('gemini_web', 'any', 'inference_domain', 'gemini.google.com'),
   ('perplexity_web', 'any', 'inference_domain', 'www.perplexity.ai'));

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('chatgpt_web', 'any', 'inference_domain', '.chatgpt.com'),
  ('gemini_web', 'any', 'inference_domain', '.gemini.google.com'),
  ('perplexity_web', 'any', 'inference_domain', '.perplexity.ai')
ON CONFLICT (app_key, platform, kind, value) DO NOTHING;

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key) VALUES
  ('tf2:a64fvqypthxcqzcwgpxxeeo4kvhk5onujlq22qafww3ql422pxyq', 'ChatGPT (web)', 'openai', 'extension', '{"host":"m.chatgpt.com"}', 'chatgpt_web')
ON CONFLICT (tool_fingerprint) DO NOTHING;

CREATE OR REPLACE FUNCTION ops.tool_display_name(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce(
    (SELECT c.display_name
       FROM ref.tool_catalogue c
      WHERE c.tool_fingerprint = p_tool_fingerprint),
    nullif(substring(p_tool_fingerprint FROM '^host:(.*)$'), ''),
    'Unrecognised tool')
$$;
