-- 09 — prompt search filters: bring an existing database to the new schema.
--
-- One change: sac_vault may read the columns a filtered content search narrows by. The vault joins
-- its own index (ingest.search_text) to ingest.submission and applies the person, tool, device,
-- mode and received-at predicates itself, because query-api is not granted the index and must not
-- do the filtering. The previous grant already let the vault read user_ref and device_id; this adds
-- tool_fingerprint, collection_mode and received_at, and is idempotent.
--
-- The lab's database already has it. Apply to any other database after task 08's migration:
--   psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/09-prompt-search-filters/MIGRATION.sql

GRANT SELECT (tenant_id, submission_id, device_id, user_ref, tool_fingerprint,
              collection_mode, content_state, received_at, expires_at)
  ON ingest.submission TO sac_vault;
