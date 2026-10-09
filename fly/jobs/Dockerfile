# syntax=docker/dockerfile:1
#
# The jobs app's image: the jobs binary, scheduled by supercronic over fly/jobs/crontab. Build
# context: the repository root, after the jobs image:
#
#   docker build -f services/jobs/Dockerfile -t jobs .
#   docker build -f fly/jobs/Dockerfile --build-arg JOBS_IMAGE=jobs -t jobs-cron .

ARG JOBS_IMAGE=jobs
FROM ${JOBS_IMAGE} AS jobs

# supercronic runs each crontab line with /bin/sh, so the image has a shell.
FROM alpine:3.22
ADD --checksum=sha256:a53ae236602c7338aba3fbaff40bda6300eae3b9fedb8261eb06cfe3724430c1 --chmod=0755 \
    https://github.com/aptible/supercronic/releases/download/v0.2.49/supercronic-linux-amd64 /usr/local/bin/supercronic
COPY --from=jobs /usr/local/bin/jobs /usr/local/bin/jobs
COPY fly/jobs/crontab /etc/supercronic/crontab
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/supercronic", "-json", "-passthrough-logs", "/etc/supercronic/crontab"]
