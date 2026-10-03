#!/bin/sh
set -e

CONFIG_DIR="$HOME/.config/opencode"
mkdir -p "$CONFIG_DIR" "$HOME/.local/share/opencode"

# Preserve user edits and authentication in the mounted home volume.
if [ ! -f "$CONFIG_DIR/opencode.json" ]; then
  cp /opt/harness/opencode.json "$CONFIG_DIR/opencode.json"
fi

# The host gateway checks for Host: localhost. Forward it to host.docker.internal
# from this container's own loopback, as the Pi harness does.
if [ -n "$MCP_GATEWAY_UPSTREAM" ]; then
  socat "TCP-LISTEN:${MCP_GATEWAY_UPSTREAM##*:},bind=127.0.0.1,fork,reuseaddr" "TCP:$MCP_GATEWAY_UPSTREAM" &
fi

exec "$@"
