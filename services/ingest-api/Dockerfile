# syntax=docker/dockerfile:1
#
# ingest-api — the validating write path (docs/00-architecture.md ADR 0001).
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, not this directory:
#     docker build -f services/ingest-api/Dockerfile .
# because the image needs two files that do not live under the service directory —
# contracts/event-envelope.schema.json (the contract it validates against) and
# services/ingest-api/testdata/route-fidelity.seed.json (ref.route_fidelity as data, §4.4) — and the
# Go module replaces github.com/shadow-ai-capture/device/protocol and the generated contract types
# with paths relative to the module.
#
# THREE STAGES, two audiences:
#   * `build`      — compiles from source. Needs a Go toolchain image, so a host with no network and
#                    no cached golang image cannot run it. This is the production path.
#   * `lab`        — packages a binary compiled on the HOST (lab/build.mjs does that). Depends on no
#                    toolchain image, which is what makes the $0.00 lab runnable offline:
#                        docker build --target lab -t sac/ingest-api:lab -f services/ingest-api/Dockerfile .
#   * `production` — distroless, non-root, no shell. The default target of `docker build .`.

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

# Only what the module needs: the service, its two replacement targets, and the contract.
COPY services/ingest-api/ ./services/ingest-api/
COPY device/protocol/ ./device/protocol/
COPY contracts/ ./contracts/

WORKDIR /src/services/ingest-api
RUN go build -trimpath -ldflags="-s -w" -o /out/ingest-api ./cmd/ingest-api \
 && go vet ./... \
 && /out/ingest-api -h >/dev/null 2>&1 || true

# =================================================================================================
# Stage 2 — the lab image: a host-compiled binary in a minimal, non-root image
# =================================================================================================
FROM ${LAB_BASE} AS lab

# Path to the binary produced by lab/build.mjs, relative to the build context (repository root).
ARG BINARY=services/ingest-api/bin/ingest-api
# The non-secret deployment vocabulary. Every name here is read by the binary and asserted against
# infra/main.bicep by lab/tools/check-config-agreement.mjs; the image-level ones (SAC_HTTP_ADDR,
# SAC_STORE, SAC_SCHEMA, SAC_ROUTES_FILE) are listed there as deliberate extensions, because a
# container platform passes environment and cannot mount a repository path.
ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=memory \
    SAC_ROLE=ingest-api \
    SAC_SCHEMA=/etc/sac/event-envelope.schema.json \
    SAC_ROUTES_FILE=/etc/sac/route-fidelity.seed.json

RUN adduser -D -H -u 10001 sac
COPY ${BINARY} /usr/local/bin/ingest-api
COPY contracts/event-envelope.schema.json /etc/sac/event-envelope.schema.json
COPY services/ingest-api/testdata/route-fidelity.seed.json /etc/sac/route-fidelity.seed.json
RUN chmod 0555 /usr/local/bin/ingest-api && chmod 0444 /etc/sac/*.json

EXPOSE 8080
USER 10001:10001
# The platform probes /healthz and /readyz (infra/modules/container-app.bicep). This HEALTHCHECK is
# for docker compose and for a person running the lab, not for Container Apps, which uses its own.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/ingest-api"]

# =================================================================================================
# Stage 3 — production: distroless, non-root, no shell and no package manager
# =================================================================================================
FROM ${RUNTIME_BASE} AS production

ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=memory \
    SAC_ROLE=ingest-api \
    SAC_SCHEMA=/etc/sac/event-envelope.schema.json \
    SAC_ROUTES_FILE=/etc/sac/route-fidelity.seed.json

COPY --from=build /out/ingest-api /usr/local/bin/ingest-api
COPY --from=build /src/contracts/event-envelope.schema.json /etc/sac/event-envelope.schema.json
COPY --from=build /src/services/ingest-api/testdata/route-fidelity.seed.json /etc/sac/route-fidelity.seed.json

EXPOSE 8080
USER nonroot:nonroot
# No HEALTHCHECK: distroless has no shell to run one in. The platform's own probes are the health
# surface (liveness /healthz, readiness /readyz), which is where they belong — a container that
# restarts itself on a failing probe would be a second, disagreeing liveness authority.
ENTRYPOINT ["/usr/local/bin/ingest-api"]
