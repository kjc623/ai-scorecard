FROM golang:1.99-nonexistent AS build
RUN echo should-never-run

FROM alpine:latest AS lab
RUN echo lab-stage-built

FROM gcr.io/distroless/static-debian12:nonroot AS production
COPY --from=build /nonexistent /x
