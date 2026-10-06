# The `migrate` image: applies database/schema.sql and the numbered migrations, then grants each
# component login its role. Run once per deployment, before the services start.
#
#   docker build -f database/Dockerfile -t migrate .      (build context: the repository root)
#
# Configuration is the SAC_PG_* variables and SAC_DB_LOGINS; see database/README.md.

ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY platform/go.mod platform/go.sum platform/
COPY database/go.mod database/go.sum database/
RUN cd database && go mod download
COPY platform/ platform/
COPY database/ database/
RUN cd database && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM ${RUNTIME_BASE}
COPY --from=build /out/migrate /migrate
USER nonroot:nonroot
ENTRYPOINT ["/migrate"]
