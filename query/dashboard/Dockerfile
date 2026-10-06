# syntax=docker/dockerfile:1
#
# dashboard — the analyst pages and the server that signs people in through control-api and
# forwards their reads. The package has no dependencies, so nothing is installed.
#
# Build context is the repository root:
#     docker build -f query/dashboard/Dockerfile -t dashboard .

FROM node:22-alpine

ENV NODE_ENV=production
WORKDIR /app

COPY query/dashboard/package.json ./
COPY query/dashboard/index.html query/dashboard/explore.html ./
COPY query/dashboard/styles.css query/dashboard/explore.css query/dashboard/signin.css ./
COPY query/dashboard/src/ ./src/
COPY query/dashboard/server/ ./server/

USER node
EXPOSE 8080
CMD ["node", "server/main.mjs"]
