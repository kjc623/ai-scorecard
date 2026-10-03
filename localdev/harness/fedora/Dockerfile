# syntax=docker/dockerfile:1
# Fedora-based OpenCode harness with the same lab, Docker and MCP access as Pi.

ARG GO_IMAGE=golang:1.27-bookworm
ARG DOCKER_CLI_IMAGE=docker:cli

FROM ${GO_IMAGE} AS go
FROM ${DOCKER_CLI_IMAGE} AS docker-cli

FROM fedora:latest

ARG OPENCODE_PACKAGE=@opencode/cli

RUN dnf -y install ca-certificates curl git nodejs npm postgresql ripgrep shadow-utils socat tar gzip \
 && dnf clean all \
 && npm install -g "${OPENCODE_PACKAGE}" \
 && useradd --create-home --uid 1000 --shell /bin/bash opencode \
 && git config --system --add safe.directory /workspace \
 && mkdir -p /home/opencode/.config/opencode /home/opencode/.local/share/opencode /cache/go-build /cache/go \
 && chown -R opencode:opencode /home/opencode /cache

COPY --from=go /usr/local/go /usr/local/go
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose

ENV PATH=/usr/local/go/bin:$PATH \
    GOCACHE=/cache/go-build \
    GOPATH=/cache/go \
    GOTOOLCHAIN=local

COPY opencode.json /opt/harness/opencode.json
COPY --chmod=755 entrypoint.sh /usr/local/bin/harness-entrypoint

USER opencode
WORKDIR /workspace
ENTRYPOINT ["harness-entrypoint"]
CMD ["opencode"]
