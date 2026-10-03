# The development PKI, baked into an image so it can be delivered to the lab through a named volume.
#
# Why: the lab must not depend on a host-path bind mount. Some Docker daemons (including this
# environment's) present a bind of a path the daemon cannot see as an empty directory, so the
# services reported `open /authlab/dev-ca.crt: no such file or directory`. A build-time COPY streams
# the generated material through the build context, and run.mjs copies it into a named volume the
# services mount. No secret is committed: localdev/.authlab is generated fresh by `authlab pki`.
FROM alpine:latest

COPY localdev/.authlab/ /pki/

# The services run as a non-root user (uid 10001) and read these through the volume, so the
# generated material must be readable. It is lab-only and regenerated every run.
RUN chmod -R a+rX /pki

ENTRYPOINT ["sh"]
