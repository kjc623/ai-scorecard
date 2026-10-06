# The lab's device-side tool (key material and the smoke). Build context: the repository root.
#
#   docker build -f localdev/authlab/Dockerfile -t sac/authlab:lab .

FROM golang:1.27-alpine AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY device/protocol/ device/protocol/
COPY localdev/authlab/ localdev/authlab/
RUN cd localdev/authlab && go build -trimpath -ldflags="-s -w" -o /out/authlab .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/authlab /usr/local/bin/authlab
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/authlab"]
