# localdev — the $0.00 development lab (Part A of docs/lab/LAB-COST.md §1)

PostgreSQL with `database/schema.sql` applied, the three services in containers, and a smoke test that
proves they serve. No Azure, no credentials, no cost.

This directory exists because **the services could not be run in a container at all** before it: no
Dockerfiles existed (F1), both binaries bound loopback on ports the deployment does not probe (F2), the
container app passed environment variables no binary read (F3), and neither service could open a
database connection (F4). F1–F3 are fixed here. F4's state is in [What is not fixed](#what-is-not-fixed).

```powershell
node localdev/build.mjs     # cross-compile both services and build the lab images
node localdev/run.mjs       # up + smoke test + report; leaves the lab running
node localdev/run.mjs --down
```

`run.mjs` prints every check and exits non-zero if any fails. On a fresh volume all eighteen pass:

```
lab: ingest-api http://127.0.0.1:8080, content-vault http://127.0.0.1:8081, query-api http://127.0.0.1:8082
  ok   ingest-api /healthz — 200
  ok   ingest-api /readyz — 200 {"status":"ready"}
  ok   content-vault /healthz — 200
  ok   content-vault /readyz — 200 {"status":"ready","key_backend":"local-software","store":"memory"}
  ok   database/schema.sql applied (ingest tables) — 4 tables
  ok   ref.route_fidelity seeded — 7 routes
  ok   two routes are accepted, or recognised as the submission already recorded — status 200, accepted 2, duplicate 0, rejected 0
  ok   every event has a submission_id, and they are the same one — 1 submission_id(s)
  ok   neither observation is rejected, and both reach one logical submission — outcomes accepted,accepted
  ok   409 duplicate_batch — 409 duplicate_batch
  ok   200 with a per-event rejection, not a batch failure — status 200
  ok   reason mode_violation with the offending pointer — mode_violation /events/0/content_digest
  ok   query-api /healthz — 200
  ok   query-api /readyz — 200 {"status":"ready","role":"sac_query"}
  ok   a read with no tenant is refused, and reaches no database — 403 unauthorised_role
  ok   POST /v1/query answers with a §13 envelope — status 200, result_state not_yet_covered
  ok   the answer carries freshness and coverage, so a number never travels without its state — freshness=true coverage=true
  ok   a request that tries to speak SQL is refused — 400 prohibited_field
```

On a rerun against the same volume the two routes are reported `duplicate` rather than `accepted`,
which is also a pass.

## Addresses and ports

There are two ways to reach a service, and which one applies depends on where the caller runs. The
table at the top of `docker-compose.yml` is where these are decided; this is a copy for reading.

| Service | From a container on the `scorecard` network | From the host (default) | Host port variable |
|---|---|---|---|
| `postgres` | `postgres:5432` | `localhost:5432` | `LAB_PG_PORT` |
| `ingest-api` | `http://ingest-api:8080` | `http://localhost:8080` | `LAB_INGEST_PORT` |
| `content-vault` | `http://content-vault:8080` | `http://localhost:8081` | `LAB_VAULT_PORT` |
| `query-api` | `http://query-api:8080` | `http://localhost:8082` | `LAB_QUERY_PORT` |

**On the host**, the right-hand ports are defaults. Set the variable in the environment or in
`localdev/.env` to move one (`LAB_QUERY_PORT=18082 node localdev/run.mjs`). Nothing else needs
editing: `run.mjs` asks `docker compose port` which port was published.

**From another container**, join the network and use the service names. The published host ports do
not exist there:

```yaml
services:
  my-tool:
    networks: [scorecard]
    environment:
      LAB_INGEST_URL: http://ingest-api:8080
      LAB_VAULT_URL: http://content-vault:8080
      LAB_QUERY_URL: http://query-api:8080
networks:
  scorecard:
    external: true
```

With those three set, `node localdev/run.mjs --no-up` smokes the lab from inside the network. The two
database checks run `psql` inside the `postgres` container, so they need the docker CLI and report
that they did not run when it is absent; the sixteen HTTP checks need nothing but Node.

These variables are named `LAB_*`, not `SAC_*`, on purpose: `SAC_*` is the deployment's vocabulary,
and `localdev/tools/check-config-agreement.mjs` fails on any `SAC_*` name in the compose file that a
binary does not read.

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

