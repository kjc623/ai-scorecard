-- Microsoft Copilot is its own vendor, not GitHub's. Two apps join the catalogue:
--   * microsoft_copilot — the consumer Copilot at copilot.microsoft.com, named by that domain and
--     every host under it. Its prompt streams over a WebSocket, which the extension reads with its
--     main-world hook; a browser request to the domain is Microsoft Copilot.
--   * m365_copilot — Microsoft 365 Copilot (BizChat / Office web), named by its web origins
--     copilot.cloud.microsoft and m365.cloud.microsoft. Its chat turn is a SignalR frame on the
--     substrate.office.com socket, read the same way; that host is shared, so it is not a domain
--     signal and the frame alone attributes the turn.
-- The copilot.microsoft.com tool_catalogue row was mis-filed under GitHub Copilot; it is repointed
-- to Microsoft Copilot. GitHub Copilot gains its Business and Enterprise plan hosts, so a prompt
-- from those plans is decrypted and named like the individual plan's.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

INSERT INTO ref.app (app_key, display_name, vendor, category, source_url) VALUES
  ('m365_copilot', 'Microsoft 365 Copilot', 'microsoft', 'chat_assistant',
   'https://learn.microsoft.com/microsoft-365-copilot/microsoft-365-copilot-requirements'),
  ('microsoft_copilot', 'Microsoft Copilot', 'microsoft', 'chat_assistant',
   'https://copilot.microsoft.com')
ON CONFLICT (app_key) DO NOTHING;

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('m365_copilot', 'any', 'inference_domain', 'copilot.cloud.microsoft'),
  ('m365_copilot', 'any', 'inference_domain', 'm365.cloud.microsoft'),
  ('microsoft_copilot', 'any', 'inference_domain', '.copilot.microsoft.com'),
  ('microsoft_copilot', 'any', 'inference_domain', 'copilot.microsoft.com')
ON CONFLICT (app_key, platform, kind, value) DO NOTHING;

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key) VALUES
  ('app:m365_copilot', 'Microsoft 365 Copilot', 'microsoft', 'endpoint', '{}', 'm365_copilot'),
  ('app:microsoft_copilot', 'Microsoft Copilot', 'microsoft', 'endpoint', '{}', 'microsoft_copilot'),
  ('tls_7a4b6619639eeb3a', 'GitHub Copilot', 'github', 'tls', '{"host":"api.business.githubcopilot.com","path":"/chat/completions"}', 'github_copilot'),
  ('tls_37b0c18f488554d9', 'GitHub Copilot', 'github', 'tls', '{"host":"api.enterprise.githubcopilot.com","path":"/chat/completions"}', 'github_copilot')
ON CONFLICT (tool_fingerprint) DO NOTHING;

-- copilot.microsoft.com was catalogued as GitHub Copilot. It is consumer Microsoft Copilot.
UPDATE ref.tool_catalogue
   SET display_name = 'Microsoft Copilot', vendor = 'microsoft', app_key = 'microsoft_copilot'
 WHERE tool_fingerprint = 'tls_3a0703d7a0dc3a68';
