# syntax=docker/dockerfile:1
#
# ingest-api. Build context is the repository root, because the module replaces three sibling
# modules with local paths:
#
#   docker build -f ingestion/ingest-api/Dockerfile -t ingest-api .

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

COPY ingestion/ingest-api/go.mod ingestion/ingest-api/go.sum ingestion/ingest-api/
COPY endpoint/protocol/go.mod endpoint/protocol/
COPY contracts/generated/go/go.mod contracts/generated/go/
COPY platform/go.mod platform/go.sum platform/
RUN cd ingestion/ingest-api && go mod download

COPY endpoint/protocol/ endpoint/protocol/
COPY contracts/generated/go/ contracts/generated/go/
COPY platform/ platform/
COPY ingestion/ingest-api/ ingestion/ingest-api/
RUN cd ingestion/ingest-api && go build -trimpath -ldflags="-s -w" -o /out/ingest-api ./cmd/ingest-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ingest-api /ingest-api
ENV SAC_HTTP_ADDR=0.0.0.0:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/ingest-api"]
