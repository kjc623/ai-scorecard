-- The tenant's enforcement rules: an ordered list, set on the Settings page and delivered in the
-- policy bundle. A label must be a ref.data_class code.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the table is created only when missing, the function is
-- replaced, and the trigger and the policy are dropped when present and created again.

CREATE TABLE IF NOT EXISTS ops.enforcement_rule (
  tenant_id   uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  position    int NOT NULL CHECK (position >= 0),
  rule_id     text NOT NULL CHECK (rule_id ~ '^[a-z][a-z0-9_.-]{0,127}$'),
  action      text NOT NULL CHECK (action IN ('allow','warn','block')),
  match       jsonb NOT NULL DEFAULT '{}'::jsonb,
  message     text NOT NULL DEFAULT '' CHECK (char_length(message) <= 280),
  link        text CHECK (link LIKE 'https://_%'),
  updated_by  text NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, rule_id),
  CONSTRAINT enforcement_rule_position_unique UNIQUE (tenant_id, position),
  CONSTRAINT enforcement_rule_match_shape CHECK (
    jsonb_typeof(match) = 'object'
    AND match - ARRAY['labels','tools','categories','sanction','routes'] = '{}'::jsonb
    AND NOT jsonb_path_exists(match, 'strict $.* ? (@.type() != "array")')
    AND NOT jsonb_path_exists(match, 'strict $.*[*] ? (@.type() != "string")', '{}', true))
);

CREATE OR REPLACE FUNCTION ops.enforce_rule_labels() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_label text;
BEGIN
  -- A labels value that is not an array is left to the match shape CHECK.
  IF jsonb_typeof(NEW.match->'labels') IS DISTINCT FROM 'array' THEN
    RETURN NEW;
  END IF;
  SELECT l INTO v_label
    FROM jsonb_array_elements_text(NEW.match->'labels') AS l
   WHERE NOT EXISTS (SELECT 1 FROM ref.data_class c WHERE c.class_code = l)
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'enforcement rule % names label % outside ref.data_class', NEW.rule_id, v_label
      USING ERRCODE = 'check_violation', CONSTRAINT = 'enforcement_rule_labels_known';
  END IF;
  RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS enforcement_rule_labels_known ON ops.enforcement_rule;
CREATE TRIGGER enforcement_rule_labels_known
  BEFORE INSERT OR UPDATE OF match ON ops.enforcement_rule
  FOR EACH ROW EXECUTE FUNCTION ops.enforce_rule_labels();

ALTER TABLE ops.enforcement_rule ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.enforcement_rule FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.enforcement_rule;
CREATE POLICY tenant_isolation ON ops.enforcement_rule
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT, INSERT, DELETE ON ops.enforcement_rule TO sac_control;
