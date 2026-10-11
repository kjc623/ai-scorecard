-- Custom labels: the tenant's own labels, which the device matches against a prompt's words and
-- its attached files' names, and which an enforcement rule may name beside the data classes.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema: the table is created only when missing, the functions are
-- replaced, and the triggers and the policy are dropped when present and created again.

-- The tenant's own labels, set on the Settings page beside the enforcement rules and delivered in
-- the policy bundle's custom_labels. The device labels a prompt with one when its text holds one of
-- the words or customer names as whole words in any case, or a file attached to it has one of the
-- extensions or a name matching one of the file name patterns (* and ? are wildcards). The label is
-- the class code the envelope carries and an enforcement rule names; the display name is what people
-- see. A label a rule names cannot be deleted (ops.guard_custom_label_in_use).
CREATE TABLE IF NOT EXISTS ops.custom_label (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  label         text NOT NULL CHECK (label ~ '^custom_[a-z0-9_]{1,56}$'),
  display_name  text NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 80 AND display_name = btrim(display_name)),
  terms         jsonb NOT NULL,
  updated_by    text NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, label),
  CONSTRAINT custom_label_display_name_unique UNIQUE (tenant_id, display_name) DEFERRABLE INITIALLY DEFERRED,
  -- terms holds the four lists, each an array of strings, with at least one term among them.
  CONSTRAINT custom_label_terms_shape CHECK (
    jsonb_typeof(terms) = 'object'
    AND terms ?& ARRAY['words','customer_names','file_extensions','file_names']
    AND terms - ARRAY['words','customer_names','file_extensions','file_names'] = '{}'::jsonb
    AND NOT jsonb_path_exists(terms, 'strict $.* ? (@.type() != "array")')
    AND NOT jsonb_path_exists(terms, 'strict $.*[*] ? (@.type() != "string")', '{}', true)
    AND jsonb_path_exists(terms, '$.*[*]'))
);

-- An enforcement rule's labels are classifier classes or the tenant's custom labels: any other
-- label could never match, so the rule is refused rather than stored silently inert.
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
     AND NOT EXISTS (SELECT 1 FROM ops.custom_label c WHERE c.tenant_id = NEW.tenant_id AND c.label = l)
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'enforcement rule % names label %, which is neither a data class nor a custom label of the tenant', NEW.rule_id, v_label
      USING ERRCODE = 'check_violation', CONSTRAINT = 'enforcement_rule_labels_known';
  END IF;
  RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS enforcement_rule_labels_known ON ops.enforcement_rule;
CREATE TRIGGER enforcement_rule_labels_known
  BEFORE INSERT OR UPDATE OF match ON ops.enforcement_rule
  FOR EACH ROW EXECUTE FUNCTION ops.enforce_rule_labels();

-- A custom label an enforcement rule names stays while the rule does: deleting it would leave the
-- rule naming a label nothing produces.
CREATE OR REPLACE FUNCTION ops.guard_custom_label_in_use() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_rule text;
BEGIN
  SELECT r.rule_id INTO v_rule
    FROM ops.enforcement_rule r
   WHERE r.tenant_id = OLD.tenant_id AND r.match->'labels' ? OLD.label
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'custom label % is named by enforcement rule %', OLD.label, v_rule
      USING ERRCODE = 'check_violation', CONSTRAINT = 'custom_label_in_use';
  END IF;
  RETURN OLD;
END $$;

DROP TRIGGER IF EXISTS custom_label_in_use ON ops.custom_label;
CREATE TRIGGER custom_label_in_use
  BEFORE DELETE ON ops.custom_label
  FOR EACH ROW EXECUTE FUNCTION ops.guard_custom_label_in_use();

ALTER TABLE ops.custom_label ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.custom_label FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON ops.custom_label;
CREATE POLICY tenant_isolation ON ops.custom_label
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT, INSERT, DELETE ON ops.custom_label TO sac_control;
GRANT UPDATE (display_name, terms, updated_by, updated_at) ON ops.custom_label TO sac_control;
