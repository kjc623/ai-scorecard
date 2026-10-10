-- One sanction decision per tool rather than per fingerprint. ref.app gains the web, API and local
-- tools the catalogue names, every catalogued fingerprint names its app, ops.tool_sanction holds
-- the tenant's decision per tool, and ops.tool goes: a decision on a tool reaches each of its
-- fingerprints through the catalogue. An enforcement rule's tools list names tool keys, which the
-- policy bundle expands to the tool's fingerprints.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema. It runs as the tables' owner, which forced row-level security
-- binds like any other role, so the two data moves lift FORCE for this transaction and restore it.

INSERT INTO ref.app (app_key, display_name, vendor, category, source_url) VALUES
  ('chatgpt_web',    'ChatGPT (web)', 'openai', 'chat_assistant', 'https://chatgpt.com'),
  ('cohere_api',     'Cohere API', 'cohere', 'inference_api', 'https://docs.cohere.com/reference/chat'),
  ('deepseek_api',   'DeepSeek API', 'deepseek', 'inference_api', 'https://api-docs.deepseek.com'),
  ('gemini_web',     'Gemini (web)', 'google', 'chat_assistant', 'https://gemini.google.com'),
  ('groq_api',       'Groq API', 'groq', 'inference_api', 'https://console.groq.com/docs/api-reference'),
  ('llama_cpp',      'llama.cpp', 'llama.cpp', 'local_runtime', 'https://github.com/ggml-org/llama.cpp'),
  ('mistral_api',    'Mistral API', 'mistral', 'inference_api', 'https://docs.mistral.ai/api'),
  ('openrouter',     'OpenRouter', 'openrouter', 'inference_api', 'https://openrouter.ai/docs/api-reference/overview'),
  ('perplexity_api', 'Perplexity API', 'perplexity', 'inference_api', 'https://docs.perplexity.ai/api-reference/chat-completions-post'),
  ('perplexity_web', 'Perplexity (web)', 'perplexity', 'chat_assistant', 'https://www.perplexity.ai'),
  ('together_ai',    'Together AI', 'together', 'inference_api', 'https://docs.together.ai/reference/chat-completions-1'),
  ('vllm',           'vLLM', 'vllm', 'local_runtime', 'https://github.com/vllm-project/vllm'),
  ('xai_api',        'xAI API', 'xai', 'inference_api', 'https://docs.x.ai/docs/api-reference')
ON CONFLICT (app_key) DO NOTHING;

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('chatgpt_web', 'any', 'inference_domain', 'chatgpt.com'),
  ('chatgpt_web', 'any', 'inference_domain', 'chat.openai.com'),
  ('cohere_api', 'any', 'inference_domain', 'api.cohere.ai'),
  ('deepseek_api', 'any', 'inference_domain', 'api.deepseek.com'),
  ('gemini_web', 'any', 'inference_domain', 'gemini.google.com'),
  ('groq_api', 'any', 'inference_domain', 'api.groq.com'),
  ('llama_cpp', 'any', 'listen_port', '8080'),
  ('mistral_api', 'any', 'inference_domain', 'api.mistral.ai'),
  ('openrouter', 'any', 'inference_domain', 'openrouter.ai'),
  ('perplexity_api', 'any', 'inference_domain', 'api.perplexity.ai'),
  ('perplexity_web', 'any', 'inference_domain', 'www.perplexity.ai'),
  ('together_ai', 'any', 'inference_domain', 'api.together.xyz'),
  ('vllm', 'any', 'listen_port', '8000'),
  ('xai_api', 'any', 'inference_domain', 'api.x.ai')
ON CONFLICT (app_key, platform, kind, value) DO NOTHING;

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key)
SELECT 'app:' || app_key, display_name, vendor, 'endpoint', '{}', app_key
  FROM ref.app
 WHERE app_key IN ('chatgpt_web', 'cohere_api', 'deepseek_api', 'gemini_web', 'groq_api', 'llama_cpp',
                   'mistral_api', 'openrouter', 'perplexity_api', 'perplexity_web', 'together_ai', 'vllm',
                   'xai_api')
ON CONFLICT (tool_fingerprint) DO NOTHING;

-- The catalogue's tls, extension and process rows, by the name each was seeded under.
UPDATE ref.tool_catalogue c
   SET app_key = m.app_key
  FROM (VALUES
    ('Claude Code', 'claude_code'), ('OpenAI API', 'openai_api'), ('ChatGPT (web)', 'chatgpt_web'),
    ('GitHub Copilot', 'github_copilot'), ('Gemini (web)', 'gemini_web'), ('Gemini API', 'google_ai_api'),
    ('Perplexity (web)', 'perplexity_web'), ('Perplexity API', 'perplexity_api'), ('Cursor', 'cursor'),
    ('Mistral API', 'mistral_api'), ('Groq API', 'groq_api'), ('OpenRouter', 'openrouter'),
    ('DeepSeek API', 'deepseek_api'), ('xAI API', 'xai_api'), ('Together AI', 'together_ai'),
    ('Cohere API', 'cohere_api'), ('Ollama (local)', 'ollama'), ('LM Studio (local)', 'lm_studio'),
    ('llama.cpp (local)', 'llama_cpp'), ('vLLM (local)', 'vllm')) AS m(display_name, app_key)
 WHERE c.app_key IS NULL AND c.display_name = m.display_name;

