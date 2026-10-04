# syntax=docker/dockerfile:1
#
# sac/testlab-endpoint:dev — the testlab device image.
#
# capture-core is an appliance: one static binary that hosts proxy.tls, the policy engine, the spool
# and cli.shim on the device itself. This image packages the two Linux binaries the host compiles
# (capture-core, sac-bundle) into a node:24-alpine base, because the device also runs the CLI
# runtimes the lab intercepts (Node for the node-request.mjs client, curl for the curl client).
#
# Node 24 is deliberate: it is the first LTS where the built-in fetch honours NODE_USE_ENV_PROXY,
# which is the mechanism the shim uses to route a coding agent's fetch traffic (Claude Code's SDK
# uses fetch, not https.globalAgent) through proxy.tls.
#
# ca-certificates provides update-ca-certificates — the OS trust store the proxy installs the device
# CA into when --trust-install is set — and openssl/bash are the PKI and entrypoint tooling.
#
# BUILD CONTEXT IS endpoint/testlab:
#     docker build -t sac/testlab-endpoint:dev .
# (run.mjs compiles the binaries into build/ first, then `docker compose up -d --build`.)
FROM node:24-alpine

RUN apk add --no-cache ca-certificates curl openssl bash

# The REAL Claude Code terminal CLI, so the lab proves a real coding agent — not a synthetic fetch
# or curl — is captured by proxy.tls. `npm i -g @anthropic-ai/claude-code` installs the npm wrapper
# plus a platform-native optional dependency (linux-x64-musl here); the postinstall copies that
# self-contained native ELF binary over bin/claude.exe. `--allow-scripts=…` runs the postinstall,
# which npm 11 otherwise blocks. The result is a ~240MB native binary (Bun-based), NOT a Node
# process, so NODE_OPTIONS/NODE_EXTRA_CA_CERTS/NODE_USE_ENV_PROXY do not apply to it: it honours
# HTTPS_PROXY itself and trusts the device CA via the system/SSL_CERT_FILE path the shim sets.
RUN npm i -g @anthropic-ai/claude-code --allow-scripts=@anthropic-ai/claude-code

COPY build/ /app/bin/
COPY scripts/ /app/scripts/
RUN chmod +x /app/scripts/*.sh

# Every service in the compose file uses this one image; each sets its own command.
