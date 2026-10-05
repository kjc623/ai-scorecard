# syntax=docker/dockerfile:1
#
# dashboard — the analyst pages (index.html, explore.html) and their server, tools/serve.mjs: the
# sign-in pages, the session cookie, and the forwarders to query-api and control-api.
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#     docker build -f query/dashboard/Dockerfile -t sac-dashboard .
#     docker run --rm -p 127.0.0.1:8787:8787 sac-dashboard
#
# With no environment it is a preview: the pages run on their sample transports and the server is
# the development principal. In a deployment it is given SAC_CONTROL_URL, SAC_INTERNAL_TOKEN,
# SAC_PUBLIC_URL and SAC_QUERY_API_URL (README, "Running the server"), and every page and read then
# needs a session from control-api. The package has zero dependencies, so there is nothing to install.

ARG NODE_IMAGE=node:22-alpine
FROM ${NODE_IMAGE}

ENV NODE_ENV=production
WORKDIR /app

COPY query/dashboard/package.json ./package.json
COPY query/dashboard/index.html query/dashboard/module.html query/dashboard/explore.html ./
COPY query/dashboard/styles.css query/dashboard/explore.css query/dashboard/signin.css ./
COPY query/dashboard/src/ ./src/
COPY query/dashboard/tools/serve.mjs query/dashboard/tools/session.mjs query/dashboard/tools/signin-page.mjs ./tools/

RUN adduser -D -H -u 10001 sac \
 && chown -R 10001:10001 /app

EXPOSE 8787
USER 10001:10001

HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:8787/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["node", "tools/serve.mjs", "--host", "0.0.0.0"]
