#!/usr/bin/env bash
# create-secrets.sh — create the secrets pre-prod's apps read, as Fly.io secrets.
#
#   fly/scripts/create-secrets.sh <key-dir> <device-cert-chain.pem> <device-key.pem>
#
# Generates the environment's keys and token in the formats the components read, and stages each
# on the apps that read it (fly/RUNBOOK.md lists them); the next deploy applies them. The device
# hostname's certificate chain and key go to the edge, and the vendor Entra application's client
# secret, read from the terminal, goes to control-api.
#
# A secret that exists is never replaced: replacing the device CA, the policy key, the directory
# key or a content key would strand enrolled devices or make stored data unreadable. Each key this
# run generates is also written to <key-dir>, a new or empty directory: keep its files outside
# Fly.io, which never shows a secret's value again, then delete the directory. Prints the policy public key the
# agent release pins, and no other key.
#
# The apps are $FLY_APP_PREFIX-<component> (default sac-preprod). Needs flyctl (signed in) and
# openssl. Runs in Git Bash, macOS and Linux.
set -euo pipefail

usage="usage: create-secrets.sh <key-dir> <device-cert-chain.pem> <device-key.pem>"
keys="${1:?$usage}"
device_cert="${2:?$usage}"
device_key="${3:?$usage}"
prefix="${FLY_APP_PREFIX:-sac-preprod}"

if [ ! -r "$device_cert" ] || [ ! -r "$device_key" ]; then
  echo "error: cannot read $device_cert and $device_key" >&2
  exit 2
fi
if [ -e "$keys" ] && [ -n "$(ls -A "$keys")" ]; then
  echo "error: $keys is not empty; name a new directory for the generated keys" >&2
  exit 2
fi
umask 077
mkdir -p "$keys"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The names of every app's secrets, one per line in $work/<component>.names.
for component in edge ingest-api control-api content-vault query-api dashboard; do
  flyctl secrets list --app "$prefix-$component" --json \
    | sed -n 's/^ *"name": *"\([^"]*\)".*/\1/p' > "$work/$component.names"
done

# How many of the apps hold the secret: all, none or some.
holders() {
  local name="$1" component n=0 total=0
  shift
  for component in "$@"; do
    total=$((total + 1))
    if grep -qxF "$name" "$work/$component.names"; then n=$((n + 1)); fi
  done
  if [ "$n" -eq "$total" ]; then echo all; elif [ "$n" -eq 0 ]; then echo none; else echo some; fi
}

# stage NAME FILE COMPONENT... sets NAME, its value read from FILE, on each app, for the next
# deploy to apply. The value travels on stdin between """, which may span lines.
stage() {
  local name="$1" file="$2" component
  shift 2
  for component in "$@"; do
    { printf '%s="""' "$name"; cat "$file"; printf '"""\n'; } \
      | flyctl secrets import --stage --app "$prefix-$component" >/dev/null \
      || { echo "error: could not set $name on $prefix-$component" >&2; exit 1; }
  done
}

# create NAME FILE COMPONENT... stages NAME on the apps unless they all hold it already. It fails
# when only some of them do, and returns 1 when the secret was kept. (It runs as an `if`
# condition, where set -e does not apply, so every failure exits explicitly.)
create() {
  local name="$1" file="$2"
  shift 2
  case "$(holders "$name" "$@")" in
    all) echo "kept    $name"; return 1 ;;
    some) echo "error: only some of $* hold $name; set it on the others from your copy" >&2; exit 1 ;;
  esac
  stage "$name" "$file" "$@"
  echo "created $name"
}

# put NAME FILE COMPONENT... is create, keeping a copy of the generated value in the key directory.
put() {
  if create "$@"; then cp "$2" "$keys/$1"; fi
}

# put_file NAME FILE COMPONENT... is put for a secret that a fly.toml [[files]] entry writes out as
# a file: the secret is the file's content, base64-encoded.
put_file() {
  local name="$1" file="$2"
  shift 2
  openssl base64 -A -in "$file" > "$file.b64"
  if create "$name" "$file.b64" "$@"; then cp "$file" "$keys/$name.pem"; fi
}

