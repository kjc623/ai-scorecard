-- The app catalog: ref.app and ref.app_signal with their seed, an `endpoint` fingerprint per app in
-- ref.tool_catalogue, and read access for control-api and query-api.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: tables are created and the column added only when missing,
-- the changed CHECK is dropped when present and added again, and the seed rows are inserted only
-- when absent.

CREATE TABLE IF NOT EXISTS ref.app (
  app_key       text PRIMARY KEY CHECK (app_key ~ '^[a-z][a-z0-9_]{1,63}$'),
  display_name  text NOT NULL,
  vendor        text NOT NULL,
  category      text NOT NULL CHECK (category IN ('chat_assistant','coding_agent','ide_assistant',
                  'ide','local_runtime','inference_api','ai_feature')),
  source_url    text NOT NULL
);

CREATE TABLE IF NOT EXISTS ref.app_signal (
  app_key   text NOT NULL REFERENCES ref.app,
  platform  text NOT NULL CHECK (platform IN ('windows','macos','linux','any')),
  kind      text NOT NULL CHECK (kind IN ('windows_exe','windows_uninstall_name','windows_appx',
              'macos_bundle_id','linux_package','publisher','cli_binary','npm_package',
              'pipx_package','ide_extension_id','inference_domain','listen_port','model_store')),
  value     text NOT NULL,
  PRIMARY KEY (app_key, platform, kind, value)
);

ALTER TABLE ref.tool_catalogue
  ADD COLUMN IF NOT EXISTS app_key text REFERENCES ref.app;

ALTER TABLE ref.tool_catalogue
  DROP CONSTRAINT IF EXISTS tool_catalogue_signal_kind_check,
  ADD CONSTRAINT tool_catalogue_signal_kind_check
    CHECK (signal_kind IN ('tls','process','extension','other','endpoint'));

GRANT SELECT ON ref.app, ref.app_signal TO sac_control;
GRANT SELECT ON ref.app, ref.app_signal TO sac_query;

INSERT INTO ref.app (app_key, display_name, vendor, category, source_url) VALUES
  ('anthropic_api',      'Anthropic API', 'anthropic', 'inference_api',
   'https://code.claude.com/docs/en/network-config'),
  ('aws_bedrock',        'Amazon Bedrock', 'amazon', 'inference_api',
   'https://github.com/boto/botocore/blob/develop/botocore/data/bedrock-runtime/2023-09-30/endpoint-rule-set-1.json'),
  ('azure_ai_foundry',   'Azure AI Foundry', 'microsoft', 'inference_api',
   'https://github.com/MicrosoftDocs/azure-ai-docs/blob/main/articles/foundry/foundry-models/includes/concepts-endpoints-2.md'),
  ('chatgpt_desktop',    'ChatGPT Desktop', 'openai', 'chat_assistant',
   'https://github.com/Homebrew/homebrew-cask/blob/main/Casks/c/chatgpt.rb'),
  ('claude_code',        'Claude Code', 'anthropic', 'coding_agent',
   'https://code.claude.com/docs/en/setup'),
  ('claude_code_vscode', 'Claude Code for VS Code', 'anthropic', 'ide_assistant',
   'https://code.claude.com/docs/en/vs-code'),
  ('claude_desktop',     'Claude Desktop', 'anthropic', 'chat_assistant',
   'https://code.claude.com/docs/en/desktop'),
  ('codex',              'Codex CLI', 'openai', 'coding_agent',
   'https://github.com/openai/codex'),
  ('continue',           'Continue', 'continue', 'ide_assistant',
   'https://github.com/continuedev/continue'),
  ('copilot_cli',        'GitHub Copilot CLI', 'github', 'coding_agent',
   'https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/install-copilot-cli'),
  ('cursor',             'Cursor', 'cursor', 'ide',
   'https://github.com/Homebrew/homebrew-cask/blob/main/Casks/c/cursor.rb'),
  ('gemini_cli',         'Gemini CLI', 'google', 'coding_agent',
   'https://github.com/google-gemini/gemini-cli'),
  ('github_copilot',     'GitHub Copilot', 'github', 'ide_assistant',
   'https://github.com/microsoft/vscode-copilot-chat'),
  ('google_ai_api',      'Google AI API', 'google', 'inference_api',
   'https://github.com/googleapis/python-genai/blob/main/google/genai/_api_client.py'),
  ('jetbrains',          'JetBrains IDEs', 'jetbrains', 'ide',
   'https://github.com/microsoft/winget-pkgs/tree/master/manifests/j/JetBrains'),
  ('lm_studio',          'LM Studio', 'lmstudio', 'local_runtime',
   'https://github.com/lmstudio-ai/docs'),
  ('ollama',             'Ollama', 'ollama', 'local_runtime',
   'https://github.com/ollama/ollama/blob/main/docs/faq.mdx'),
  ('openai_api',         'OpenAI API', 'openai', 'inference_api',
   'https://github.com/openai/openai-python'),
  ('vscode',             'Visual Studio Code', 'microsoft', 'ide',
   'https://github.com/microsoft/vscode-docs/blob/main/docs/setup/portable.md'),
  ('windsurf',           'Windsurf', 'codeium', 'ide',
   'https://github.com/microsoft/winget-pkgs/tree/master/manifests/c/Codeium/Windsurf')
