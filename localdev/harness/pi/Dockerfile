# syntax=docker/dockerfile:1
#
# The Pi coding harness (@earendil-works/pi-coding-agent), with the toolchains this repository needs:
# Node for the gates and query-api, Go for ingest-api and content-vault, psql for the lab database,
# and the docker CLI for the lab itself (the socket is mounted by the compose file).
#
# MCP: Pi's built-in MCP support is used, and mcp.json points it at the Docker Desktop MCP gateway
# on the host (localdev/harness/mcp-gateway.mjs starts that).
#
# Built and started by localdev/harness.compose.yaml; it is not part of the lab itself.

ARG GO_IMAGE=golang:1.27-bookworm
ARG NODE_IMAGE=node:22-bookworm-slim
ARG DOCKER_CLI_IMAGE=docker:cli

FROM ${GO_IMAGE} AS go
FROM ${DOCKER_CLI_IMAGE} AS docker-cli

FROM ${NODE_IMAGE}

ARG PI_VERSION=latest

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git ripgrep postgresql-client socat \
 && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose

RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION} \
 # The repository is a bind mount owned by whoever the host says; git refuses it otherwise.
 && git config --system --add safe.directory /workspace \
 # Created here so the named volumes mounted over them inherit the right owner.
 && mkdir -p /home/node/.pi/agent /cache/go-build /cache/go \
 && chown -R node:node /home/node/.pi /cache

# Build caches live in a volume, never in the repository's .tools/ (that is the Windows host's).
ENV PATH=/usr/local/go/bin:$PATH \
    GOCACHE=/cache/go-build \
    GOPATH=/cache/go \
    GOTOOLCHAIN=local

# entrypoint.sh copies this into ~/.pi/agent when the home volume has none (see that file).
COPY mcp.json /opt/harness/mcp.json
COPY --chmod=755 entrypoint.sh /usr/local/bin/harness-entrypoint

USER node

WORKDIR /workspace
ENTRYPOINT ["harness-entrypoint"]
CMD ["pi"]
