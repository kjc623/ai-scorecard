#!/usr/bin/env bash
# run-job.sh — start a Container Apps job and wait for it to finish; print its log if it fails.
#
#   azure/scripts/run-job.sh <resource-group> <job>
set -euo pipefail

rg="${1:?usage: run-job.sh <resource-group> <job>}"
job="${2:?usage: run-job.sh <resource-group> <job>}"

execution="$(az containerapp job start --resource-group "$rg" --name "$job" --query name --output tsv)"
echo "started $job execution $execution"

for _ in $(seq 1 120); do
  status="$(az containerapp job execution show --resource-group "$rg" --name "$job" \
    --job-execution-name "$execution" --query properties.status --output tsv)"
  case "$status" in
    Succeeded)
      echo "$job succeeded"
      exit 0
      ;;
    Failed | Stopped | Degraded)
      echo "$job $status" >&2
      az containerapp job logs show --resource-group "$rg" --name "$job" --execution "$execution" \
        --container "$job" --format text >&2 || true
      exit 1
      ;;
  esac
  sleep 10
done

echo "$job did not finish within 20 minutes" >&2
exit 1
