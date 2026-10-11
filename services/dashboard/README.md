# dashboard

The analyst-facing web application: Overview, Rules (the enforcement rules, read top to bottom like
a firewall's rulebase: each rule for the people, groups, departments, units or teams it names, the
tools and the labels it checks, its action and the prompts it decided over 30 days, edited as a
draft that is saved whole; and the custom labels rules can name), Usage (tools, data classes, teams,
and users: the people list and each person's page), Devices, Audit trail, Settings (Settings,
Deployment, Identity Providers), Archive (pre-production only: Insights, with Exposure, the prompts
that matched an enforcement rule on one of their labels, what happened to them, the tools they went
to and the labels involved, each against the period before, and every enforcement rule with the
prompts it matched; Adoption, how many people use AI out of the directory, split by the sanction of
their tools, the applications that sent their first prompt in the window, and each day's or week's
prompts by sanction; and Discovered, every AI application, CLI, IDE extension, local model and
inference endpoint the devices' inventory scan found, with the prompts each sent, and the devices
whose scan is not reporting), and the Search page, where an analyst lists events, findings, devices
and audit entries, opens a device to see each of its collectors with its state and cause, searches
prompt text, and reads an event's stored prompt. An admin adds, edits and deletes teams on Teams,
from any mix of the people they tick and the groups, departments and units the identity provider
synced and other teams.

It has two halves and no dependencies:

- **The pages** (`index.html`, `explore.html`, `src/*.js`, the stylesheets) are plain ES modules
  served as they are. They build closed query documents (`src/dsl.js`, `src/questions.js`); the
  browser never speaks SQL and never holds a token. `src/transport.js` is the only module that
  calls `fetch`, always on the page's own origin.
- **The server** (`server/`) signs people in through control-api's identity service, keeps the
  product access token per session (the browser holds one opaque HttpOnly cookie), and forwards:
  `/v1/*` to query-api and `/admin/v1/*` to control-api with `Authorization: Bearer <product token>`,
  `/onboard/*` and an identity provider's SCIM calls (`/scim/v2/*`, with their own bearer token
  and never a cookie) to control-api untouched, `GET`/`HEAD` of the browser extension's update manifest
  and CRX (`/v1/extension/updates.xml`, `/v1/extension/shadow-ai-capture.crx`) to control-api with
  no credential, and a minted retrieval URL
  (`GET /v1/content/retrieval/<tenant>/<grant>`) straight to content-vault, so content never
  transits query-api. A state-changing `/v1` or `/admin/v1` request from another origin is refused.

Roles and the environment decide what is offered: `server/session.mjs` maps each role to its
capabilities and pages, and query-api and control-api enforce the same map (`test/parity.test.mjs`
keeps them equal).

## Configuration

The server refuses to start unless every required variable is set.

| Variable | Required | Meaning |
|---|---|---|
| `SAC_CONTROL_URL` | yes | control-api: its internal identity API (`/internal/v1/auth/*`), admin API, onboarding pages and extension downloads |
| `SAC_INTERNAL_TOKEN` | yes | The bearer for control-api's `/internal/*` API (Key Vault secret `sac-internal-token`); never sent to a browser |
| `SAC_PUBLIC_URL` | yes | The address people use, e.g. `https://shadow.example.com`. The sign-in redirect URI is `{SAC_PUBLIC_URL}/callback`; cookies are `Secure` when it is https |
| `SAC_QUERY_API_URL` | yes | query-api, for `/v1/*` |
| `SAC_CONTENT_VAULT_URL` | yes | content-vault, for minted retrieval URLs only |
| `SAC_HTTP_ADDR` | no | Listen address, `host:port` or `:port`; default `0.0.0.0:8080` |
| `SAC_ENVIRONMENT` | no | `preprod` or `prod` (the default). Pre-production also offers the pages still being built, under Archive in the navigation; production offers neither, and its Overview's Sensitive prompts tile opens nothing |

Routes: `/signin`, `/signin/start`, `/callback`, `/signout`, `/login` (control-api's onboarding
hand-off), `/session` (who is signed in and which pages the roles may open), `/healthz` and
`/readyz` (no dependencies). Every other page and read needs a session. Logs are JSON lines on
stdout.

## Build and run

```bash
docker build -f services/dashboard/Dockerfile -t dashboard .   # from the repository root
docker run --rm -p 8080:8080 \
  -e SAC_CONTROL_URL=http://control-api:8080 -e SAC_INTERNAL_TOKEN=... \
  -e SAC_PUBLIC_URL=https://shadow.example.com \
  -e SAC_QUERY_API_URL=http://query-api:8080 -e SAC_CONTENT_VAULT_URL=http://content-vault:8080 \
  dashboard
```

Without Docker: `node server/main.mjs` with the same environment (Node 22).

## Test

```bash
npm test     # node --test "test/*.test.mjs"
```

The tests need no network beyond loopback: the server tests start fake control-api and query-api
listeners on port 0, and the page tests drive the DOM-free controllers against fake transports
(`test/fixtures.mjs`, `test/explore-fake.mjs`). `test/parity.test.mjs` imports query-api's registry,
templates, result states and roles, so it runs from a full checkout.
