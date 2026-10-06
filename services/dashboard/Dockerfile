# syntax=docker/dockerfile:1
#
# dashboard — the analyst pages and the server that signs people in through control-api and
# forwards their reads. The package has no dependencies, so nothing is installed.
#
# Build context is the repository root:
#     docker build -f services/dashboard/Dockerfile -t dashboard .

FROM node:22-alpine

ENV NODE_ENV=production
WORKDIR /app

COPY services/dashboard/package.json ./
COPY services/dashboard/index.html services/dashboard/explore.html ./
COPY services/dashboard/styles.css services/dashboard/explore.css services/dashboard/signin.css ./
COPY services/dashboard/src/ ./src/
COPY services/dashboard/server/ ./server/

USER node
EXPOSE 8080
CMD ["node", "server/main.mjs"]