ON CONFLICT DO NOTHING;

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('anthropic_api',      'any',     'inference_domain',       'api.anthropic.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.af-south-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-east-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-east-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-northeast-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-northeast-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-northeast-3.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-south-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-south-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-southeast-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-southeast-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-southeast-3.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-southeast-4.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-southeast-5.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-southeast-6.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ap-southeast-7.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ca-central-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.ca-west-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-central-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-central-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-north-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-south-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-south-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-west-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-west-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.eu-west-3.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.il-central-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.me-central-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.me-south-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.mx-central-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.sa-east-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.us-east-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.us-east-2.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.us-gov-east-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.us-gov-west-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.us-west-1.amazonaws.com'),
  ('aws_bedrock',        'any',     'inference_domain',       '.bedrock-runtime.us-west-2.amazonaws.com'),
  ('azure_ai_foundry',   'any',     'inference_domain',       '.models.ai.azure.com'),
  ('azure_ai_foundry',   'any',     'inference_domain',       '.openai.azure.com'),
  ('azure_ai_foundry',   'any',     'inference_domain',       '.services.ai.azure.com'),
  ('chatgpt_desktop',    'any',     'inference_domain',       'chatgpt.com'),
  ('chatgpt_desktop',    'macos',   'macos_bundle_id',        'com.openai.chat'),
  ('chatgpt_desktop',    'macos',   'macos_bundle_id',        'com.openai.codex'),
  ('claude_code',        'any',     'cli_binary',             'claude'),
  ('claude_code',        'any',     'inference_domain',       'api.anthropic.com'),
  ('claude_code',        'any',     'npm_package',            '@anthropic-ai/claude-code'),
  ('claude_code',        'windows', 'windows_exe',            'claude.exe'),
  ('claude_code_vscode', 'any',     'ide_extension_id',       'anthropic.claude-code'),
  ('claude_desktop',     'any',     'inference_domain',       'claude.ai'),
  ('claude_desktop',     'macos',   'macos_bundle_id',        'com.anthropic.claudefordesktop'),
  ('claude_desktop',     'windows', 'publisher',              'Anthropic, PBC'),
  ('claude_desktop',     'windows', 'windows_appx',           'Claude_pzs8sxrjxfjjc'),
  ('codex',              'any',     'cli_binary',             'codex'),
  ('codex',              'any',     'npm_package',            '@openai/codex'),
  ('codex',              'windows', 'windows_exe',            'codex-aarch64-pc-windows-msvc.exe'),
  ('codex',              'windows', 'windows_exe',            'codex-x86_64-pc-windows-msvc.exe'),
  ('codex',              'windows', 'windows_exe',            'codex.exe'),
  ('continue',           'any',     'ide_extension_id',       'continue.continue'),
  ('copilot_cli',        'any',     'cli_binary',             'copilot'),
  ('copilot_cli',        'any',     'npm_package',            '@github/copilot'),
  ('cursor',             'macos',   'macos_bundle_id',        'com.todesktop.230313mzl4w4u92'),
  ('cursor',             'windows', 'publisher',              'Anysphere'),
  ('gemini_cli',         'any',     'cli_binary',             'gemini'),
  ('gemini_cli',         'any',     'npm_package',            '@google/gemini-cli'),
  ('github_copilot',     'any',     'ide_extension_id',       'github.copilot-chat'),
  ('google_ai_api',      'any',     'inference_domain',       '.aiplatform.googleapis.com'),
  ('google_ai_api',      'any',     'inference_domain',       'generativelanguage.googleapis.com'),
  ('jetbrains',          'windows', 'publisher',              'JetBrains s.r.o.'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'CLion'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'DataGrip'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'GoLand'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'IntelliJ IDEA'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'JetBrains Rider'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'PhpStorm'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'PyCharm'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'RubyMine'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'RustRover'),
  ('jetbrains',          'windows', 'windows_uninstall_name', 'WebStorm'),
  ('lm_studio',          'any',     'listen_port',            '1234'),
  ('lm_studio',          'any',     'model_store',            '~/.lmstudio/models'),
  ('ollama',             'any',     'listen_port',            '11434'),
  ('ollama',             'linux',   'model_store',            '/usr/share/ollama/.ollama/models'),
  ('ollama',             'macos',   'model_store',            '~/.ollama/models'),
  ('ollama',             'windows', 'model_store',            '%USERPROFILE%\.ollama\models'),
  ('ollama',             'windows', 'windows_exe',            'ollama app.exe'),
  ('ollama',             'windows', 'windows_exe',            'ollama.exe'),
  ('openai_api',         'any',     'inference_domain',       'api.openai.com'),
  ('vscode',             'macos',   'macos_bundle_id',        'com.microsoft.VSCode'),
  ('vscode',             'windows', 'publisher',              'Microsoft Corporation'),
  ('vscode',             'windows', 'windows_exe',            'Code.exe'),
  ('windsurf',           'windows', 'publisher',              'Codeium')
ON CONFLICT DO NOTHING;

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key)
SELECT 'app:' || app_key, display_name, vendor, 'endpoint', '{}', app_key FROM ref.app
ON CONFLICT (tool_fingerprint) DO NOTHING;