**Not in it, deliberately.** Blob storage (content-vault performs no blob I/O in this build — it holds
wrapped keys, and ciphertext goes device-to-blob under a grant, so a fake blob account would prove
nothing), the dashboard (a static file), and the device tier (runs on the host). That leaves the blob
path, a real certificate authority and a real cloud KMS untestable here; docs/lab/LAB-COST.md §6 says
which platform properties the Azure half (Part B) can and cannot test.

**One thing the lab cannot show, stated rather than implied.** `query-api` runs here with
`SAC_DEV_TRUST_PRINCIPAL=1`, which accepts a development header in place of the authenticated
session — because that session is not built yet. Every read in a real deployment would be refused
`403 unauthorised_role` until it is. The lab proves the pipeline runs end to end against the real
schema; it does not prove who is allowed to ask.

## The files

| File | What it does |
|---|---|
| `build.mjs` | Cross-compiles the Go services and builds the lab images. `--auth` also compiles the two services with `-tags sac_sql_driver` for the device-auth lab; `--skip-docker` compiles only; `--production` prints the commands a networked host would run instead |
| `run.mjs` | Up, smoke test, report. Leaves the lab running; `--down` tears it down with the volume; `--no-up` smokes against something already running; `--auth` runs the opt-in device-auth lab against `authlab.compose.yaml` |
| `docker-compose.yml` | The five containers, their addresses and their wiring. There is no `build:` stanza on purpose — see "The build constraints" |
| `authlab.compose.yaml` | The opt-in device-auth lab: PostgreSQL with the real schema, `control-api` and `ingest-api` in `sql` mode, and the `edge` gateway stand-in. Driven by `run.mjs --auth` |
| `edge/` | A standard-library Go program that simulates Application Gateway: TLS 1.3, an optional client certificate forwarded as `X-Client-Cert`, and `X-Forwarded-Proto`/`Host`. It imports no service package and is the same path a deployment uses |
| `authlab/` | The host-side tool `run.mjs --auth` builds and runs: it generates the development PKI (fresh every run, into `.authlab/`) and drives both auth modes end to end through the edge |
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

## The device-auth lab (opt-in)

The default lab above cannot prove enrol-then-ingest: with the memory store `control-api` and
`ingest-api` share no state, so the credential one issues is not the credential the other verifies.
This opt-in lab fixes that by running both services against the real PostgreSQL schema, behind a
stand-in for the Azure edge (ADR 0020):

```
node localdev/build.mjs --auth     # also compiles the two services with -tags sac_sql_driver
node localdev/run.mjs --auth       # up, generate dev PKI, seed, smoke, report
node localdev/run.mjs --auth --down
```

It stands up PostgreSQL with `database/schema.sql` applied unmodified, `control-api` and `ingest-api`
in `sql` mode, and `edge` — a faithful stand-in for Application Gateway: it terminates TLS 1.3,
requests a client certificate **without requiring one** (so a `dpop` device needs none), forwards the
presented chain as `X-Client-Cert`, and sets `X-Forwarded-Proto`/`X-Forwarded-Host`. The service code
behind it is the production code path; the edge imports nothing from a service and no service has an
"if lab" branch.

It proves, through the edge, exactly the two production modes and their refusals:

* `x509` — generate a key and CSR, `POST /v1/enrol`, receive a leaf, `POST /v1/events` with it: accepted;
* `dpop` — generate a key and JWK, `POST /v1/enrol`, `POST /v1/token`, `POST /v1/events` with the bound
  token and proof: accepted;
* no credential is refused `401`, a replayed DPoP `jti` is refused `401`, and a certificate from
  another CA is refused `401`.

The edge answers on `https://localhost:8443` (`LAB_EDGE_PORT`), PostgreSQL on `localhost:55435`
(`LAB_AUTH_PG_PORT`). No secret is in the repository: the dev CA, the edge server leaf and the
access-token key are generated fresh into `localdev/.authlab/` on every run, and that directory is
gitignored.

### The module-cache precondition