ALTER TABLE ref.tool_catalogue ALTER COLUMN app_key SET NOT NULL;

CREATE TABLE IF NOT EXISTS ops.tool_sanction (
  tenant_id         uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  tool_key          text NOT NULL REFERENCES ref.app,
  sanctioned_state  text NOT NULL CHECK (sanctioned_state IN ('sanctioned','unsanctioned','unknown')),
  decided_by        text,
  decided_at        timestamptz,
  PRIMARY KEY (tenant_id, tool_key),
  CONSTRAINT tool_sanction_attributed
    CHECK (sanctioned_state = 'unknown' OR (decided_by IS NOT NULL AND decided_at IS NOT NULL))
);

-- The decisions already made, carried over: a tool takes the latest decision among its
-- fingerprints. Before the new table is forced, and with ops.tool readable by its owner.
DO $$
BEGIN
  IF to_regclass('ops.tool') IS NOT NULL THEN
    ALTER TABLE ops.tool NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE ops.tool_sanction NO FORCE ROW LEVEL SECURITY;
    INSERT INTO ops.tool_sanction (tenant_id, tool_key, sanctioned_state, decided_by, decided_at)
    SELECT DISTINCT ON (t.tenant_id, c.app_key)
           t.tenant_id, c.app_key, t.sanctioned_state, t.decided_by, t.decided_at
      FROM ops.tool t
      JOIN ref.tool_catalogue c ON c.tool_fingerprint = t.tool_fingerprint
     WHERE t.sanctioned_state <> 'unknown'
     ORDER BY t.tenant_id, c.app_key, t.decided_at DESC
    ON CONFLICT (tenant_id, tool_key) DO NOTHING;
  END IF;
END $$;

ALTER TABLE ops.tool_sanction ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.tool_sanction FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.tool_sanction;
CREATE POLICY tenant_isolation ON ops.tool_sanction
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());
GRANT SELECT, INSERT, UPDATE ON ops.tool_sanction TO sac_control;
GRANT SELECT ON ops.tool_sanction TO sac_query;

-- A per-tool override names a tool key: the overrides on a tool's fingerprints become one on the
-- tool, at the narrowest of their modes; an override on a fingerprint the catalogue does not know
-- is dropped.
ALTER TABLE ops.tenant NO FORCE ROW LEVEL SECURITY;
UPDATE ops.tenant t
   SET scope_overrides = coalesce((SELECT jsonb_object_agg(x.app_key, x.mode)
                                     FROM (SELECT c.app_key, min(e.value) AS mode
                                             FROM jsonb_each_text(t.scope_overrides) AS e(key, value)
                                             JOIN ref.tool_catalogue c ON c.tool_fingerprint = e.key
                                            GROUP BY c.app_key) AS x), '{}'::jsonb)
 WHERE EXISTS (SELECT 1 FROM jsonb_each_text(t.scope_overrides) AS e(key, value)
                WHERE NOT EXISTS (SELECT 1 FROM ref.app a WHERE a.app_key = e.key));
ALTER TABLE ops.tenant FORCE ROW LEVEL SECURITY;

-- A rule's tools list names tool keys: each fingerprint becomes its tool, a fingerprint the catalogue
-- does not know is dropped, and a list already of tool keys is left as it is.
ALTER TABLE ops.enforcement_rule NO FORCE ROW LEVEL SECURITY;
UPDATE ops.enforcement_rule r
   SET match = r.match || jsonb_build_object('tools',
         coalesce((SELECT jsonb_agg(DISTINCT c.app_key ORDER BY c.app_key)
                     FROM jsonb_array_elements_text(r.match -> 'tools') AS f(fp)
                     JOIN ref.tool_catalogue c ON c.tool_fingerprint = f.fp), '[]'::jsonb))
 WHERE jsonb_typeof(r.match -> 'tools') = 'array'
   AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(r.match -> 'tools') AS f(fp)
                WHERE NOT EXISTS (SELECT 1 FROM ref.app a WHERE a.app_key = f.fp));
ALTER TABLE ops.enforcement_rule FORCE ROW LEVEL SECURITY;

-- The reads: the view joins the catalogue to the tool's decision, and the name comes from the
-- catalogue alone.
CREATE OR REPLACE VIEW mart.v_tool_usage
WITH (security_invoker = true) AS
SELECT a.tenant_id,
       a.bucket_start,
       a.bucket_size,
       a.tool_fingerprint,
       coalesce(c.display_name, a.tool_fingerprint) AS display_name,
       s.sanctioned_state,
       a.submissions,
       a.users,
       a.bytes_total,
       a.blocked,
       a.warned,
       a.logged
  FROM mart.agg_tool_period a
  LEFT JOIN ref.tool_catalogue c
    ON c.tool_fingerprint = a.tool_fingerprint
  LEFT JOIN ops.tool_sanction s
    ON s.tenant_id = a.tenant_id
   AND s.tool_key = c.app_key;

CREATE OR REPLACE FUNCTION ops.tool_display_name(p_tool_fingerprint text)
RETURNS text
LANGUAGE sql STABLE AS $$
  SELECT coalesce(
    (SELECT c.display_name
       FROM ref.tool_catalogue c
      WHERE c.tool_fingerprint = p_tool_fingerprint),
    'Unrecognised tool')
$$;

DROP TABLE IF EXISTS ops.tool;
