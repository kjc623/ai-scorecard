# syntax=docker/dockerfile:1
#
# jobs: the scheduled database jobs. The deployment passes the subcommand as the container's
# arguments (`aggregate` or `expire`).
#
# Build context is the repository root, because the module uses the shared platform module
# through `replace github.com/shadow-ai-capture/platform => ../platform`:
#
#   docker build -f services/jobs/Dockerfile -t jobs .

ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
ENV CGO_ENABLED=0
WORKDIR /src

# Module files first, so the download layer is reused until a dependency changes.
COPY services/platform/go.mod services/platform/go.sum ./services/platform/
COPY services/jobs/go.mod services/jobs/go.sum ./services/jobs/
WORKDIR /src/services/jobs
RUN go mod download

COPY services/platform/ /src/services/platform/
COPY services/jobs/ /src/services/jobs/
RUN go build -trimpath -ldflags="-s -w" -o /out/jobs ./cmd/jobs

FROM ${RUNTIME_BASE}
COPY --from=build /out/jobs /usr/local/bin/jobs
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/jobs"]
