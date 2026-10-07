# 04. Server: store the new kinds

## Problem

Task 03 changed the envelope contract. ingest-api validates records against the embedded schema,
but the database still allows only the old kinds and has no columns for the new fields. The
schema checks (`services/database/tools/check-schema.mjs`) compare the database's kind and mode
boundaries with the contract, so they fail until the database follows.

## Goal

ingest-api accepts and stores `discovery` and `agent_activity` records, and prompts on the new
routes. The database enforces the same per-kind rules as the contract, and every gate passes again.

## Scope

- **`services/database/schema.sql`**, and the same change as the next free numbered migration in
  `services/database/migrations/` (README there), because pre-prod exists:
  - `ingest.observation`:
    - Add the §3 fields as columns. Use `model_names text[]`, and integers for the token and
      duration fields.
    - Replace the `model_detection` kind value and CHECK with `discovery`, add `agent_activity`,
      and replace the `detection_basis` CHECK values.
    - Add shape CHECKs that mirror the contract's `allOf` branches:
      `observation_discovery_shape`, `observation_activity_shape`, and the new-field
      prohibitions on `observation_prompt_shape` and `observation_rollup_shape`.
  - `ingest.submission.kind`: the same kind change.
  - `ingest.record_event`: copy the new fields from the envelope into the observation.
  - `ref.route_fidelity`: seed rows for `tool.hook` (5), `tool.otel` (15), `inv.scan` (75) and
    `net.flow` (80). `yields_content` is true for the two `tool.*` routes and false for the other
    two.
  - `ops.tool_display_name` and the grants need no change. Check this, and say so in the report.
  - `services/database/invariants.test.sql`: cases for each new kind, one accepted and one
    refused per shape CHECK.
  - The migration:
    - replaces the CHECKs in place on the existing tables, drops and recreates
      `ingest.record_event`, and inserts the `ref.route_fidelity` rows;
    - must turn a database built from `main`'s `schema.sql` into the same schema the new
      `schema.sql` builds.
    - Prove this in a throwaway PostgreSQL 16 container. Build one database from
      `git show main:services/database/schema.sql` plus the new migration, and one from the new
      `schema.sql`. Diff `pg_dump --schema-only` of both; the diff must be empty, ignoring the
      `public.schema_migration` rows. Later briefs refer to this as "the migration proof".
    - Pre-prod has no `model_detection` rows (nothing emits it), so no data conversion is needed.
      Assert that in the migration with a guard that fails if any such row exists.
- **`services/database/tools/check-schema.mjs`**: replace `model_detection` with the new kinds in
  the kind and shape agreement (around line 451), so the check compares the new contract with the
  new CHECKs.
- **ingest-api** (`services/ingest-api/internal/ingest/service.go`):
  - `dedupTier`: give `discovery` and `agent_activity` their own tiers, `V` and `A`, in the style
    of the existing ones. Update the response vocabulary only if the tier is part of the response.
  - Update `internal/contract/contract_test.go` so the fixtures use the new kinds instead of
    `model_detection`.
- **Readers of `model_detection`**:
  - `services/jobs/internal/rollup/rollup.go` (~157): the `detections` count becomes
    `s.kind = 'discovery'`.
  - `services/query-api/src/registry.js` (~745, ~844): the kind dimension values.
  - The dashboard test fixtures `services/dashboard/test/fixtures.mjs` and `explore-fake.mjs`:
    rename only; no new page.
- **Tests**: add one ingest-api store test (live database, only when `SAC_TEST_PG_DSN` is set,
  random tenant, as the existing ones do) that writes one record of each new kind and reads the
  columns back.

## Done when

- `node services/database/tools/check-schema.mjs` passes.
- The database gate passes: `node tools/accept.mjs --only database` (PostgreSQL in Docker).
- The migration proof shows an empty diff.
- `node tools/accept.mjs` passes as a whole.
- The report shows a `discovery` record and an `agent_activity` record posted to a local
  ingest-api test server and stored. Use the existing store test harness (`SAC_TEST_PG_DSN`).
- This task has no device check. After the owner merges, the deploy workflow's `migrate` step
  applies the migration in pre-prod. Ask the owner to confirm the deploy run succeeded, then show
  from the run's log (`gh run view <run id> --log`, read-only) that `migrate` applied the new
  migration.
