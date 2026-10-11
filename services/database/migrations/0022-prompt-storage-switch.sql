-- Whether prompt content is stored follows the tenant's content search setting: a content upload is
-- granted only while content_search is not 'disabled'. The daily byte budget it replaces is dropped;
-- grants it denied keep their 'over_budget' reason.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

ALTER TABLE ops.tenant DROP COLUMN IF EXISTS content_budget_bytes_per_day;

ALTER TABLE ops.grant DROP CONSTRAINT IF EXISTS grant_denial_reason_check;
ALTER TABLE ops.grant ADD CONSTRAINT grant_denial_reason_check
  CHECK (denial_reason IN ('retention_expired','not_policy_relevant','over_budget','mode_not_permitted','storage_off'));
