# The lab's scheduler for the jobs image. In Azure, Container Apps runs `jobs aggregate` and
# `jobs expire` as scheduled one-shot jobs; the lab runs the same binary in a loop instead.
# Build context: the repository root, after the jobs image:
#
#   docker build -f localdev/jobs/Dockerfile -t sac/lab-jobs:lab .

ARG JOBS_IMAGE=sac/jobs:lab
FROM ${JOBS_IMAGE} AS jobs

FROM alpine:3.22
COPY --from=jobs /usr/local/bin/jobs /usr/local/bin/jobs
COPY localdev/jobs/schedule.sh /usr/local/bin/schedule
USER 65532:65532
ENTRYPOINT ["/bin/sh", "/usr/local/bin/schedule"]
