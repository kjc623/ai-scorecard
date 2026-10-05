# syntax=docker/dockerfile:1
#
# oidc — the local OpenID Connect stand-in (see main.go for what is real and what is a
# fixture). It is a standard-library-only program with no module dependency and no runtime
# data file, so localdev/build.mjs cross-compiles it on the host (GOPROXY=off) and this image
# never needs a golang image or a network — the same shape as the edge and contentlab images.
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#     docker build --target lab -t sac/oidc:lab -f localdev/oidc/Dockerfile .
ARG LAB_BASE=alpine:latest
FROM ${LAB_BASE} AS lab

# Path to the binary produced by localdev/build.mjs, relative to the build context.
ARG BINARY=localdev/oidc/bin/oidc

RUN adduser -D -H -u 10001 oidc
COPY ${BINARY} /usr/local/bin/oidc
RUN chmod 0555 /usr/local/bin/oidc

EXPOSE 8080
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/oidc"]
