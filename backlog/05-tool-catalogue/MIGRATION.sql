-- In-place migration: tool catalogue and sanction (backlog/05-tool-catalogue).
-- Applies the schema change to an existing database without touching ingest, mart or ops data.
-- Every step is additive or a create-or-replace; re-running is safe.
--
-- An existing database is brought to this state by running this file as a role that can create
-- objects (the lab's `postgres`), inside the `shadow` database:
--
--   docker exec -i sac-authlab-postgres-1 psql -U postgres -d shadow -v ON_ERROR_STOP=1 \
--     < backlog/05-tool-catalogue/MIGRATION.sql
--
-- Like 02/03/04, this is a schema change shared by both tenants; it adds reference data and a
-- function, and a grant, and never touches a tenant's rows.
BEGIN;

-- 1. The shared fingerprint -> name catalogue. Global (no tenant_id, no RLS), like every ref table.
CREATE TABLE IF NOT EXISTS ref.tool_catalogue (
  tool_fingerprint  text PRIMARY KEY,
  display_name      text NOT NULL,
  vendor            text,
  signal_kind       text NOT NULL CHECK (signal_kind IN ('tls','process','extension','other')),
  evidence          jsonb NOT NULL DEFAULT '{}'::jsonb
);

COMMENT ON TABLE ref.tool_catalogue IS
  'The seed tool catalogue: behaviour-derived tool_fingerprint -> display name. Global reference data, not per-tenant state; ops.tool keeps the per-tenant sanction decision and an optional name override. A fingerprint held here is not thereby sanctioned: sanctioned state defaults to unknown (brief C8).';

-- 2. The seed rows. DO NOTHING on conflict so re-running never overwrites an operator's edit.
INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence) VALUES
  ('tls_b6681b043244c43f', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/v1/messages"}'),
  ('tls_5a5a41ed0bf50d9d', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/v1/messages/count_tokens"}'),
  ('tls_69f6ab029df3c019', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/event_logging/v2/batch"}'),
  ('tls_31299ef7601928f7', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/event_logging/batch"}'),
  ('tls_cb53d2b2add3d450', 'Claude Code', 'anthropic', 'tls', '{"host":"api.anthropic.com","path":"/api/claude_cli_profile"}'),
  ('tls_f32477ff734d70d1', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/chat/completions"}'),
  ('tls_4a602150609f427e', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/responses"}'),
  ('tls_15ab95c6f0615e12', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/models"}'),
  ('tls_8a9512eae8499418', 'OpenAI API', 'openai', 'tls', '{"host":"api.openai.com","path":"/v1/completions"}'),
  ('tls_11574658dafb8805', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chatgpt.com","path":"/backend-api/conversation"}'),
  ('tls_fd863543bed5e1fd', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chatgpt.com","path":"/backend-api/chat/completions"}'),
  ('tls_2af2dd0ea445e033', 'ChatGPT (web)', 'openai', 'tls', '{"host":"chat.openai.com","path":"/backend-api/conversation"}'),
  ('tls_f412811be7ac6539', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/chat/completions"}'),
  ('tls_336b980c5f15d4f0', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/v1/chat/completions"}'),
  ('tls_ba5b03450233e3fc', 'GitHub Copilot', 'github', 'tls', '{"host":"api.githubcopilot.com","path":"/models"}'),
  ('tls_3a0703d7a0dc3a68', 'GitHub Copilot', 'github', 'tls', '{"host":"copilot.microsoft.com","path":"/chat/completions"}'),
  ('tls_7659f7a5e64cd885', 'Gemini (web)', 'google', 'tls', '{"host":"gemini.google.com","path":"/"}'),
  ('tls_0320fa08a4f64851', 'Gemini API', 'google', 'tls', '{"host":"generativelanguage.googleapis.com","path":"/v1beta/models"}'),
  ('tls_a40a04ef6e27de9f', 'Perplexity (web)', 'perplexity', 'tls', '{"host":"www.perplexity.ai","path":"/"}'),
  ('tls_aefb9cd055abfffb', 'Perplexity API', 'perplexity', 'tls', '{"host":"api.perplexity.ai","path":"/chat/completions"}'),
  ('tls_9230903190dcf0dc', 'Cursor', 'cursor', 'tls', '{"host":"api2.cursor.sh","path":"/"}'),
  ('tls_14a9086e088f0d82', 'Cursor', 'cursor', 'tls', '{"host":"api.cursor.com","path":"/"}'),
  ('tls_84160351478923e0', 'Mistral API', 'mistral', 'tls', '{"host":"api.mistral.ai","path":"/v1/chat/completions"}'),
  ('tls_41b7a7dd22bb3bae', 'Groq API', 'groq', 'tls', '{"host":"api.groq.com","path":"/openai/v1/chat/completions"}'),
  ('tls_823b80bdeef13f38', 'OpenRouter', 'openrouter', 'tls', '{"host":"openrouter.ai","path":"/api/v1/chat/completions"}'),
  ('tls_ce5fbce2b73c1868', 'DeepSeek API', 'deepseek', 'tls', '{"host":"api.deepseek.com","path":"/chat/completions"}'),
  ('tls_d829c7e9598888f9', 'xAI API', 'xai', 'tls', '{"host":"api.x.ai","path":"/v1/chat/completions"}'),
  ('tls_544b80dcc1446f8b', 'Together AI', 'together', 'tls', '{"host":"api.together.xyz","path":"/v1/chat/completions"}'),
  ('tls_4fb4f5cb98d7a1c3', 'Cohere API', 'cohere', 'tls', '{"host":"api.cohere.ai","path":"/v1/chat"}'),
  ('ollama_local',     'Ollama (local)',   'ollama',   'process',   '{"basis":"loopback port map","note":"lab and simulator fingerprint for a local Ollama runtime"}'),
  ('proc_ollama',      'Ollama (local)',   'ollama',   'process',   '{"image_signature":"ollama"}'),
  ('proc_lmstudio',    'LM Studio (local)','lmstudio', 'process',   '{"image_signature":"lm studio"}'),
  ('proc_llama',       'llama.cpp (local)','llama.cpp','process',   '{"image_signature":"llama.cpp"}'),
  ('proc_vllm',        'vLLM (local)',     'vllm',     'process',   '{"image_signature":"vllm"}'),
  ('chatgpt_web',      'ChatGPT (web)',    'openai',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('claude_web',       'Claude (web)',     'anthropic','extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('copilot_chat',     'GitHub Copilot',   'github',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('cursor_ide',       'Cursor',           'cursor',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('gemini_web',       'Gemini (web)',     'google',   'extension', '{"note":"simulator/legacy extension fingerprint"}'),
  ('perplexity_web',   'Perplexity (web)', 'perplexity','extension','{"note":"simulator/legacy extension fingerprint"}'),
  ('genai.web.chat.v1:chatgpt', 'ChatGPT (web)', 'openai',   'extension', '{"note":"extension fingerprint, pre-tf1 protocol"}'),
  ('genai.web.chat.v1:claude',  'Claude (web)',  'anthropic','extension', '{"note":"extension fingerprint, pre-tf1 protocol"}'),
  ('genai.web.chat.v1:gemini',  'Gemini (web)',  'google',   'extension', '{"note":"extension fingerprint, pre-tf1 protocol"}')
ON CONFLICT (tool_fingerprint) DO NOTHING;

-- 3. The read-time resolver. Created after both tables exist (a SQL function body is checked at
--    creation). CREATE OR REPLACE is safe: the body is the whole object.
CREATE OR REPLACE FUNCTION ops.tool_display_name(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce(
    (SELECT t.display_name
       FROM ops.tool t
      WHERE t.tenant_id = ops.current_tenant()
        AND t.tool_fingerprint = p_tool_fingerprint
        AND t.display_name IS NOT NULL),
    (SELECT c.display_name
       FROM ref.tool_catalogue c
      WHERE c.tool_fingerprint = p_tool_fingerprint),
    'Unrecognised tool')
$$;

COMMENT ON FUNCTION ops.tool_display_name(text) IS
  'Fingerprint -> display name: tenant override, else shared catalogue, else "Unrecognised tool". STABLE, tenant-scoped through ops.tool RLS; the raw fingerprint is never substituted for the name.';

-- 4. Grants: the catalogue is read by the query role; the sanction write needs INSERT+UPDATE on
--    ops.tool; the new function needs EXECUTE. ops.audit INSERT already exists.
GRANT SELECT ON ref.tool_catalogue TO sac_query;
GRANT INSERT, UPDATE ON ops.tool TO sac_query;
GRANT EXECUTE ON FUNCTION ops.tool_display_name(text) TO sac_query;

COMMIT;
