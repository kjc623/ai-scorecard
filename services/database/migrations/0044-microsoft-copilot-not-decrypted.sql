-- copilot.microsoft.com leaves the hosts the device decrypts. Microsoft Copilot's chat runs over a
-- WebSocket, which the proxy cannot carry, so decrypting the host breaks the site in the browser;
-- the extension reads that socket instead. The tls row named a path, /chat/completions, that
-- Microsoft Copilot never requests. A request to the host is still Microsoft Copilot: the app's
-- inference domains name it.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

DELETE FROM ref.tool_catalogue
 WHERE tool_fingerprint = 'tls_3a0703d7a0dc3a68';
