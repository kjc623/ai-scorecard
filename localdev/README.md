# localdev — the $0.00 development lab (Part A of docs/lab/LAB-COST.md §1)

PostgreSQL with `database/schema.sql` applied, both Go services in containers, and a smoke test that proves
they serve. No Azure, no credentials, no cost.

This directory exists because **the services could not be run in a container at all** before it: no
Dockerfiles existed (F1), both binaries bound loopback on ports the deployment does not probe (F2), the
container app passed environment variables no binary read (F3), and neither service could open a
database connection (F4). F1–F3 are fixed here. F4's state is in [What is not fixed](#what-is-not-fixed).

```powershell
node localdev/build.mjs     # cross-compile both services and build the lab images
node localdev/run.mjs       # up + smoke test + report; leaves the lab running
node localdev/run.mjs --down
```

`run.mjs` prints every check and exits non-zero if any fails. On this host all thirteen pass:

```
  ok   ingest-api /healthz — 200
  ok   ingest-api /readyz — 200 {"status":"ready"}
  ok   content-vault /healthz — 200
  ok   content-vault /readyz — 200 {"status":"ready","key_backend":"local-software","store":"memory"}
  ok   database/schema.sql applied (ingest tables) — 4 tables
  ok   ref.route_fidelity seeded — 7 routes
  ok   200 with two accepted results — status 200, accepted 2
  ok   one logical submission for two observations — 1 submission_id(s)
  ok   dedup_tier recomputed at ingest — tiers T,T
  ok   409 duplicate_batch — 409 duplicate_batch
  ok   200 with a per-event rejection, not a batch failure — status 200
  ok   reason mode_violation with the offending pointer — mode_violation /events/0/content_digest
```

## What is in it

| Container | Image | Why |
|---|---|---|
| `postgres` | `postgres:17-alpine` | the real engine, with the repository's schema applied **unmodified** |
| `schema` | `postgres:17-alpine` | a one-shot `psql -f database/schema.sql`, idempotent (it checks for the `ingest` schema first), and everything else waits for it to exit 0 |
| `ingest-api` | `sac/ingest-api:lab` | `POST /v1/events`, `/healthz`, `/readyz` on `0.0.0.0:8080` |
| `content-vault` | `sac/content-vault:lab` | the internal vault surface, `/healthz`, `/readyz`; published on host `8081` |
| `query-api` | `sac/query-api:lab` | the one read path: `POST /v1/query`, `/healthz`, `/readyz`; published on host `8082` |

**`query-api` joined this file last, and it was not a one-line addition.** The service had no HTTP
server and no image: `azure/main.bicep` declared a container app called `query-api` with an image
reference, `docs/04-dashboard-and-query.md` documented `POST /v1/query`, and nothing listened. The
closed DSL, the planner, the audit chain and the suppression rules were all built; the transport was
not. `query/query-api/src/http/` is that transport, and `query/query-api/Dockerfile` is the image. A
lab cannot start a service that has no entry point, which is why the read path was missing from this
table rather than merely unwired.

**Not in it, deliberately.** Blob storage (azurite and minio are both cached here, but content-vault
performs no blob I/O in this build — it holds wrapped keys, and ciphertext goes device-to-blob under a
grant, so a fake blob account would prove nothing), the dashboard (a static file), and the device tier
(runs on the host). docs/lab/LAB-COST.md §6 lists what that leaves untestable.

**One thing the lab cannot show, stated rather than implied.** `query-api` runs here with
`SAC_DEV_TRUST_PRINCIPAL=1`, which accepts a development header in place of the authenticated
session — because that session is not built yet. Every read in a real deployment would be refused
`403 unauthorised_role` until it is. The lab proves the pipeline runs end to end against the real
schema; it does not prove who is allowed to ask.

## The files

| File | What it does |
|---|---|
| `build.mjs` | Cross-compiles the Go services and builds all three lab images. `--skip-docker` compiles only; `--production` prints the commands a networked host would run instead |
| `run.mjs` | Up, smoke test, report. Leaves the lab running; `--down` tears it down with the volume; `--no-up` smokes against something already running |
| `docker-compose.yml` | The four containers and their wiring. There is no `build:` stanza on purpose — see "The offline constraints" |
| `tools/check-config-agreement.mjs` | **The checker that keeps the configuration honest.** It parses the `SAC_*` names out of `azure/main.bicep`, each service's own source and its Dockerfile, and fails the build when the deployment passes a name a binary never reads, or a binary reads a name nothing accounts for |

