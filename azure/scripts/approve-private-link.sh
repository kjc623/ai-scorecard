#!/usr/bin/env bash
# approve-private-link.sh — approve Front Door's pending private endpoint connections to the
# environment's Container Apps environment. Front Door creates them pending; until they are approved
# the browser edge cannot reach the dashboard. Safe to run on every deployment.
#
#   azure/scripts/approve-private-link.sh <resource-group>
set -euo pipefail

rg="${1:?usage: approve-private-link.sh <resource-group>}"
environment_id="$(az containerapp env list --resource-group "$rg" --query '[0].id' --output tsv)"

pending="$(az network private-endpoint-connection list --id "$environment_id" \
  --query "[?properties.privateLinkServiceConnectionState.status=='Pending'].id" --output tsv)"

for connection in $pending; do
  az network private-endpoint-connection approve --id "$connection" --description "Front Door origin" --output none
  echo "approved $connection"
done
[ -n "$pending" ] || echo "no pending private endpoint connections"
