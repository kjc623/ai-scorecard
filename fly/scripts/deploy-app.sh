#!/usr/bin/env bash
# deploy-app.sh — deploy one pre-prod app with an image already in Fly.io's registry.
#
#   fly/scripts/deploy-app.sh <component> [fly deploy options...]
#
# Deploys fly/<component>/fly.toml to $FLY_APP_PREFIX-<component> with the image
# registry.fly.io/$FLY_APP_PREFIX-<component>:$SAC_IMAGE_TAG, creating at most one machine. The
# fly.toml must name that app, because the other apps reach it by that name. The deploy workflow
# runs it; the options carry the settings that differ per environment (-e NAME=VALUE).
set -euo pipefail

component="${1:?usage: deploy-app.sh <component> [fly deploy options...]}"
shift
app="${FLY_APP_PREFIX:?set FLY_APP_PREFIX}-$component"
config="$(cd "$(dirname "$0")/.." && pwd)/$component/fly.toml"

if ! grep -qxF "app = \"$app\"" "$config"; then
  echo "error: $config does not name the app $app; its app and the .internal addresses in fly/ use the prefix" >&2
  exit 1
fi
flyctl deploy --config "$config" --image "registry.fly.io/$app:${SAC_IMAGE_TAG:?set SAC_IMAGE_TAG}" \
  --ha=false --yes "$@"
