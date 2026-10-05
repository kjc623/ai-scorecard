-- In-place migration: directory display name on ops.user_dim (backlog/06-directory-sync).
-- Applies the one schema change this task makes to an existing database without touching any data.
-- Additive and idempotent; re-running is safe.
--
-- An existing database is brought to this state by running this file as a role that can alter
-- objects (the lab's `postgres`), inside the `shadow` database:
--
--   docker exec -i sac-authlab-postgres-1 psql -U postgres -d shadow -v ON_ERROR_STOP=1 \
--     < backlog/06-directory-sync/MIGRATION.sql
--
-- Like 02/03/04/05, this is a schema change shared by both tenants; it adds one nullable column and
-- never touches a tenant's rows. A new database gets the column from database/schema.sql directly.
BEGIN;

-- The directory's display name for a user_ref. Nullable: a tenant that has not synced, a provider
-- that returns no display name, and a 'hashed' tenant (which stores no clear name) all leave it
-- NULL, and the read falls back to the account name the device reports, then to user_ref.
ALTER TABLE ops.user_dim ADD COLUMN IF NOT EXISTS display_name text;

COMMENT ON COLUMN ops.user_dim.display_name IS
  'The directory''s display name for the user_ref, shown beside the account name the device reports. It is not the mapping to a real person: that is directory_object_id_enc, which is encrypted and never exported. Written only while the tenant''s device_identity is ''clear''.';

COMMIT;
