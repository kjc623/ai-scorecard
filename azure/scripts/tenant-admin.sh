#!/usr/bin/env bash
# tenant-admin.sh — run a control-api tenant command in an environment, and print its output.
#
#   azure/scripts/tenant-admin.sh <resource-group> tenant create --name "Contoso" --region eastus
#   azure/scripts/tenant-admin.sh <resource-group> tenant invite --tenant <id> --domain contoso.com
#
# Starts the environment's `tenant-admin` job (the control-api image, running as control-api's
# identity) with the given arguments and follows the execution's log. The arguments go in the start
# request's body, so options such as --name reach control-api rather than az. Needs az and node.
set -euo pipefail

rg="${1:?usage: tenant-admin.sh <resource-group> <control-api arguments...>}"
shift
[ $# -gt 0 ] || { echo "usage: tenant-admin.sh <resource-group> <control-api arguments...>" >&2; exit 2; }

job_id="$(az containerapp job show --resource-group "$rg" --name tenant-admin --query id --output tsv)"
image="$(az containerapp job show --resource-group "$rg" --name tenant-admin --query 'properties.template.containers[0].image' --output tsv)"

body="$(node -e 'const [image, ...args] = process.argv.slice(1); console.log(JSON.stringify({ containers: [{ name: "tenant-admin", image, args }] }))' "$image" "$@")"
execution="$(az rest --method post --url "https://management.azure.com${job_id}/start?api-version=2024-03-01" \
  --body "$body" --query name --output tsv)"

echo "started execution $execution"
az containerapp job logs show --resource-group "$rg" --name tenant-admin --execution "$execution" \
  --container tenant-admin --follow --format text
