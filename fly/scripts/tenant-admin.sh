#!/usr/bin/env bash
# tenant-admin.sh — run a control-api tenant command in pre-prod, and print its output.
#
#   fly/scripts/tenant-admin.sh tenant create --name "Endpoint Test" --ceiling m3 --actor you@vendor.com
#   fly/scripts/tenant-admin.sh tenant invite --tenant <id> --domain contoso.com --actor you@vendor.com
#
# Runs `control-api <arguments>` in the running control-api machine over `fly ssh console`, with
# that machine's settings and database login. The app is $FLY_APP_PREFIX-control-api (default
# sac-preprod). Needs flyctl, signed in.
set -euo pipefail

if [ $# -eq 0 ]; then
  echo "usage: tenant-admin.sh <control-api arguments...>" >&2
  exit 2
fi
prefix="${FLY_APP_PREFIX:-sac-preprod}"

# The command travels as one string that the machine splits into arguments, honouring double
# quotes; each argument is double-quoted, with a backslash before any \ or " inside it.
command="/usr/local/bin/control-api"
for arg in "$@"; do
  arg="${arg//\\/\\\\}"
  command="$command \"${arg//\"/\\\"}\""
done

flyctl ssh console --quiet --app "$prefix-control-api" --command "$command"
