-- Providers with no API to read: the customer forwards the log a provider writes in its own cloud
-- to control-api's intake, and every delivery presents the token the connection holds as a hash
-- (ops.provider_connection.intake_token_hash). ops.tenant_for_intake resolves a delivery's tenant
-- before any is known, as the other pre-tenant lookups do. Gemini Code Assist (a Cloud Logging
-- sink to Pub/Sub), Amazon Bedrock (CloudWatch Logs to Firehose) and Perplexity Enterprise (its
-- audit log webhook) are the first such providers, and Gemini Code Assist joins the catalogue.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

ALTER TABLE ref.provider DROP CONSTRAINT IF EXISTS provider_credential_check;
ALTER TABLE ref.provider ADD CONSTRAINT provider_credential_check
  CHECK (credential IN ('entra_app','api_key','intake'));

INSERT INTO ref.app (app_key, display_name, vendor, category, source_url) VALUES
  ('gemini_code_assist', 'Gemini Code Assist', 'google', 'ide_assistant',
   'https://marketplace.visualstudio.com/items?itemName=Google.geminicodeassist')
ON CONFLICT (app_key) DO NOTHING;

INSERT INTO ref.app_signal (app_key, platform, kind, value) VALUES
  ('gemini_code_assist', 'any', 'ide_extension_id', 'google.geminicodeassist'),
  ('gemini_code_assist', 'any', 'ide_extension_id', 'com.google.tools.ij.aiplugin')
ON CONFLICT (app_key, platform, kind, value) DO NOTHING;

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key) VALUES
  ('app:gemini_code_assist', 'Gemini Code Assist', 'google', 'endpoint', '{}', 'gemini_code_assist')
ON CONFLICT (tool_fingerprint) DO NOTHING;

INSERT INTO ref.provider (provider, display_name, vendor, credential, source_url, apps, credential_label, account_label) VALUES
  ('gemini_code_assist', 'Gemini Code Assist', 'google', 'intake',
   'https://docs.cloud.google.com/gemini/docs/log-gemini',
   '{gemini_code_assist,gemini_cli}', NULL, NULL),
  ('amazon_bedrock', 'Amazon Bedrock', 'amazon', 'intake',
   'https://docs.aws.amazon.com/bedrock/latest/userguide/model-invocation-logging.html',
   '{aws_bedrock}', NULL, NULL),
  ('perplexity_enterprise', 'Perplexity Enterprise', 'perplexity', 'intake',
   'https://intercom.help/perplexity-ai/en/articles/11652747-audit-logs',
   '{perplexity_web}', NULL, NULL)
ON CONFLICT (provider) DO NOTHING;

UPDATE ref.route_fidelity
   SET description = 'The provider''s own record of the prompt, read from its admin API or delivered from its log under the tenant''s consent (ops.provider_connection). The text the provider received, exactly; it does not see what the device composed, decided or attached.'
 WHERE source = 'provider.api';

ALTER TABLE ops.provider_connection ADD COLUMN IF NOT EXISTS intake_token_hash text;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ops.provider_connection'::regclass
                    AND conname = 'provider_connection_intake_token_hash_key') THEN
    ALTER TABLE ops.provider_connection ADD CONSTRAINT provider_connection_intake_token_hash_key
      UNIQUE (intake_token_hash);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ops.provider_connection'::regclass
                    AND conname = 'provider_connection_intake_token_is_sha256') THEN
    ALTER TABLE ops.provider_connection ADD CONSTRAINT provider_connection_intake_token_is_sha256
      CHECK (intake_token_hash ~ '^sha256:[0-9a-f]{64}$');
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                  WHERE conrelid = 'ops.provider_connection'::regclass
                    AND conname = 'provider_connection_one_secret') THEN
    ALTER TABLE ops.provider_connection ADD CONSTRAINT provider_connection_one_secret
      CHECK (credential_enc IS NULL OR intake_token_hash IS NULL);
  END IF;
END $$;

-- The lookup reads as sac_resolver, which reads only the columns it answers from.
DROP POLICY IF EXISTS pre_tenant_lookup ON ops.provider_connection;
CREATE POLICY pre_tenant_lookup ON ops.provider_connection FOR SELECT TO sac_resolver USING (true);
GRANT SELECT (tenant_id, connection_id, provider, intake_token_hash, status) ON ops.provider_connection
  TO sac_resolver;

-- The function is created, or replaced, as its owner sac_resolver, which holds the membership and
-- the CREATE on ops only while it does.
GRANT sac_resolver TO CURRENT_USER;
GRANT CREATE ON SCHEMA ops TO sac_resolver;
SET ROLE sac_resolver;
CREATE OR REPLACE FUNCTION ops.tenant_for_intake(p_provider text, p_connection uuid, p_hash text)
RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT c.tenant_id
    FROM ops.provider_connection c
   WHERE c.connection_id = p_connection
     AND c.provider = p_provider
     AND c.intake_token_hash = p_hash
     AND c.status = 'active'
$$;
COMMENT ON FUNCTION ops.tenant_for_intake(text, uuid, text) IS
  'The tenant whose active connection to p_provider, p_connection, holds the intake token whose hash is p_hash, or NULL.';
REVOKE EXECUTE ON FUNCTION ops.tenant_for_intake(text, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ops.tenant_for_intake(text, uuid, text) TO sac_control;
RESET ROLE;
REVOKE CREATE ON SCHEMA ops FROM sac_resolver;
REVOKE sac_resolver FROM CURRENT_USER;
