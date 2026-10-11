-- What device discovery found, for the Discovered tab of Insights.
--
-- mart.agg_discovery_period counts, per bucket, the devices that reported each tool by the kind of
-- finding and its item (a local model's name, an inference connection's host). The partial index
-- lets the rollup read a window of discoveries without scanning the tenant's prompts.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

CREATE INDEX IF NOT EXISTS observation_discovery ON ingest.observation (tenant_id, received_at) WHERE kind = 'discovery';

CREATE TABLE IF NOT EXISTS mart.agg_discovery_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  tool_fingerprint text NOT NULL,
  discovery_type   text NOT NULL CHECK (discovery_type IN ('app_installed','app_running','cli_installed','ide_extension','local_model','inference_connection')),
  item             text NOT NULL DEFAULT '',
  devices          int NOT NULL CHECK (devices > 0),
  last_seen        timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, tool_fingerprint, discovery_type, item)
);
CREATE INDEX IF NOT EXISTS agg_discovery_period_by_tool ON mart.agg_discovery_period (tenant_id, tool_fingerprint, bucket_size, bucket_start);

ALTER TABLE mart.agg_discovery_period ENABLE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_discovery_period FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mart.agg_discovery_period;
CREATE POLICY tenant_isolation ON mart.agg_discovery_period
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT ON mart.agg_discovery_period TO sac_query;
GRANT SELECT, INSERT, UPDATE, DELETE ON mart.agg_discovery_period TO sac_ops;
