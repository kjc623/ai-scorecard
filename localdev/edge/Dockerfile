# syntax=docker/dockerfile:1
#
# edge — the local stand-in for Azure Application Gateway (ADR 0020 decision 1, decision 5).
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#     docker build --target lab -t sac/edge:lab -f localdev/edge/Dockerfile .
#
# ONE STAGE, because the edge is a standard-library-only program with no module dependency and no
# runtime data file. localdev/build.mjs cross-compiles it on the host (GOPROXY=off), so this image
# never needs a golang image or a network, exactly like the other lab images.
ARG LAB_BASE=alpine:latest
FROM ${LAB_BASE} AS lab

# Path to the binary produced by localdev/build.mjs, relative to the build context (repository root).
ARG BINARY=localdev/edge/bin/edge

# Same non-root convention the services use (uid 10001); the edge has no reason to differ.
RUN adduser -D -H -u 10001 edge
COPY ${BINARY} /usr/local/bin/edge
RUN chmod 0555 /usr/local/bin/edge

# 8443 is the device-facing TLS listener the compose file publishes; 8081 is the plain-HTTP health
# listener a container healthcheck uses (no client certificate required to ask "are you alive").
EXPOSE 8443 8081
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/edge"]