random32() { openssl rand -base64 32 | tr -d '\n' > "$1"; }

# Product access tokens (ES256).
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$work/session.pem" 2>/dev/null
put_file SESSION_SIGNING_KEY "$work/session.pem" control-api

# Signed policy bundles (Ed25519). Its public half is pinned by the agent release.
openssl genpkey -algorithm ed25519 -out "$work/policy.pem"
put_file POLICY_SIGNING_KEY "$work/policy.pem" control-api

# The device CA: control-api signs device certificates with it; ingest-api and control-api verify
# against it. Its halves exist together or not at all.
ca_cert="$(holders SAC_CA_CERT_PEM ingest-api control-api)"
ca_key="$(holders SAC_CA_KEY_PEM control-api)"
if [ "$ca_cert" = none ] && [ "$ca_key" = none ]; then
  # MSYS2_ARG_CONV_EXCL stops Git Bash rewriting the /CN= subject as a Windows path.
  MSYS2_ARG_CONV_EXCL="/CN=" openssl req -x509 -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$work/ca.key" -out "$work/ca.pem" -days 3650 \
    -subj "/CN=Shadow AI Capture Device CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
  put SAC_CA_CERT_PEM "$work/ca.pem" ingest-api control-api
  put SAC_CA_KEY_PEM "$work/ca.key" control-api
elif [ "$ca_cert" = all ] && [ "$ca_key" = all ]; then
  echo "kept    SAC_CA_CERT_PEM"
  echo "kept    SAC_CA_KEY_PEM"
else
  echo "error: only part of the device CA exists on ingest-api and control-api; restore the rest from your copy" >&2
  exit 1
fi

random32 "$work/internal-token"
put SAC_INTERNAL_TOKEN "$work/internal-token" control-api dashboard
random32 "$work/directory-key"
put SAC_DIRECTORY_KEY "$work/directory-key" control-api
random32 "$work/cursor-key"
put SAC_CURSOR_KEY "$work/cursor-key" query-api
{ printf 'v1:'; openssl rand -base64 32 | tr -d '\n'; } > "$work/content-keys"
put SAC_CONTENT_KEYS "$work/content-keys" content-vault

# The device hostname's certificate from a public CA, which the edge presents to devices. The owner
# holds these files already, so no copy is made.
create EDGE_TLS_CERT_PEM "$device_cert" edge || true
create EDGE_TLS_KEY_PEM "$device_key" edge || true

# The vendor Entra application's client secret, which the owner holds already.
if [ "$(holders SAC_ENTRA_CLIENT_SECRET control-api)" = all ]; then
  echo "kept    SAC_ENTRA_CLIENT_SECRET"
else
  printf 'Client secret of the vendor Entra application (not shown): ' >&2
  IFS= read -rs entra_secret
  printf '\n' >&2
  if [ -z "$entra_secret" ]; then
    echo "error: no client secret given" >&2
    exit 1
  fi
  printf '%s' "$entra_secret" > "$work/entra-client-secret"
  unset entra_secret
  create SAC_ENTRA_CLIENT_SECRET "$work/entra-client-secret" control-api
fi

echo
if [ -f "$keys/POLICY_SIGNING_KEY.pem" ]; then
  echo "Policy public key (set as the GitHub environment variable SAC_POLICY_PUBLIC_KEY):"
  openssl pkey -in "$keys/POLICY_SIGNING_KEY.pem" -pubout -outform DER | tail -c 32 | od -An -v -tx1 | tr -d ' \n'
  echo
else
  echo "The policy key was kept: SAC_POLICY_PUBLIC_KEY stays as it is."
fi
echo
if [ -z "$(ls -A "$keys")" ]; then
  rmdir "$keys"
  echo "Every generated secret existed already: no key was generated."
  exit 0
fi
echo "The keys this run generated are in $keys. Keep a copy of each outside Fly.io, which never"
echo "shows a secret's value again, then delete the directory. Pre-prod's devices and stored"
echo "content survive a move of the services only if these keys do."
