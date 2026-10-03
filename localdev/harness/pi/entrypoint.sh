#!/bin/sh
# ~/.pi lives in a volume so a login survives the container. A volume that already exists hides what
# the image put there, so the MCP config is copied in on start, and only when missing — an existing
# login, settings and an edited mcp.json are never overwritten.
set -e

AGENT_DIR="$HOME/.pi/agent"
mkdir -p "$AGENT_DIR"

if [ ! -f "$AGENT_DIR/mcp.json" ]; then
  cp /opt/harness/mcp.json "$AGENT_DIR/mcp.json"
fi

# The MCP gateway refuses any request whose Host header is not localhost (its DNS-rebinding guard),
# and a container reaches the host as host.docker.internal. So the gateway is forwarded onto this
# container's own loopback, where MCP_GATEWAY_URL can truthfully say localhost.
if [ -n "$MCP_GATEWAY_UPSTREAM" ]; then
  socat "TCP-LISTEN:${MCP_GATEWAY_UPSTREAM##*:},bind=127.0.0.1,fork,reuseaddr" "TCP:$MCP_GATEWAY_UPSTREAM" &
fi

exec "$@"
