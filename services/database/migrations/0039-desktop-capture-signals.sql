-- Signals the endpoint agent names and decrypts desktop traffic by:
--   * ChatGPT Desktop on Windows, by its executable and its package family, so a request its
--     process sends is ChatGPT Desktop rather than ChatGPT (web).
--   * Cohere's current API host, api.cohere.com, beside api.cohere.ai.
--   * Gemini CLI's requests when its user signs in with a Google account, which go to Code Assist
--     (cloudcode-pa.googleapis.com). The Gemini Code Assist IDE extensions call the same methods,
--     and are named Gemini CLI too.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('chatgpt_desktop', 'windows', 'windows_appx', 'OpenAI.ChatGPT-Desktop_2p2nqsd0c76g0'),
  ('chatgpt_desktop', 'windows', 'windows_exe', 'ChatGPT.exe'),
  ('cohere_api', 'any', 'inference_domain', 'api.cohere.com')
ON CONFLICT (app_key, platform, kind, value) DO NOTHING;

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key) VALUES
  ('tls_32a5e5df08769f13', 'Gemini CLI', 'google', 'tls', '{"host":"cloudcode-pa.googleapis.com","path":"/v1internal:streamGenerateContent"}', 'gemini_cli'),
  ('tls_65263f31609dd072', 'Gemini CLI', 'google', 'tls', '{"host":"cloudcode-pa.googleapis.com","path":"/v1internal:generateContent"}', 'gemini_cli')
ON CONFLICT (tool_fingerprint) DO NOTHING;
