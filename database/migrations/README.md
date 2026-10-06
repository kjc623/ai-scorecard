# Migrations

`../schema.sql` is schema version 1 and always describes the full current schema: an empty database
is built from it alone.

Every change to the schema is made in two places: in `schema.sql`, so a new database gets it, and as
a numbered file here, so an existing database gets it. Files are named `NNNN-name.sql` — a
four-digit version from `0002` upwards with no gaps, then a lower-case name with words joined by
`-` (for example `0002-add-content-index.sql`). The migrate job applies each pending file in its own
transaction and records it in `public.schema_migration`; an applied file is never edited.
