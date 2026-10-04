#!/bin/sh
# The device entrypoint. This container IS the appliance:
#
#   1. install the upstream CA into the OS trust store, so proxy.tls trusts the upstream it
#      re-originates to (api.anthropic.test presents a leaf signed by this CA);
#   2. mint the device CA + signed policy bundle with sac-bundle (offline provisioning);
#   3. start capture-core with the trust/CA/shim/provider flags an enrolment profile would carry,
#      and record its PID;
#   4. hold the container alive after capture-core exits, so the host driver can exec the
#      trust-removal checks.
set -eu

# 1. Trust the upstream the proxy re-originates to.
cp /pki/upstream-ca.crt /usr/local/share/ca-certificates/upstream-ca.crt
update-ca-certificates

# 2. Mint the device CA and the signed policy bundle. --node-require makes the bundle carry
#    cli_shim.node_require, which is what makes cli.shim write node-proxy.cjs and NODE_OPTIONS.
mkdir -p /state/bundle
/app/bin/sac-bundle \
  --out /state/bundle \
  --device-id 22222222-2222-4222-8222-222222222222 \
  --hosts api.anthropic.test \
  --listen 127.0.0.1:8843 \
  --canary api.anthropic.test:443 \
  --proxy-addr 127.0.0.1:8843 \
  --runtimes go,node,python \
  --shim-managed-dir /state/shim \
  --node-require \
  --tenant-default m1

# 3. Start the agent in the background. The flags are exactly the enrolment profile shape, with the
#    appliance's own /state as the spool/shim/bundle/health directory.
/app/bin/capture-core \
  --tenant-id 11111111-1111-4111-8111-111111111111 \
  --device-id 22222222-2222-4222-8222-222222222222 \
  --user-ref lab \
  --spool-dir /state/spool \
  --spool-key /state/spool.key \
  --health-file /state/health.jsonl \
  --health-interval 1s \
  --retention 1h \
  --bundle /state/bundle/bundle.json \
  --policy-key "$(cat /state/bundle/policy-key.pub)" \
  --policy-key-id policy-key-1 \
  --proxy-tls \
  --proxy-tls-listen 127.0.0.1:8843 \
  --proxy-tls-canary api.anthropic.test:443 \
  --proxy-loopback=false \
  --ca-cert /state/bundle/ca.pem \
  --ca-key /state/bundle/ca.key \
  --trust-install \
  --trust-remove-on-stop \
  --cli-shim \
  --shim-dir /state/shim \
  --log-level debug \
  --log-format text &
CAPTURE_PID=$!
echo "$CAPTURE_PID" > /state/capture-core.pid
echo "device: capture-core started (pid=$CAPTURE_PID)"

# 4. Wait for capture-core to exit (the driver sends SIGTERM for the trust-removal check), then hold
#    the container alive so the driver can exec the removal assertions against it.
wait "$CAPTURE_PID" || true
echo "device: capture-core exited; holding the container open for the driver"
exec tail -f /dev/null
