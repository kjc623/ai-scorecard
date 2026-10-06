#!/usr/bin/env bash
# create-secrets.sh — create the secrets an environment's services read, in its Key Vault.
#
#   azure/scripts/create-secrets.sh <key-vault-name>
#
# Needs az (signed in, with Key Vault Secrets Officer on the vault and your address in
# keyVaultAdminIpRules) and openssl. Runs in Git Bash, macOS, Linux and Cloud Shell. Idempotent: an
# existing secret is never replaced, because replacing the directory key, the device CA or a content
# key would make existing data unreadable. Prints the policy public key the agent release pins.
#
# The device TLS certificate (sac-device-tls) is not created here: import the certificate your CA
# issued for the device hostname with `az keyvault certificate import`.
set -euo pipefail

vault="${1:?usage: create-secrets.sh <key-vault-name>}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

exists() {
  az keyvault secret show --vault-name "$vault" --name "$1" --query id --output tsv >/dev/null 2>&1
}

put_file() {
  if exists "$1"; then echo "kept    $1"; return; fi
  az keyvault secret set --vault-name "$vault" --name "$1" --file "$2" --output none
  echo "created $1"
}

put_value() {
  if exists "$1"; then echo "kept    $1"; return; fi
  az keyvault secret set --vault-name "$vault" --name "$1" --value "$2" --output none
  echo "created $1"
}

random32() { openssl rand -base64 32; }

# Product access tokens (ES256).
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$work/session.pem" 2>/dev/null
put_file sac-session-signing-key "$work/session.pem"

# Signed policy bundles (Ed25519). Its public half is pinned by the agent release.
openssl genpkey -algorithm ed25519 -out "$work/policy.pem"
put_file sac-policy-signing-key "$work/policy.pem"

# The device CA: control-api signs device certificates with it; the origins verify against it.
if exists sac-device-ca-cert || exists sac-device-ca-key; then
  if ! exists sac-device-ca-cert || ! exists sac-device-ca-key; then
    echo "error: only half of the device CA exists in $vault; restore the other half" >&2
    exit 1
  fi
  echo "kept    sac-device-ca-cert"
  echo "kept    sac-device-ca-key"
else
  # MSYS2_ARG_CONV_EXCL stops Git Bash rewriting the /CN= subject as a Windows path.
  MSYS2_ARG_CONV_EXCL="/CN=" openssl req -x509 -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$work/ca.key" -out "$work/ca.pem" -days 3650 \
    -subj "/CN=Shadow AI Capture Device CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
  put_file sac-device-ca-cert "$work/ca.pem"
  put_file sac-device-ca-key "$work/ca.key"
fi

put_value sac-internal-token "$(random32)"
put_value sac-directory-key "$(random32)"
put_value sac-cursor-key "$(random32)"
put_value sac-content-keys "v1:$(random32)"

# Print the policy public key from whatever key the vault holds now.
az keyvault secret show --vault-name "$vault" --name sac-policy-signing-key --query value --output tsv > "$work/current-policy.pem"
public_hex="$(openssl pkey -in "$work/current-policy.pem" -pubout -outform DER | tail -c 32 | od -An -v -tx1 | tr -d ' \n')"
echo
echo "Policy public key (set as the GitHub environment variable SAC_POLICY_PUBLIC_KEY):"
echo "$public_hex"
