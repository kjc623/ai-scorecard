-- Enforcement rules can redirect: a person browsing to a web app the rule matches is shown the
-- splash page and sent on to the rule's link, so a redirect rule has one.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

ALTER TABLE ops.enforcement_rule DROP CONSTRAINT IF EXISTS enforcement_rule_action_check;
ALTER TABLE ops.enforcement_rule ADD CONSTRAINT enforcement_rule_action_check
  CHECK (action IN ('allow','warn','block','redirect'));

ALTER TABLE ops.enforcement_rule DROP CONSTRAINT IF EXISTS enforcement_rule_redirect_link;
ALTER TABLE ops.enforcement_rule ADD CONSTRAINT enforcement_rule_redirect_link
  CHECK (action <> 'redirect' OR link IS NOT NULL);
