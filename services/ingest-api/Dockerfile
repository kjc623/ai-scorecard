# syntax=docker/dockerfile:1
#
# ingest-api. Build context is the repository root, because the module replaces three sibling
# modules with local paths:
#
#   docker build -f services/ingest-api/Dockerfile -t ingest-api .

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

COPY services/ingest-api/go.mod services/ingest-api/go.sum services/ingest-api/
COPY endpoint/protocol/go.mod endpoint/protocol/
COPY contracts/generated/go/go.mod contracts/generated/go/
COPY services/platform/go.mod services/platform/go.sum services/platform/
RUN cd services/ingest-api && go mod download

COPY endpoint/protocol/ endpoint/protocol/
COPY contracts/generated/go/ contracts/generated/go/
COPY services/platform/ services/platform/
COPY services/ingest-api/ services/ingest-api/
RUN cd services/ingest-api && go build -trimpath -ldflags="-s -w" -o /out/ingest-api ./cmd/ingest-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ingest-api /ingest-api
ENV SAC_HTTP_ADDR=0.0.0.0:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/ingest-api"]
