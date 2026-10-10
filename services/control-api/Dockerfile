# syntax=docker/dockerfile:1
#
# control-api. The build context is the repository root, because the module replaces
# device/protocol and platform with their paths in the repository:
#
#   docker build -f services/control-api/Dockerfile \
#     --build-context agent-release=<dir with release.json, agent-release.json, ShadowAICapture.msi, shadow-ai-capture.crx> \
#     -t control-api .
#
# The agent-release context is copied to /opt/sac/agent-release, where the tenant package and the
# browser extension routes read it. Without it the image carries no release and those routes answer
# that the release is unavailable.

ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY device/protocol/ device/protocol/
COPY services/platform/ services/platform/
COPY services/control-api/go.mod services/control-api/go.sum services/control-api/
WORKDIR /src/services/control-api
RUN go mod download
COPY services/control-api/ ./
RUN go build -trimpath -ldflags="-s -w" -o /out/control-api ./cmd/control-api

FROM scratch AS agent-release

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/control-api /usr/local/bin/control-api
COPY --from=agent-release . /opt/sac/agent-release/
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/control-api"]
