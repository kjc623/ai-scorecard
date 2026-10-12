-- What coding agents report, for the Coding agents tab of Insights.
--
-- mart.agg_agent_period counts, per bucket, each tool's model requests by model with the tokens they
-- sent and received, the actions the agent took, the people behind them, and the people behind the
-- tool on any model. The partial index lets
-- the rollup read a window of agent activity without scanning the tenant's prompts.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

CREATE INDEX IF NOT EXISTS observation_agent_activity ON ingest.observation (tenant_id, received_at) WHERE kind = 'agent_activity';

CREATE TABLE IF NOT EXISTS mart.agg_agent_period (
  tenant_id        uuid NOT NULL,
  bucket_start     timestamptz NOT NULL,
  bucket_size      text NOT NULL CHECK (bucket_size IN ('hour','day')),
  tool_fingerprint text NOT NULL,
  model            text NOT NULL DEFAULT '',
  requests         bigint NOT NULL CHECK (requests >= 0),
  actions          bigint NOT NULL CHECK (actions >= 0),
  input_tokens     bigint NOT NULL CHECK (input_tokens >= 0),
  output_tokens    bigint NOT NULL CHECK (output_tokens >= 0),
  users            int NOT NULL CHECK (users > 0),
  tool_users       int NOT NULL CHECK (tool_users >= users),
  PRIMARY KEY (tenant_id, bucket_start, bucket_size, tool_fingerprint, model)
);

ALTER TABLE mart.agg_agent_period ENABLE ROW LEVEL SECURITY;
ALTER TABLE mart.agg_agent_period FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mart.agg_agent_period;
CREATE POLICY tenant_isolation ON mart.agg_agent_period
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT ON mart.agg_agent_period TO sac_query;
GRANT SELECT, INSERT, UPDATE, DELETE ON mart.agg_agent_period TO sac_ops;
