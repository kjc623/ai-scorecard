#!/bin/sh
# Mint the upstream PKI the device re-originates to.
#
# The device intercepts api.anthropic.test:443 and re-originates the connection to the upstream it
# resolves. proxy.tls dials the upstream with the system root pool, so the device installs
# upstream-ca.crt into its OS trust store and this leaf must chain to it. The device's OWN CA is a
# different root, minted separately by sac-bundle inside the device container.
set -eu

PKI=/pki
mkdir -p "$PKI"

# 1. The upstream CA: a self-signed root the device will trust (installed via update-ca-certificates).
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 30 \
  -keyout "$PKI/upstream-ca.key" -out "$PKI/upstream-ca.crt" \
  -subj "/CN=Testlab Upstream CA/O=Shadow AI Capture"

# 2. The server leaf for api.anthropic.test, signed by the upstream CA, with the DNS SAN a modern
#    client's hostname verification requires.
openssl req -new -newkey rsa:2048 -nodes -sha256 \
  -keyout "$PKI/upstream-server.key" -out "$PKI/upstream-server.csr" \
  -subj "/CN=api.anthropic.test/O=Shadow AI Capture"

cat > "$PKI/san.cnf" <<'EOF'
subjectAltName=DNS:api.anthropic.test
EOF

openssl x509 -req -in "$PKI/upstream-server.csr" \
  -CA "$PKI/upstream-ca.crt" -CAkey "$PKI/upstream-ca.key" \
  -CAcreateserial -days 30 -sha256 \
  -extfile "$PKI/san.cnf" \
  -out "$PKI/upstream-server.crt"

chmod -R a+rX "$PKI"
echo "pki: minted upstream CA and server cert for api.anthropic.test:"
ls -la "$PKI"
