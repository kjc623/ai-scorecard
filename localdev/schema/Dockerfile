# The one-shot schema applier, as an image rather than a bind mount.
#
# Why an image: some Docker daemons replace a single-file bind with an empty directory (and this
# environment's daemon did exactly that, so psql read /schema/schema.sql and reported "Is a
# directory"). A build-time COPY streams the real file through the build context, which works on
# every daemon. The schema applied is still the repository's own database/schema.sql, unmodified —
# it is copied, never forked.
FROM postgres:17-alpine

COPY database/schema.sql /schema/schema.sql
COPY localdev/schema/apply.sh /schema/apply.sh

# The connection is supplied by the compose service's environment (PGHOST/PGUSER/PGDATABASE/...),
# exactly as when the script was an inline entrypoint.
ENTRYPOINT ["sh", "/schema/apply.sh"]
