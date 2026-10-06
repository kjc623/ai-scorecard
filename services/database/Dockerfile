# The `migrate` image: applies services/database/schema.sql and the numbered migrations, then grants each
# component login its role. Run once per deployment, before the services start.
#
#   docker build -f services/database/Dockerfile -t migrate .      (build context: the repository root)
#
# Configuration is the SAC_PG_* variables and SAC_DB_LOGINS; see services/database/README.md.

ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY services/platform/go.mod services/platform/go.sum services/platform/
COPY services/database/go.mod services/database/go.sum services/database/
RUN cd services/database && go mod download
COPY services/platform/ services/platform/
COPY services/database/ services/database/
RUN cd services/database && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM ${RUNTIME_BASE}
COPY --from=build /out/migrate /migrate
USER nonroot:nonroot
ENTRYPOINT ["/migrate"]
