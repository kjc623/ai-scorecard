# syntax=docker/dockerfile:1
#
# content-vault. Build from the repository root:
#     docker build -f vault/content-vault/Dockerfile -t content-vault .

FROM golang:1.27-alpine AS build
ENV CGO_ENABLED=0
WORKDIR /src
# The module replaces two sibling modules by relative path; their go.mod files are copied first so
# the dependency download is cached until a go.mod or go.sum changes.
COPY platform/go.mod platform/go.sum ./platform/
COPY endpoint/protocol/go.mod ./endpoint/protocol/
COPY vault/content-vault/go.mod vault/content-vault/go.sum ./vault/content-vault/
RUN cd vault/content-vault && go mod download
COPY platform/ ./platform/
COPY endpoint/protocol/ ./endpoint/protocol/
COPY vault/content-vault/ ./vault/content-vault/
RUN cd vault/content-vault && go build -trimpath -ldflags="-s -w" -o /out/content-vault ./cmd/content-vault

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/content-vault /usr/local/bin/content-vault
ENV SAC_HTTP_ADDR=0.0.0.0:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/content-vault"]
