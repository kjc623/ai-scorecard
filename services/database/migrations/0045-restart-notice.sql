-- How a device reports an AI app that was already running when the agent changed the configuration
-- the app reads only at start becomes a tenant setting. Every existing tenant gets the default,
-- 'dashboard': the device shows as degraded with restart_required until the app restarts.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the column, with its check, is added only when missing.

ALTER TABLE ops.tenant ADD COLUMN IF NOT EXISTS restart_notice text NOT NULL DEFAULT 'dashboard'
  CHECK (restart_notice IN ('off','dashboard','notify','both'));