That last one is worth knowing about even if you never run the lab. It is the reason a name in
`azure/main.bicep` cannot quietly stop meaning anything, and it is how the `query-api` gap was found:
before it listed `query-api`, the checker reported the names the deployment passed as "declared with
no binary in this repository yet" — a note, rather than the defect it actually was.

## The credential the lab uses

`ingest-api` refuses to serve without device authentication, and the lab has no certificate authority.
So the compose file starts it with two flags that exist for exactly this:

* `-dev-trust-principal` — the device principal comes from `X-Dev-Tenant-Id` / `X-Dev-Device-Id`
  instead of a client certificate;
* `-dev-seed-principal tenant:device` — registers that principal in the in-memory store, because the
  authoritative credential check still runs.

Both are refused in a deployment (and `-dev-trust-principal` is mutually exclusive with
`-tls-client-ca`). They live in `localdev/docker-compose.yml` rather than in the image, so the production
image cannot accidentally trust a header. There is no secret anywhere in this directory: the
PostgreSQL password is a throwaway for a container on the host's loopback, and the lab's tenant,
device and key material are fixtures.

## The offline constraints, stated

* **No Go toolchain image is cached**, so the Dockerfiles' `build` stages cannot run here. The `lab`
  stage of each Dockerfile packages a binary compiled by `localdev/build.mjs` on the *host*, which is why
  the lab works at all offline. `node localdev/build.mjs --production` prints the two commands a networked
  host runs instead.
* **The host is Windows and the container is Linux**, so the build cross-compiles (`GOOS=linux`,
  `CGO_ENABLED=0`). With cgo off there is no cross toolchain to install, and the resulting static
  binary runs in alpine and in distroless/static alike.
* **The document says PostgreSQL 16; this uses 17.** The only PostgreSQL image cached here is
  `postgres:17-alpine`, and the schema applies to it unmodified — that is the same engine T2 verified
  the invariants against. On a host with a network, `postgres:16-alpine` is a one-word change.

## Configuration: one vocabulary, five artifacts

Every setting is settable by a flag and by an environment variable, **and a flag wins**. Precedence is
decided by asking the flag package which flags were passed, so an explicitly empty `--region ""` is a
request rather than an absence. The environment names are the deployment's names: the `SAC_*` variables
`azure/main.bicep` passes.

| Setting | Flag | Environment | Set by |
|---|---|---|---|
| listen address | `--addr` | `SAC_HTTP_ADDR` | image (`0.0.0.0:8080`), because a container's loopback is unreachable |
| store mode | `--store` | `SAC_STORE` | image (`memory`), the only mode this build can serve |
| contract schema | `--schema` | `SAC_SCHEMA` | image (`/etc/sac/…`), because a repository path does not exist in a container |
| route ranking | `--routes-file` | `SAC_ROUTES_FILE` | image — `ref.route_fidelity` ships as data, never compiled in (§4.4) |
| database host | `--pg-host` | `SAC_PG_HOST` | **deployment** |
| database name | `--pg-database` | `SAC_PG_DATABASE` | **deployment** |
| identity | `--role` | `SAC_ROLE` | **deployment** |
| blob endpoint | `--blob-ciphertext-endpoint` | `SAC_BLOB_CIPHERTEXT_ENDPOINT` | **deployment** (read, validated, reported unused — see below) |
| telemetry | — | `SAC_APPINSIGHTS` | **deployment** (read, validated, never logged, not exported to) |
| region | `--region` | `SAC_REGION` | **nobody yet** — see the gap below |
| key backend | `--key-backend` | `SAC_KEY_BACKEND` | image (`local`) |
| internal ingress | (n/a) | `SAC_INTERNAL_ONLY` | **deployment**; also the vault's acknowledgement for a non-loopback bind |
| non-loopback ack | `--allow-non-loopback` | `SAC_ALLOW_NON_LOOPBACK` | image/laptop spelling of the same statement |

Two of those deserve their own sentence. `SAC_BLOB_CIPHERTEXT_ENDPOINT` and `SAC_APPINSIGHTS` are
passed by the deployment to processes that do not use them: the endpoint is validated at boot and the
startup log says plainly that no blob I/O happens, and a connection string nobody exports to is
reported as configured. Reading a parameter and saying it is unused is honest; leaving it unread is
what F3 was.

