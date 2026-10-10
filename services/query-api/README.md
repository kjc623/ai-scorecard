# query-api

The dashboard's only read path. A closed query DSL ([`DSL.md`](DSL.md)) is validated, cost-guarded
and compiled server-side to parameterised SQL over `mart` and `ingest.submission`; every value is
bound, and a name outside the closed vocabulary is rejected rather than escaped. Each read runs in
one transaction scoped to the caller's tenant (`set_config('app.tenant_id', …, true)`, enforced by
row-level security), writes its audit row before anything is served, and returns one envelope that
carries the data with its freshness, coverage and suppression state. It also accepts two audited
writes (finding review, tool sanction) and forwards the two content requests to `content-vault`,
which decides everything about content.

| Route | |
|---|---|
| `POST /v1/query` | a template or a DSL document (`DSL.md`) |
| `POST /v1/finding-review`, `POST /v1/tool-sanction` | audited configuration writes |
| `POST /v1/content-search`, `POST /v1/content/retrieval` | forwarded to content-vault with the caller's token |
| `POST /v1/list-export` | the current filtered events or findings list as a bounded CSV, stored server-side and returned as a single-use, short-lived download link (role `analyst`, `content_reader` or `admin`); audited with the filters |
| `POST /v1/subject-export`, `POST /v1/subject-erasure` | an admin's data-subject export (an archive of one person's events, findings and stored prompts, the prompts decrypted by content-vault) and a request to erase one person's data (performed by the `jobs erase` job, which writes the receipt) |
| `GET /v1/export/{id}` | one single-use, short-lived download of a generated export |
| `GET /healthz`, `GET /readyz` | liveness; readiness (one database round trip) |

Every route but the probes needs `Authorization: Bearer <product access token>` from control-api;
the tenant, actor and roles come from its claims. Node 22, dependencies `pg` and `jose`.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `SAC_PG_HOST`, `SAC_PG_DATABASE` | yes | PostgreSQL server and database |
| `SAC_PG_USER` | yes | database login, `query-api` (granted `sac_query`) |
| `SAC_PG_PORT` | no | default `5432` |
| `SAC_PG_SSLMODE` | no | `require` (default) or `verify-full` verify the server certificate; `disable` for a local database |
| `SAC_PG_PASSWORD` | no | when unset, each connection authenticates with a Microsoft Entra token for the managed identity (`IDENTITY_ENDPOINT`, `IDENTITY_HEADER`, `AZURE_CLIENT_ID`, set by Container Apps), cached until five minutes before expiry |
| `SAC_AUTH_ISSUER` | yes | control-api's token issuer (`iss`) |
| `SAC_AUTH_AUDIENCE` | no | default `sac-query` |
| `SAC_AUTH_JWKS_URL` | no | default `{SAC_AUTH_ISSUER}/.well-known/jwks.json` |
| `SAC_CONTENT_VAULT_URL` | yes | content-vault's internal URL |
| `SAC_CURSOR_KEY` | yes | HMAC key for list cursors, ≥ 32 characters (Key Vault secret `sac-cursor-key`); every replica must share it |
| `SAC_HTTP_ADDR` | no | listen address, default `0.0.0.0:8080` |

A missing or malformed value is a refusal to start. Logs are JSON lines on stdout.

## Build and test

```
docker build -f services/query-api/Dockerfile -t query-api .   # from the repository root
npm ci && npm test                                         # in services/query-api
npm run snapshot:check                                     # compiled SQL against test/snapshots
```

`npm test` runs without a database. `test/db.test.mjs` also runs every compiled statement, the read
path and row-level security against a real PostgreSQL when `SAC_TEST_PG_DSN` names one
(`postgres://…`, a role that may `SET ROLE sac_query`, `services/database/schema.sql` applied); without it
those tests skip. A compiler change that alters SQL is regenerated with `npm run snapshot`.
