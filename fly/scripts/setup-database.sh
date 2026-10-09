#!/usr/bin/env bash
# setup-database.sh — create pre-prod's database, its administrator and the component logins on
# Supabase, once, and give each app its password.
#
#   fly/scripts/setup-database.sh <project-ref> <pooler-host> <supabase-ca.crt>
#
# Connects through the session pooler as the project's `postgres` user, whose password it reads
# from SUPABASE_DB_PASSWORD or the terminal, verifying the server against the project's CA
# certificate (Database settings → SSL configuration → Download certificate). It creates:
#
#   - sac_admin, a login with CREATEROLE that owns the database, as which migrate runs;
#   - the logins ingest-api, control-api, content-vault, query-api and jobs, to which migrate
#     grants their roles;
#   - the database `shadow`, owned by sac_admin.
#
# Each login gets a generated password, staged as SAC_PG_PASSWORD on its app (sac_admin's on
# migrate); query-api also gets the CA certificate, which it trusts the server by. No password is
# printed or logged: the statements that carry one run with log_statement off. It refuses to run
# when any of the logins or the database exists, so it never changes a password in use.
#
# The apps are $FLY_APP_PREFIX-<component> (default sac-preprod). Needs flyctl (signed in), psql
# and openssl. Runs in Git Bash, macOS and Linux.
set -euo pipefail

usage="usage: setup-database.sh <project-ref> <pooler-host> <supabase-ca.crt>"
ref="${1:?$usage}"
host="${2:?$usage}"
ca="${3:?$usage}"
prefix="${FLY_APP_PREFIX:-sac-preprod}"
database=shadow

if [ ! -r "$ca" ]; then
  echo "error: cannot read $ca" >&2
  exit 2
fi
if [ -z "${SUPABASE_DB_PASSWORD:-}" ]; then
  printf 'Password of the Supabase project'"'"'s postgres user (not shown): ' >&2
  IFS= read -rs SUPABASE_DB_PASSWORD
  printf '\n' >&2
fi
umask 077
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# psql as postgres, through the session pooler, with the server's certificate verified.
admin_psql() {
  PGPASSWORD="$SUPABASE_DB_PASSWORD" psql --no-psqlrc --quiet -v ON_ERROR_STOP=1 \
    "host=$host port=5432 dbname=postgres user=postgres.$ref sslmode=verify-full sslrootcert=$ca" "$@"
}

existing="$(admin_psql -At -c "
  SELECT string_agg(rolname, ', ' ORDER BY rolname) FROM pg_roles
   WHERE rolname IN ('sac_admin', 'ingest-api', 'control-api', 'content-vault', 'query-api', 'jobs');")"
if [ -n "$existing" ]; then
  echo "error: the logins $existing exist already; this script runs only once" >&2
  exit 1
fi
if [ -n "$(admin_psql -At -c "SELECT 1 FROM pg_database WHERE datname = '$database'")" ]; then
  echo "error: the database $database exists already; this script runs only once" >&2
  exit 1
fi

# stage NAME FILE COMPONENT sets NAME, its value read from FILE, on the app, for the next deploy.
stage() {
  { printf '%s="""' "$1"; cat "$2"; printf '"""\n'; } \
    | flyctl secrets import --stage --app "$prefix-$3" >/dev/null
}

for login in sac_admin ingest-api control-api content-vault query-api jobs; do
  openssl rand -hex 32 | tr -d '\n' > "$work/$login"
done

# The passwords reach the apps first: if creating the logins then fails, a rerun starts over.
stage SAC_PG_PASSWORD "$work/sac_admin" migrate
stage SAC_PG_PASSWORD "$work/ingest-api" ingest-api
stage SAC_PG_PASSWORD "$work/control-api" control-api
stage SAC_PG_PASSWORD "$work/content-vault" content-vault
stage SAC_PG_PASSWORD "$work/query-api" query-api
stage SAC_PG_PASSWORD "$work/jobs" jobs
openssl base64 -A -in "$ca" > "$work/ca.b64"
stage SUPABASE_CA_CERT "$work/ca.b64" query-api
echo "staged SAC_PG_PASSWORD on migrate, ingest-api, control-api, content-vault, query-api and jobs"
echo "staged SUPABASE_CA_CERT on query-api"

# The SQL travels on stdin, so no password appears in a command line. postgres is made a member
# of sac_admin only for as long as it takes to create the database sac_admin owns.
{
  echo "SET log_statement = 'none';"
  echo "BEGIN;"
  echo "CREATE ROLE sac_admin LOGIN CREATEROLE PASSWORD '$(cat "$work/sac_admin")';"
  for login in ingest-api control-api content-vault query-api jobs; do
    echo "CREATE ROLE \"$login\" LOGIN PASSWORD '$(cat "$work/$login")';"
  done
  echo "GRANT sac_admin TO CURRENT_USER;"
  echo "COMMIT;"
  echo "CREATE DATABASE $database OWNER sac_admin;"
  echo "REVOKE sac_admin FROM CURRENT_USER;"
} | admin_psql -f - || {
  echo "error: creating the logins or the database failed; if the logins exist and $database does" >&2
  echo "not, create it as postgres with: GRANT sac_admin TO postgres; CREATE DATABASE $database OWNER" >&2
  echo "sac_admin; REVOKE sac_admin FROM postgres;" >&2
  exit 1
}

echo "created the logins sac_admin, ingest-api, control-api, content-vault, query-api and jobs"
echo "created the database $database, owned by sac_admin"
