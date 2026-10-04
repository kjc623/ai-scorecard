# syntax=docker/dockerfile:1
#
# aggregator — the scheduled mart rollup job (docs/03 §5, docs/04 §4).
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, for the same reason as the other services: the Go module
# graph is expressed relative to it. In short, the `lab` stage packages a host-compiled binary and
# needs no toolchain image (which is what keeps the offline lab runnable); `production` is
# distroless and non-root; `build` is the networked path.
#
# The binary is a long-running job: it serves only /healthz and /readyz and writes nothing but the
# mart aggregates and their watermarks.

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

COPY aggregation/aggregator/ ./aggregation/aggregator/

WORKDIR /src/aggregation/aggregator
# Untagged, like every other service's `build` stage: the PostgreSQL driver is compiled only under
# the sac_sql_driver tag, which the host build (localdev/build.mjs) applies before packaging the
# binary in the `lab` stage. The tag is deliberately absent here so this stage needs no network and
# no module cache, which is what lets the offline lab build.
RUN go build -trimpath -ldflags="-s -w" -o /out/aggregator ./cmd/aggregator

# =================================================================================================
# Stage 2 — the lab image: a host-compiled binary in a minimal, non-root image
# =================================================================================================
FROM ${LAB_BASE} AS lab

ARG BINARY=aggregation/aggregator/bin/aggregator
ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=sql

RUN adduser -D -H -u 10001 sac
COPY ${BINARY} /usr/local/bin/aggregator
RUN chmod 0555 /usr/local/bin/aggregator

EXPOSE 8080
USER 10001:10001
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/aggregator"]

# =================================================================================================
# Stage 3 — production: distroless, non-root
# =================================================================================================
FROM ${RUNTIME_BASE} AS production

ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=sql

COPY --from=build /out/aggregator /usr/local/bin/aggregator

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/aggregator"]
