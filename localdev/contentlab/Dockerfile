# syntax=docker/dockerfile:1
#
# contentlab — the local stand-in for ciphertext storage and for the analyst's retrieval surface
# (docs/02-ingest-and-transport.md §10, §11). See main.go for what is real and what is a stand-in.
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#     docker build --target lab -t sac/contentlab:lab -f localdev/contentlab/Dockerfile .
#
# One stage, like the edge: a standard-library-only program that localdev/build.mjs cross-compiles on
# the host, so the image needs no golang image and no network.
ARG LAB_BASE=alpine:latest
FROM ${LAB_BASE} AS lab

ARG BINARY=localdev/contentlab/bin/contentlab

RUN adduser -D -H -u 10001 sac && mkdir -p /data && chown 10001:10001 /data
COPY ${BINARY} /usr/local/bin/contentlab
RUN chmod 0555 /usr/local/bin/contentlab

EXPOSE 8080
USER 10001:10001
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/contentlab"]