`build.mjs --auth` compiles the two services with `-tags sac_sql_driver`, which is the only thing that
links `github.com/jackc/pgx/v5` into them, and it runs with `GOPROXY=off`. The tagged build therefore
needs `pgx` (v5.11.0) already in the host module cache; a host without it fails the tagged compile and
leaves the default lab untouched, because the tag is opt-in. This is the same constraint that keeps
the default lab in `memory` mode (see "What is not fixed"): the acceptance gates run every Go package
with an empty module cache, so nothing the default build needs may require a fetch.

## The build constraints, stated

* **The lab images are built without a Go toolchain image and without a network.** The `lab` stage of
  each Go Dockerfile packages a binary compiled by `localdev/build.mjs` on the *host* (with
  `GOPROXY=off`), so the Dockerfiles' `build` stages — which need a golang image and a module proxy —
  are never run by the lab, and the compose file has no `build:` stanza. That is what lets the lab be
  built on a host that has neither. `node localdev/build.mjs --production` prints the three commands,
  one per service, that build the `production` stage instead.
* **The host is Windows and the container is Linux**, so the build cross-compiles (`GOOS=linux`,
  `CGO_ENABLED=0`). With cgo off there is no cross toolchain to install, and the resulting static
  binary runs in alpine and in distroless/static alike.
* **The document says PostgreSQL 16; this uses 17.** The compose file names `postgres:17-alpine`, and
  the schema applies to it unmodified — that is the same engine T2 verified the invariants against.
  `postgres:16-alpine` is a one-word change in `docker-compose.yml`.

## Configuration: one vocabulary, five artifacts

Most settings are settable by a flag and by an environment variable, **and a flag wins**. Precedence is
decided by asking the flag package which flags were passed, so an explicitly empty `--region ""` is a
request rather than an absence. The environment names are the deployment's names: the `SAC_*` variables
`azure/main.bicep` passes. A few settings have only one spelling: `--pg-port`, `--dsn`, `--driver` and
the `-dev-*` test flags have no environment variable, and `SAC_APPINSIGHTS` and `SAC_INTERNAL_ONLY`
have no flag. The table lists the settings the lab and the deployment set; the binaries also read
`SAC_KEYVAULT_URI` (content-vault) and `SAC_TLS_CERT_PEM` / `SAC_TLS_KEY_PEM` / `SAC_TLS_CLIENT_CA_PEM`
(ingest-api).

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

It reads `azure/main.bicep`, the two Go services' sources, `query-api`'s `src/http/config.js`, all three
Dockerfiles and this directory's compose file, and fails if:

* a name the deployment passes is not read by the service it is passed to (F3);
* a name a binary reads is accounted for by neither the deployment, the image, nor the extension table;
* an image or compose setting is not read by the binary it configures;
* the parser finds nothing — because a checker that silently matches nothing always passes.

Its sibling Go tests (`{ingestion,vault}/*/cmd/*/infra_agreement_test.go`) assert the same property from inside
each module, where `go test ./...` and the acceptance gate run them on every commit. Verified by
breaking a name on purpose: the checker reported it from three directions at once.

## What is not fixed

**Three PostgreSQL servers, three ports, and none of them knows about the others.** This lab publishes
PostgreSQL on host `5432` by default (`LAB_PG_PORT` moves it). `database/tools/run-invariants.ps1` starts its *own* throwaway server on
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
  `5432`. Nothing in the repository does by default, and `LAB_PG_PORT` moves the lab off it;
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
and `azure/modules/container-app.bicep` probes over HTTP by default (`probeScheme`, which
`azure/main.bicep` never sets to `HTTPS`) on the same port a TLS listener would use, with no volume
mount and no `command`/`args`, and every app's `keyVaultEnv` is empty, so no certificate reaches the
container. Making the container *reachable and probeable* is done; making a real deployment
*authenticated* needs a decision in `azure/` (delivering the certificate material and switching the
probe to HTTPS, a second port for probes, or an explicit edge-forwarded-certificate mode) — that is a
trust-model decision, reported rather than taken.

**Per-event `readyz` depth.** `ingest-api`'s readiness asks the store to read `ref.route_fidelity`, a
real query once the SQL mode exists. `content-vault`'s asks whether the configured key backend is
implemented — which is the one thing it can fail today: a process started with an acknowledged-but-absent
KMS is alive and must not be sent traffic. Neither probe is a constant, and neither pretends to check a
dependency the build does not have.
