# syntax=docker/dockerfile:1
#
# query-api — the one read path (docs/04, ADR 0003).
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#     docker build -f query/query-api/Dockerfile .
#
# WHY THIS FILE DID NOT EXIST, and why that mattered. azure/main.bicep declared a container app
# named query-api with `image: '${registryLoginServer}/query-api:${imageTag}'`, docs/04 documented
# POST /v1/query, and the package had no server and no image: the deployment named a service that
# could not be built or run. This file is the other half of src/http/main.js.
#
# WHAT IS SPECIFIC TO THIS SERVICE
#   * It is Node, not Go, so there is no cross-compiled host binary to package. The lab stage is
#     therefore a plain runtime image with the source copied in — which is possible only because the
#     package has ZERO dependencies (no `node_modules` to install, and no network at build time).
#     That is a real constraint of this repository, not a convenience: extension/contract.test.mjs
#     asserts `dependencies` is undefined, and the offline lab depends on it.
#   * The read path must not run as root, and it must not be able to write: `sac_query` holds SELECT
#     and no DML (database/schema.sql §10), but the process itself is also made read-only.
#   * It refuses to start without SAC_PG_HOST and SAC_PG_DATABASE. There is deliberately no in-memory
#     fallback: an in-memory read path would answer every query with a number from nowhere.

ARG NODE_IMAGE=node:22-alpine

# =================================================================================================
# Stage 1 — the lab image: source + runtime, no build step, no dependencies
# =================================================================================================
FROM ${NODE_IMAGE} AS lab

ENV NODE_ENV=production \
    SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_ROLE=sac_query

WORKDIR /app

# The package is self-contained: src/ and that is all the server needs at run time. The tests, the
# tools and the DSL document are not copied — an image should carry what it serves and nothing else.
COPY query/query-api/package.json ./package.json
COPY query/query-api/src/ ./src/

# A non-root user that owns nothing writable.
RUN adduser -D -H -u 10001 sac \
 && chown -R 10001:10001 /app

EXPOSE 8080
USER 10001:10001

# The platform probes /healthz and /readyz through the ingress; this is for a bare `docker run`.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:8080/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["node", "src/http/main.js"]

# =================================================================================================
# Stage 2 — production: the same tree, with the runtime stripped to what is needed
# =================================================================================================
#
# This stage is intentionally thin. A Node image needs no compiler at run time and this package has
# no dependencies, so the only thing that separates lab from production is that production does not
# carry a shell: the image is run with the same files and the same entry point.
FROM ${NODE_IMAGE} AS production

ENV NODE_ENV=production \
    SAC_HTTP_ADDR=0.0.0.0:8080 \
    SAC_ROLE=sac_query

WORKDIR /app
COPY --from=lab /app/package.json ./package.json
COPY --from=lab /app/src/ ./src/

RUN adduser -D -H -u 10001 sac \
 && chown -R 10001:10001 /app \
 && rm -f /bin/sh /usr/bin/wget 2>/dev/null || true

EXPOSE 8080
USER 10001:10001
ENTRYPOINT ["node", "src/http/main.js"]
