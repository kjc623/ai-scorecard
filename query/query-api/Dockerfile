# syntax=docker/dockerfile:1
#
# query-api. Build context is the repository root:
#     docker build -f query/query-api/Dockerfile -t query-api .

FROM node:22-alpine

ENV NODE_ENV=production \
    SAC_HTTP_ADDR=0.0.0.0:8080

WORKDIR /app

# The locked production dependencies first, so this layer is cached until the lockfile changes.
COPY query/query-api/package.json query/query-api/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
 && npm cache clean --force

COPY query/query-api/src/ ./src/

USER node
EXPOSE 8080
CMD ["node", "src/http/main.js"]
