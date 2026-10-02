# syntax=docker/dockerfile:1
#
# content-vault — the only component that can unwrap a content key (docs/06 §5.3, D7).
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#     docker build -f services/content-vault/Dockerfile .
# for the same reason as ingest-api: the Go module graph is expressed relative to it.
#
# See services/ingest-api/Dockerfile for the three-stage rationale. In short: `lab` packages a
# host-compiled binary and needs no toolchain image, which is what keeps the offline lab runnable;
# `production` is distroless and non-root; `build` is the networked path.
#
# Two things are specific to this service:
#   * the binary is subcommand-shaped — `content-vault serve` — so the image supplies the subcommand
#     in CMD rather than in ENTRYPOINT, and a deployment can override it (`version`, `schema-sql`);
#   * it has internal ingress only, and it refuses a non-loopback bind unless that is acknowledged.
#     The image sets SAC_INTERNAL_ONLY=true, which is the deployment's own statement of the fact the
#     --allow-non-loopback flag asks an operator to assert. The acknowledgement is not a control: the
#     network lock is (docs/02 §12, and the service README says the same).

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

COPY services/content-vault/ ./services/content-vault/
COPY contracts/ ./contracts/

WORKDIR /src/services/content-vault
RUN go build -trimpath -ldflags="-s -w" -o /out/content-vault ./cmd/content-vault

# =================================================================================================
# Stage 2 — the lab image: a host-compiled binary in a minimal, non-root image
# =================================================================================================
FROM ${LAB_BASE} AS lab

ARG BINARY=services/content-vault/bin/content-vault
ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=memory \
    SAC_KEY_BACKEND=local \
    SAC_ROLE=content-vault \
    SAC_INTERNAL_ONLY=true

RUN adduser -D -H -u 10001 sac
COPY ${BINARY} /usr/local/bin/content-vault
RUN chmod 0555 /usr/local/bin/content-vault

EXPOSE 8080
USER 10001:10001
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/content-vault"]
CMD ["serve"]

# =================================================================================================
# Stage 3 — production: distroless, non-root, no shell
# =================================================================================================
FROM ${RUNTIME_BASE} AS production

ENV SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_STORE=memory \
    SAC_KEY_BACKEND=local \
    SAC_ROLE=content-vault \
    SAC_INTERNAL_ONLY=true

COPY --from=build /out/content-vault /usr/local/bin/content-vault

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/content-vault"]
CMD ["serve"]
