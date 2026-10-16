# dashboard

The analyst-facing web application. The navigation holds four destinations, each with its pages as
tabs under its title, and Archive and Settings at its foot; every other screen is opened from a row,
a tile or a card. A role is offered only the pages it may open, and an address from an earlier
layout lands on the page that holds it now.

- **Overview**: the window's prompts, attachments, sensitive prompts, users and devices reporting,
  the tools by sanction, the rules at work and the tools in use. Every card that shows a figure, here
  and on every other page, opens where its rows are: a page, Search, or the table below it on the
  same page; what a figure counts is behind the (i) beside its label.
- **Usage**: Tools (every tool by prompts, narrowed by sanction decision, and each tool's people and
  findings), Teams (an admin adds, edits and deletes teams from any mix of the people they tick, the
  groups, departments and units the identity provider synced, and other teams) and Users (everyone
  the directory or a device has named, with their teams and prompts, and each person's page).
- **Search** (`explore.html`): an analyst lists events and findings, searches prompt text, and
  reads an event's stored prompt. An event read from a provider's admin API names the provider
  where a device would be, and one a device and a provider both collected names both. Each event
  lists every route that collected it, and the Route and Routes filters find events by any of them
  or those several collected.
- **Policy**: Rules (the enforcement rules, read top to bottom like a firewall's rulebase: each rule
  for the people, groups, departments, units or teams it names, the tools and the labels it checks,
  its action and the prompts it decided over 30 days; everyone reads them, and an admin edits them
  as a draft that is saved whole), Labels (the custom labels rules can name), Tools (each tool's
  sanction decision, made there by an admin) and Activity (pre-production only: the prompts that
  matched an enforcement rule on one of their labels, what happened to them, the tools they went to
  and the labels involved, each against the period before).
- **Archive** (pre-production only): Insights, with Adoption (how many people use AI out of the
  directory, split by the sanction of their tools, the applications that sent their first prompt in
  the window, and each day's or week's prompts by sanction), Discovered (every AI application, CLI,
  IDE extension, local model and inference endpoint the devices' inventory scan found, with the
  prompts each sent, and the devices whose scan is not reporting), Demand (the tools not sanctioned
  by team, each team's people using each out of its size, and the people each tool would need a seat
  for) and Coding agents (each coding agent's people, actions and tokens against the period before,
  and the models that received its code and context).
- **Settings**: Collection (the collection mode, prompt storage, retention, per-tool overrides, TLS
  inspection, the kill switch, the endpoint collectors and how apps that need a restart are
  reported), Devices (each device with the mode it applied and the apps on it that need a restart,
  and each of its collectors with its state and cause; an admin installs the agent below the
  list), Identity Providers (the directory read, sign-in and SCIM provisioning), Integrations
  (every AI provider whose admin API the product reads, as control-api lists them: an admin
  connects each with its key and account or the tenant's Entra consent, replaces its key, pulls it
  now or disconnects it, and sees its last pull and the records read) and Audit trail (filtered by
  actor, action, object type and case). A viewer's Settings is Devices alone.

It has two halves and no dependencies:

- **The pages** (`index.html`, `explore.html`, `src/*.js`, the stylesheets) are plain ES modules
  served as they are. They build closed query documents (`src/dsl.js`, `src/questions.js`); the
  browser never speaks SQL and never holds a token. `src/transport.js` is the only module that
  calls `fetch`, always on the page's own origin.
- **The server** (`server/`) signs people in through control-api's identity service, keeps the
  product access token per session (the browser holds one opaque HttpOnly cookie), and forwards:
  `/v1/*` to query-api and `/admin/v1/*` to control-api with `Authorization: Bearer <product token>`,
  `/onboard/*` and an identity provider's SCIM calls (`/scim/v2/*`, with their own bearer token
  and never a cookie) to control-api untouched, a provider's log deliveries (`POST /intake/v1/*`,
  streamed, with the headers that carry the connection's token and never a cookie) to control-api,
  `GET`/`HEAD` of the browser extension's update manifest
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
| `SAC_ENVIRONMENT` | no | `preprod` or `prod` (the default). Pre-production also offers the pages still being built, Policy → Activity and Archive; production offers neither, and its Overview's Sensitive prompts tile opens Policy → Rules |

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
