#!/bin/sh
# Apply database/schema.sql once, idempotently, then exit. This is the same logic the inline
# compose entrypoint carried; it lives in a file so it can be baked into the schema image rather
# than bind-mounted. See localdev/schema/Dockerfile.
set -e

if [ "$(psql -Atc "select 1 from information_schema.schemata where schema_name='ingest'")" = "1" ]; then
  echo "schema: already applied (schema 'ingest' exists); nothing to do"
  exit 0
fi

echo "schema: applying database/schema.sql"
psql -v ON_ERROR_STOP=1 -f /schema/schema.sql
echo "schema: applied"
