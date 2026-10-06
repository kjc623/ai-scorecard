# The lab's device ingress. Build context: the repository root.
#
#   docker build -f localdev/edge/Dockerfile -t sac/edge:lab .

FROM golang:1.27-alpine AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY localdev/edge/ ./
RUN go build -trimpath -ldflags="-s -w" -o /out/edge .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/edge /usr/local/bin/edge
EXPOSE 8443
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/edge"]
