# edge: the device entry point. Build context: the repository root.
#
#   docker build -f services/edge/Dockerfile -t edge .
#
# Configuration is the EDGE_* variables; see services/edge/README.md.

FROM golang:1.27-alpine AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY services/edge/ ./
RUN go build -trimpath -ldflags="-s -w" -o /out/edge .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/edge /usr/local/bin/edge
EXPOSE 8443
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/edge"]
