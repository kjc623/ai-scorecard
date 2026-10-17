-- How often control-api pulls a provider connection becomes the connection's own setting, chosen by
-- the tenant's admin. Every existing connection gets the default, an hour, which is how often it was
-- pulled before.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the column, with its check, is added only when missing.

ALTER TABLE ops.provider_connection ADD COLUMN IF NOT EXISTS pull_every_minutes int NOT NULL DEFAULT 60
  CHECK (pull_every_minutes IN (15, 30, 60, 360, 1440));
