-- The prompts each enforcement rule decided, for the Rules section of Insights.
--
-- mart.agg_rule_period counts, per rule and bucket, the prompts the rule decided, split by what the
-- route did. query-api reads it beside the rules themselves: what each rule matches and does, and
-- the names of the custom labels it names.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

CREATE TABLE IF NOT EXISTS mart.agg_rule_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  rule_id          text NOT NULL,
  matches          bigint NOT NULL CHECK (matches >= 0),
  blocked          bigint NOT NULL DEFAULT 0 CHECK (blocked >= 0),
  warned           bigint NOT NULL DEFAULT 0 CHECK (warned >= 0),
  logged           bigint NOT NULL DEFAULT 0 CHECK (logged >= 0),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, rule_id)
);

ALTER TABLE mart.agg_rule_period ENABLE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_rule_period FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mart.agg_rule_period;
CREATE POLICY tenant_isolation ON mart.agg_rule_period
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT ON mart.agg_rule_period TO sac_query;
GRANT SELECT, INSERT, UPDATE, DELETE ON mart.agg_rule_period TO sac_ops;
GRANT SELECT (tenant_id, position, rule_id, action, match) ON ops.enforcement_rule TO sac_query;
GRANT SELECT (tenant_id, label, display_name) ON ops.custom_label TO sac_query;
