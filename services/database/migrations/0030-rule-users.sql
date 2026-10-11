-- Enforcement rules apply to chosen people, groups, departments, units and teams, and can be
-- switched off while keeping their place.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

ALTER TABLE ops.enforcement_rule ADD COLUMN IF NOT EXISTS users jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE ops.enforcement_rule ADD COLUMN IF NOT EXISTS enabled boolean NOT NULL DEFAULT true;

ALTER TABLE ops.enforcement_rule DROP CONSTRAINT IF EXISTS enforcement_rule_users_shape;
ALTER TABLE ops.enforcement_rule ADD CONSTRAINT enforcement_rule_users_shape CHECK (
  jsonb_typeof(users) = 'array'
  AND NOT jsonb_path_exists(users, 'strict $[*] ? (@.type() != "string")')
  AND NOT jsonb_path_exists(users, '$[*] ? (!(@ like_regex "^(user:u_[0-9a-f]{32}|(group|team):[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|(department|org_unit):.+)$"))'));
