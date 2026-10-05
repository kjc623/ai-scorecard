#!/bin/sh
# The auth lab's one-shot identity step, run by the `identity-seed` compose service before control-api:
#
#   1. copy the two signing keys control-api reads as files into the shared `authidentity` volume;
#   2. link the sample tenant to the stand-in IdP (seed-identity.sql, idempotent).
#
# The material is baked into this image by build.mjs rather than bind-mounted, for the reason the
# schema and PKI images give: some Docker daemons present a bind as an empty stub.
set -e

mkdir -p /keys
for f in session-signing.key policy-signing.key; do
  cp "/identity/$f" "/keys/$f"
done
# control-api runs as a non-root user and reads these through the volume. They are lab keys.
chmod 0444 /keys/*
echo "identity: signing keys staged in the authidentity volume"

# The seed needs the task-11 schema. A fresh lab gets it from database/schema.sql; the running lab's
# database was built before it and needs the migration, which `up` does not apply. Say so, and stop
# control-api from starting against a database it cannot use.
if [ "$(psql -Atc "select to_regclass('ops.identity_connection') is not null")" != "t" ]; then
  echo "identity: ops.identity_connection does not exist. Apply the task-11 migration first:" >&2
  echo "  docker exec -i sac-authlab-postgres-1 psql -U postgres -d shadow -v ON_ERROR_STOP=1 < backlog/11-sign-in-and-roles/MIGRATION.sql" >&2
  exit 1
fi

psql -v ON_ERROR_STOP=1 -q -f /identity/seed-identity.sql
echo "identity: sample tenant linked to the stand-in IdP (http://oidc:8080), email domain lab.test"
