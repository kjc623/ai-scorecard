# localdev

A local copy of the production system: the production images, configured and wired as
`azure/main.bicep` deploys them, on one machine with Docker. A real Windows device can be enrolled
into it with a double-clicked MSI.

| Container | What it is |
|---|---|
| `postgres` | PostgreSQL 16. `db-setup` gives it Azure's shape (a non-superuser administrator with `CREATEROLE`, one login per component); the `migrate` image applies the schema as that administrator and grants each login its role |
| `control-api`, `ingest-api`, `content-vault`, `query-api`, `dashboard` | The services, each connecting as its own login (`control-api`, `ingest-api`, …), so row-level security applies as in production |
| `jobs` | The `jobs` binary on a loop: `aggregate` every minute, `expire` every hour |
| `edge` | The device ingress, as Application Gateway: TLS with a client certificate requested but not required, the certificate forwarded in `X-Client-Cert`, and only the device API reachable (`/v1/events` to ingest-api; `/v1/enrol`, `/v1/policy`, `/v1/health`, `/v1/content/grant`, `/v1/content` to control-api; anything else 403) |
| `oidc` | A customer identity provider: an OpenID Connect provider with one realm per lab tenant and no passwords |

## Run

Docker Desktop and Node 22.

```
node localdev/build.mjs        # build the images (sac/<name>:lab); again after changing a component
node localdev/run.mjs          # start or update the lab, seed it, run the smoke
node localdev/run.mjs --down   # stop it; the database is kept
```

`run.mjs` ends with the smoke, run from inside the lab network: every service ready, the edge
refusing anything but the device API, a device enrolling with the sample tenant's deployment key
(and refused without it), its event accepted, its policy bundle verifying under the pinned key, and
an analyst signing in through the identity provider and reading that event on the dashboard. It
exits non-zero if a check fails. The containers restart with Docker. To start from an empty
database: `docker compose -f localdev/compose.yaml down -v`, then `run.mjs`.

| | Address |
|---|---|
| Dashboard | <http://127.0.0.1:8787> |
| Devices (the edge) | `https://127.0.0.1:8443` |
| Sign-in pages | <http://127.0.0.1:8790> |
| PostgreSQL | `127.0.0.1:55435`, user `postgres`, password `sac-lab-only`, database `shadow` |
| Logs | `docker compose -f localdev/compose.yaml logs -f` |

`LAB_DASHBOARD_PORT`, `LAB_EDGE_PORT`, `LAB_OIDC_PORT` and `LAB_PG_PORT` move a port, and
`LAB_PUBLIC_HOST` (default `127.0.0.1`) is the host name browsers and devices use; set them in the
environment or in `localdev/.env`. `docker compose -f localdev/dbview.compose.yaml up -d` adds a
read-only table browser on <http://127.0.0.1:8089>.

## Tenants and sign-in

| Tenant | For | Sign in as |
|---|---|---|
| Lab (`10ca1ab0-0000-4000-8000-000000000001`), M3, prompt-text search | the device installed from the lab MSI | `viewer@lab.test`, `analyst@lab.test`, `reader@lab.test` (content reader), `admin@lab.test` |
| Northwind Freight (`5a3c0de0-7e57-4a11-9000-0000000d3a01`), M2 | the smoke and simulated devices | the same names `@sample.test` |

Enter the email on the dashboard's sign-in page; the identity provider signs that account in without
a password. `node localdev/seed.mjs` writes the tenants, their identity provider connections, their
sign-in domains and their deployment keys; `run.mjs` runs it every time, and it leaves existing
tenants and keys as they are.

`node localdev/tools/simulate-devices.mjs` enrols ten simulated devices in Northwind Freight through
the edge and sends them about 300 events and a health report each (`--devices`, `--events`).

`node localdev/tools/observe.mjs <address>` opens one dashboard page in a headless Chromium and saves
a screenshot, the page text and the rendered DOM to `--out`; it is a local inspection tool, not part
of any image.

## The lab MSI

On Windows, with Go 1.27 and WiX 7 (`dotnet tool install --global wix`), with the lab started once:

```
node localdev/lab-msi.mjs
```

builds the product's release (`device/installer/release-msi.mjs`) with the lab's policy key, classifier key
and extension key, into `localdev\.msi\`, and writes `ShadowAICapture.tenant.env` beside the MSI:
the Lab tenant, the edge, the tenant's deployment key and the lab CA (`SAC_CA_FILE`). Double-click
`localdev\.msi\ShadowAICapture.msi`: the agent installs as the `ShadowAICapture` service, enrols
and starts sending. Uninstall it from Settings → Apps. control-api serves the same folder as its
agent release, and the dashboard forwards its browser extension downloads, so the update manifest is at
`http://127.0.0.1:8787/v1/extension/updates.xml`.

## Key material

Created on first use and then kept, because an installed agent and the database depend on it:
`localdev/.authlab/` holds the device CA (which also signs the edge's certificate) and
`localdev/.authlab-identity/` the signing keys, the internal token, the directory and content keys,
the identity provider's client secret, the classifier key and the deployment keys
(`identity/identity.mjs` lists them). Both are git-ignored. Deleting either strands an installed
agent and makes sealed or encrypted rows unreadable.

## How it differs from production

- No Front Door: the dashboard is published directly, so `/scim/v2/*` and `/.well-known/*` are not
  on its origin (the dashboard forwards `/onboard/*` itself).
- The edge does not require SNI and has no managed firewall rules beyond the device-path allow-list.
- Secrets come from files instead of Key Vault, and every component logs in to PostgreSQL with a
  password over an unencrypted connection instead of a Microsoft Entra token over TLS.
- The tenants, their identity provider connections and their deployment keys are seeded with SQL.
- Sign-in is through the lab identity provider; with no vendor Entra app, Intune device verification
  is unavailable.
- The jobs run in a loop, not as scheduled jobs.

## Files

| File | |
|---|---|
| `compose.yaml` | The lab |
| `build.mjs`, `run.mjs`, `seed.mjs`, `lab-msi.mjs` | Build, run, seed, and the lab MSI; `lab.mjs` holds what they share |
| `oidc/`, `jobs/` | The identity provider and the job loop (the edge is `services/edge`) |
| `authlab/` | The device-side tool: `authlab pki` makes the lab's PKI, `authlab smoke` is the smoke |
| `tools/simulate-devices.mjs` | Simulated devices |
| `tools/observe.mjs` | Open one page in a headless browser; save its screenshot, text and DOM |
| `harness.compose.yaml`, `harness/`, `QUICKSTART.md` | Coding-agent harnesses on the lab network |

Tests: `npm test` in `localdev`, and `go vet ./... && go test ./...` in `authlab` and `oidc`.
