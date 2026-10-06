-- Exports and subject erasure.
--
-- Adds the two tables the export and erasure features need, and the grants their components use.
-- A new database gets these from schema.sql; this file brings an existing database to the same shape.

CREATE TABLE ops.export (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  export_id     uuid NOT NULL,
  kind          text NOT NULL CHECK (kind IN ('list','subject')),
  source        text CHECK (source IN ('ingest.submission','mart.v_finding')),
  subject_ref   text,
  row_count     int CHECK (row_count >= 0),
  content_type  text NOT NULL,
  payload       bytea NOT NULL,
  requested_by  text NOT NULL,
  requested_at  timestamptz NOT NULL,
  expires_at    timestamptz NOT NULL,
  used_at       timestamptz,
  PRIMARY KEY (tenant_id, export_id),
  CONSTRAINT export_kind_scope CHECK (
    (kind = 'list' AND source IS NOT NULL AND subject_ref IS NULL)
    OR (kind = 'subject' AND subject_ref IS NOT NULL AND source IS NULL)),
  CONSTRAINT export_expiry_after_request CHECK (expires_at > requested_at)
);

CREATE INDEX export_expiry ON ops.export (tenant_id, expires_at);

CREATE TABLE ops.erasure_request (
  tenant_id     uuid NOT NULL REFERENCES ops.tenant(tenant_id),
  request_id    uuid NOT NULL,
  subject_ref   text NOT NULL,
  requested_by  text NOT NULL,
  requested_at  timestamptz NOT NULL,
  completed_at  timestamptz,
  receipt_id    uuid,
  PRIMARY KEY (tenant_id, request_id)
);

ALTER TABLE ops.export ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.export FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ops.export
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

ALTER TABLE ops.erasure_request ENABLE ROW LEVEL SECURITY;
ALTER TABLE ops.erasure_request FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ops.erasure_request
  USING (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

GRANT SELECT, INSERT ON ops.export TO sac_query;
GRANT UPDATE (used_at) ON ops.export TO sac_query;
GRANT SELECT, INSERT ON ops.erasure_request TO sac_query;
GRANT SELECT (tenant_id, export_id, expires_at), DELETE ON ops.export TO sac_ops;
GRANT SELECT (tenant_id, object_id, submission_id, event_id, expires_at), DELETE ON ops.content TO sac_ops;
GRANT SELECT, UPDATE ON ops.erasure_request TO sac_ops;
