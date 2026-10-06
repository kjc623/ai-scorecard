-- 0002: the Settings page's collection-mode columns and their enforcement.
-- A requested collection mode distinct from the ceiling, an optional narrower per-tool override,
-- and a trigger refusing an override wider than the requested mode.

ALTER TABLE ops.tenant
  ADD COLUMN collection_mode text CHECK (collection_mode IN ('m0','m1','m2','m3'));

ALTER TABLE ops.tenant
  ADD COLUMN scope_overrides jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE ops.tenant
  ADD CONSTRAINT tenant_collection_within_ceiling
    CHECK (collection_mode IS NULL
           OR ops.mode_rank(collection_mode) <= ops.mode_rank(ceiling_mode));

CREATE FUNCTION ops.enforce_scope_overrides() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_requested int := ops.mode_rank(coalesce(NEW.collection_mode, NEW.ceiling_mode));
  v_key  text;
  v_mode text;
  v_rank int;
BEGIN
  FOR v_key, v_mode IN
    SELECT e.key, e.value
      FROM jsonb_each_text(NEW.scope_overrides) AS e(key, value)
  LOOP
    v_rank := ops.mode_rank(v_mode);
    IF v_rank IS NULL THEN
      RAISE EXCEPTION 'scope override % has an unrecognised collection mode', v_key;
    END IF;
    IF v_rank > v_requested THEN
      RAISE EXCEPTION 'scope override % (mode %) is wider than the tenant''s requested mode (%)',
        v_key, v_mode, coalesce(NEW.collection_mode, NEW.ceiling_mode);
    END IF;
  END LOOP;
  RETURN NEW;
END $$;

CREATE TRIGGER scope_overrides_within_requested
  BEFORE INSERT OR UPDATE OF collection_mode, ceiling_mode, scope_overrides ON ops.tenant
  FOR EACH ROW EXECUTE FUNCTION ops.enforce_scope_overrides();
