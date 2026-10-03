# syntax=docker/dockerfile:1
#
# control-api — the device-facing control plane (docs/02-ingest-and-transport.md §5.1, §5.2;
# ADR 0020 decisions 3 and 4).
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, not this directory:
#     docker build -f control/control-api/Dockerfile .
# for the same reason as ingest-api: the Go module replaces github.com/shadow-ai-capture/device/protocol
# with a path relative to the module (../../endpoint/protocol), so the image needs that tree.
#
# THREE STAGES, two audiences:
#   * `build`      — compiles from source. Needs a Go toolchain image, so a host with no network and
#                    no cached golang image cannot run it. This is the production path.
#   * `lab`        — packages a binary compiled on the HOST (localdev/build.mjs does that). Depends on
#                    no toolchain image, which is what keeps the $0.00 lab runnable offline.
#   * `production` — distroless, non-root, no shell. The default target of `docker build .`.
#
# The image sets only non-secret defaults. The access-token signing key (SAC_DPOP_TOKEN_KEY_PEM) and
# the LocalCA material (SAC_CA_CERT_PEM/SAC_CA_KEY_PEM) are secrets and are injected at run time by
# the deployment; the process refuses to start without the signing key rather than issuing tokens
# under a key nobody configured.

ARG GO_IMAGE=golang:1.27-alpine
ARG LAB_BASE=alpine:latest
ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot

# =================================================================================================
# Stage 1 — compile
# =================================================================================================
FROM ${GO_IMAGE} AS build

ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GOPROXY=off \
    GOTOOLCHAIN=local
WORKDIR /src

# Only what the module needs: the service and its one replacement target.
COPY control/control-api/ ./control/control-api/
COPY endpoint/protocol/ ./endpoint/protocol/

WORKDIR /src/control/control-api
RUN go build -trimpath -ldflags="-s -w" -o /out/control-api ./cmd/control-api \
 && go vet ./...

# =================================================================================================
# Stage 2 — the lab image: a host-compiled binary in a minimal, non-root image
# =================================================================================================
FROM ${LAB_BASE} AS lab

# Path to the binary produced by localdev/build.mjs, relative to the build context (repository root).
ARG BINARY=control/control-api/bin/control-api
# The non-secret deployment vocabulary. Every name here is read by the binary; the agreement is
# asserted by cmd/control-api/infra_agreement_test.go.
ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=memory \
    SAC_ROLE=control-api \
    SAC_CREDENTIAL_TTL=2160h \
    SAC_ENROLMENT_TOKEN_TTL=24h

RUN adduser -D -H -u 10001 sac
COPY ${BINARY} /usr/local/bin/control-api
RUN chmod 0555 /usr/local/bin/control-api

EXPOSE 8080
USER 10001:10001
# The platform probes /healthz and /readyz (azure/modules/container-app.bicep). This HEALTHCHECK is
# for docker compose and for a person running the lab, not for Container Apps, which uses its own.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/control-api"]

# =================================================================================================
# Stage 3 — production: distroless, non-root, no shell and no package manager
# =================================================================================================
FROM ${RUNTIME_BASE} AS production

ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=memory \
    SAC_ROLE=control-api \
    SAC_CREDENTIAL_TTL=2160h \
    SAC_ENROLMENT_TOKEN_TTL=24h

COPY --from=build /out/control-api /usr/local/bin/control-api

EXPOSE 8080
USER nonroot:nonroot
# No HEALTHCHECK: distroless has no shell to run one in. The platform's own probes are the health
# surface (liveness /healthz, readiness /readyz).
ENTRYPOINT ["/usr/local/bin/control-api"]
