# The lab's identity provider. Build context: the repository root.
#
#   docker build -f localdev/oidc/Dockerfile -t sac/oidc:lab .

FROM golang:1.27-alpine AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY localdev/oidc/ ./
RUN go build -trimpath -ldflags="-s -w" -o /out/oidc .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/oidc /usr/local/bin/oidc
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/oidc"]