**The one known gap** is `SAC_REGION`: the binary refuses a tenant pinned to another region (§12), and
`azure/main.bicep` passes no region, so that check is inert in Azure today. It is listed in the
checker's extension table and printed on every run rather than quietly tolerated.

### The check that keeps it equal

```powershell
node localdev/tools/check-config-agreement.mjs
```

It reads `azure/main.bicep`, both services' Go sources, both Dockerfiles and this directory's compose
file, and fails if:

* a name the deployment passes is not read by the service it is passed to (F3);
* a name a binary reads is accounted for by neither the deployment, the image, nor the extension table;
* an image or compose setting is not read by the binary it configures;
* the parser finds nothing — because a checker that silently matches nothing always passes.

Its sibling Go tests (`{ingestion,vault}/*/cmd/*/infra_agreement_test.go`) assert the same property from inside
each module, where `go test ./...` and the acceptance gate run them on every commit. Verified by
breaking a name on purpose: the checker reported it from three directions at once.

## What is not fixed

**Three PostgreSQL servers, three ports, and none of them knows about the others.** This lab publishes
PostgreSQL on host `5432`. `database/tools/run-invariants.ps1` starts its *own* throwaway server on
`55434` by default — moved there from `55432` precisely because `55432` is where an earlier ad-hoc
`shadowpg` fixture lived, and a full acceptance run failed with `docker run failed for image
postgres:17-alpine` when the two collided. `query/query-api`'s integration tests reach whichever of
these is up.

They are different harnesses with different contracts — the lab's server is the one the services
share, the runner's is wiped and rebuilt per run, and a fixture is neither — so they are not merged.
But they do collide, and the failure is worth recognising:

- the invariants runner fails with `docker run failed for image postgres:17-alpine` if its port is
  taken. Move it: `-Port 55435`, or stop whatever holds it;
- this lab fails with `Bind for 0.0.0.0:5432 failed: port is already allocated` if anything else holds
  `5432`. Nothing in the repository does by default;
- `query/query-api`'s integration tests authenticate with the **lab's** credential and therefore
  prefer the lab container by name. A container they cannot authenticate to is skipped, not failed —
  see `test/helpers.mjs`, where that preference is the fix for seven integration tests that reported
  a password error against the invariants runner's randomly-passwords server.

Neither failure is silent, and each names the port or the container — which is the part worth having.

**F4 — no PostgreSQL driver.** `--store sql` refuses to start in both services, and the refusal names
the driver (`github.com/jackc/pgx/v5/stdlib`), the variables it read, what *is* verified (every
statement, executed against a live PostgreSQL by the live-schema tests) and the exact commands a
networked host runs. It does **not** fall back to memory, and no driver is vendored that this host
cannot test.

The honest detail, discovered while checking: pgx/v5 v5.11.0 **is** in this host's module cache, with
all of its dependencies. What makes the one-line wiring unsafe here is the acceptance harness — it runs
every Go package with `GOMODCACHE` pointed at an empty `.tools/gopath/pkg/mod`, so a `require` line
would turn the packages gate red on a host where the module cannot be fetched. A host whose module
cache is the default one can close F4 in a single commit; `node localdev/build.mjs` would then build an
image whose `SAC_STORE=sql` works, and the compose file's services would move from `memory` to `sql`
without another change.

**A deployment still cannot authenticate devices.** The origin must see the device certificate (§2.1),
and `azure/modules/container-app.bicep` probes with `scheme: 'HTTP'` on the same port a TLS listener
would use, with no volume mount and no `command`/`args`. Making the container *reachable and probeable*
is done; making a real deployment *authenticated* needs a decision in `azure/` (HTTPS/TCP probes, a
second port for probes, or an explicit edge-forwarded-certificate mode) — that is a trust-model
decision, reported rather than taken.

**Per-event `readyz` depth.** `ingest-api`'s readiness asks the store to read `ref.route_fidelity`, a
real query once the SQL mode exists. `content-vault`'s asks whether the configured key backend is
implemented — which is the one thing it can fail today: a process started with an acknowledged-but-absent
KMS is alive and must not be sent traffic. Neither probe is a constant, and neither pretends to check a
dependency the build does not have.
