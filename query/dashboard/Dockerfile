# syntax=docker/dockerfile:1
#
# dashboard — the static analyst pages (index.html, explore.html), served by tools/serve.mjs.
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#     docker build -f query/dashboard/Dockerfile -t sac-dashboard .
#     docker run --rm -p 127.0.0.1:8787:8787 sac-dashboard
#
# This is a preview image. Both pages run on their stub transports, so it needs no query-api, no
# database and no session. The package has zero dependencies, so there is nothing to install.

ARG NODE_IMAGE=node:22-alpine
FROM ${NODE_IMAGE}

ENV NODE_ENV=production
WORKDIR /app

COPY query/dashboard/package.json ./package.json
COPY query/dashboard/index.html query/dashboard/module.html query/dashboard/explore.html ./
COPY query/dashboard/styles.css query/dashboard/explore.css ./
COPY query/dashboard/src/ ./src/
COPY query/dashboard/tools/serve.mjs ./tools/serve.mjs

RUN adduser -D -H -u 10001 sac \
 && chown -R 10001:10001 /app

EXPOSE 8787
USER 10001:10001

HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:8787/explore.html').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["node", "tools/serve.mjs", "--host", "0.0.0.0"]
