# query/dashboard

The analyst-facing read path: the ten questions of [`docs/04`](../../docs/04-dashboard-and-query.md)
§3 asked through the **closed query DSL** of [`query/query-api/DSL.md`](../../query/query-api/DSL.md),
laid out as the information architecture of §11.

Zero dependencies. No build step for the application. Plain ES modules, HTML and CSS.

## How to open it

| Entry | Command | What it is |
|---|---|---|
| `index.html` | open the file directly | The whole app in one inline module, so `file://` works. This is the one to look at. |
| `module.html` | `node tools/serve.mjs` to <http://127.0.0.1:8787/module.html> | The ES-module entry: real `import`s, which a browser only allows over http. |
| `explore.html` | open the file directly | The search page: one query bar and filter rail over the four list reads (events, findings, devices, audit trail), with a detail panel for the row you click. Generated the same way as `index.html`. |

`explore.html` runs on its own sample transport (`src/explore-stub.js`), which applies the window
and every filter and pages with a real cursor, so a search can be seen to work. The page state is
in the address (`#events?window=d30&action=blocked&open=<id>`). A query term it cannot place, free
text included, blocks the search and says why; nothing is dropped. Its modules are
`src/explore-model.js` (datasets, query parsing, requests), `src/explore-app.js` (controller and
boot), `src/explore-render.js` and `explore.css`; `test/explore.test.mjs` pins it.

`index.html` and `module.html` run against the **stub transport** by default, so every state is visible with no server, no
session and no database. The navigation bar's last item, *State gallery*, forces one result state
across every screen: fresh, stale, degraded coverage, not-yet-covered, all-suppressed, refused,
cursor-expired, audit-unavailable, busy, chain-broken.

**Why `index.html` is generated.** A browser refuses an external ES module fetch from a `file://`
page — the module request is cross-origin from origin `null` — so a page opened straight off disk
cannot use `<script type="module" src="./src/app.js">`. `tools/build-index.mjs` inlines the module
graph into one inline module block, which fetches nothing and therefore loads. `src/*.js` is the
source of truth; `test/index-html.test.mjs` regenerates `index.html` and fails if it has drifted.

```bash
node tools/build-index.mjs          # regenerate index.html after editing src/
node tools/build-index.mjs --check  # what the test runs
node tools/probe.mjs                # drive every screen through the built page and print a table
```

## Starting Explore

Explore has two modes. The mode is chosen by the address, and the top bar says which one is on.

| Mode | Address | Top bar shows | Data |
|---|---|---|---|
| Sample | `explore.html` | "Sample data, state" and a dropdown | Generated in the browser by `src/explore-stub.js`. Needs nothing else running. |
| Live | `explore.html?transport=live` | "Live query API" | `POST /v1/query` on the page's own origin, forwarded to a real `query-api`. |

### Sample mode

Any of these works:

```bash
# 1. no server: open the file
start explore.html                       # Windows; `open` on macOS, `xdg-open` on Linux

# 2. the package's own server
node tools/serve.mjs                     # http://127.0.0.1:8787/explore.html

# 3. a container (build context is the repository root)
docker build -f query/dashboard/Dockerfile -t sac-dashboard .
docker run -d --name sac-dashboard -p 127.0.0.1:8787:8787 sac-dashboard
```

The dropdown in the top bar forces the states a healthy sample never reaches: nothing found, refused
as too broad, busy, audit unavailable, a cursor that expires on page two, and a destroyed record.

### Content on the Explore page

Explore carries the two content reads, so an analyst does not leave the page to read a prompt:

- **Search prompt text** (the second box under the filter query) asks the content vault for prompts
  containing the words given, and shows a fragment per match. It is not a filter of the list: it is
  a separate, audited read, and opening a match opens that event.
- **Retrieve content** is in an event's detail panel when its content state is `uploaded`. It takes
  a case reference, a second approver and a justification, and the vault decides. What comes back is
  shown two ways: *what the user typed*, and, collapsed beneath it, *everything captured for the
  request* (a client such as Claude Code wraps the typed text in its own context, resends the
  conversation, and sends telemetry nobody typed). Retrieved content is held only while that event
  stays open.

Both go to their own endpoints on the page's origin (`/v1/content-search`, `/v1/content/retrieval`),
which `query-api` forwards to `content-vault`; neither is a query, and no query returns content. In
sample mode they run against the sample transport. The split of typed text from client context is
display logic in `src/explore-model.js` (`exploreUserInput`): it knows Claude Code's
`<system-reminder>` blocks and the Anthropic message format, and shows any other capture whole.

The analyst is whoever the session says is asking, which today is the development principal
`tools/serve.mjs` names. The second approver is a name the requester types: the vault refuses the
requester's own name, but nothing yet makes the second person approve.

### Live mode

The local auth lab runs this for you: `docker compose -f localdev/authlab.compose.yaml up -d` starts
a `query-api` and this dashboard beside the lab's services, reading the lab tenant, and serves
<http://127.0.0.1:8787/explore.html?transport=live>. The rest of this section is how that works and
how to run it by hand against another tenant.

The browser never chooses a tenant. `tools/serve.mjs` forwards the query endpoint and the two
content endpoints to the `query-api` named by `--api` (or `SAC_QUERY_API_URL`) and adds the
development principal headers itself, from `SAC_DEV_TENANT` and `SAC_DEV_ACTOR`. This is a
development forwarder: it works only against a `query-api` started with `SAC_DEV_TRUST_PRINCIPAL=1`,
and it stands in for the authenticated session, which is not built. The content reads also need that
`query-api` to be given the vault (`SAC_CONTENT_VAULT_URL`, and `SAC_CONTENT_SEARCH_SCOPE` for search).

```bash
SAC_DEV_TENANT=<tenant uuid> node tools/serve.mjs --api http://127.0.0.1:8083
# then open http://127.0.0.1:8787/explore.html?transport=live
```

**Which database has anything in it.** The default lab (`node localdev/run.mjs`) starts its
`ingest-api` with `--store memory`, so events posted to port 8080 never reach PostgreSQL and the
`query-api` on port 8082 reads an empty database. Events only land in a database through the auth
lab, whose `ingest-api` uses the SQL store. The auth lab has no `query-api` of its own, so one is
started beside it. From the repository root:

```bash
# 1. the auth lab: its own PostgreSQL, control-api, ingest-api and the edge on :8443
node localdev/build.mjs --auth
node localdev/run.mjs --auth

# 2. sample events, sent the way devices send them (enrol, token, DPoP-signed batches)
node localdev/tools/simulate-devices.mjs            # prints the tenant id it used

# 3. a query-api reading the auth lab's database
docker run -d --name sac-authlab-query-api --network scorecard-authlab \
  -p 127.0.0.1:8083:8080 \
  -e SAC_PG_HOST=postgres -e SAC_PG_DATABASE=shadow \
  -e SAC_PG_USER=postgres -e SAC_PG_PASSWORD=sac-lab-only -e SAC_PG_SSLMODE=disable \
  -e SAC_DEV_TRUST_PRINCIPAL=1 sac/query-api:lab

# 4. the dashboard, forwarding to it as the simulator's tenant
docker build -f query/dashboard/Dockerfile -t sac-dashboard .
docker run -d --name sac-dashboard --network scorecard-authlab \
  -p 127.0.0.1:8787:8787 \
  -e SAC_QUERY_API_URL=http://sac-authlab-query-api:8080 \
  -e SAC_DEV_TENANT=5a3c0de0-7e57-4a11-9000-0000000d3a01 \
  -e SAC_DEV_ACTOR=you@lab sac-dashboard

# open http://127.0.0.1:8787/explore.html?transport=live
```

`5a3c0de0-7e57-4a11-9000-0000000d3a01` is the simulator's default tenant; pass `--tenant` to it to
use another.

**If Events says "Read not served / audit_unavailable".** The tenant the dashboard reads as does
not exist in the database, so the audit row a read must write fails its foreign key and the API
serves nothing. The usual cause is that the auth lab was recreated, which empties its database.
Run step 2 again; the dashboard and the `query-api` container do not need restarting.

**After editing `src/` or `explore.css`:** run `node tools/build-index.mjs`, then rebuild and restart
the `sac-dashboard` container. The page does not refresh itself; it reads when you search.

## Built in Explore, not wired up

Each of these is on the page and works against the sample transport. Against the live API it is
empty or inert, for the reason given. None is faked in live mode.

| On the page | What live mode shows | What is missing |
|---|---|---|
| **Findings** tab, its filters, and the finding block in the detail panel | "Not yet covered", zero rows | Findings are derived into `mart.finding` by the aggregator, which is not built. |
| **Devices**: collector, collector state, last seen, dropped; the liveness and collector-state filters | Every device is `never_reported` with no collector | Collector health is reported to a control-plane endpoint that is not built. A device that has only sent events has no health row. |
| **Devices**: managed state and region | `unknown` and "not recorded" on devices enrolled by the simulator | Enrolment through the lab does not set them. |
| **Department** filter and the department line in the detail panel | The filter matches nothing | Department comes from the directory sync into `ops.user_dim`. Nothing populates it. |
| **Content** filter values `local_only`, `uploaded`, `shredded`, and their answers in the detail panel | `uploaded` for a device collecting at M3 in the auth lab, where retrieval works; otherwise `not_captured` | `shredded` needs the erasure path. Events sent by the simulator carry no content. |
| **Free text in the query bar** | Refused with the reason, in both modes, pointing at the prompt-text search | There is no text predicate on `/v1/query` by design. Text is searched in its own box. |
| **Prompt-text search** | Matches only prompts uploaded since the vault began indexing | Attachment filenames are not searched, and there is no index-coverage block saying how much of the window is indexed. |
| **The session** | A development principal added by `tools/serve.mjs` | The authenticated session that maps a signed-in person to one tenant. Until it exists, live mode is a lab arrangement only. |

Checked against the live API in a browser: the Events list, the `action` filter, the event detail
panel, the Devices list and the Audit trail (which fills with the dashboard's own reads); and, in
the auth lab with a device at M3, a prompt-text search, opening its match, and retrieving that
event's content. **Not
checked live:** "Load next page", the other filters, the refusal states from the sample dropdown,
light mode, and narrow screens. `tools/serve.mjs`'s forwarder and
`localdev/tools/simulate-devices.mjs` have no tests.

One known mismatch outside Explore: the API's single-record answer is one row per observation with
the store's column names (`user_ref`, `tool_fingerprint`, `collection_mode`, `policy_action`), while
`src/fixtures.js` models it as a head row followed by observation rows. Explore reads both shapes.
The *Event detail* screen in `index.html` still assumes the fixture shape and has not been run
against the live API.

## Layout

| File | What it owns |
|---|---|
| `src/vocab.js` | The client's mirror of the closed DSL: sources, dimensions, measures, operators, templates, result states, state pairs. `test/parity.test.mjs` is written to compare it with the read path's registry name for name, but it imports that registry from `services/query-api/src/`, which does not exist (the registry is at `query/query-api/src/`), so today its comparisons skip rather than run. |
| `src/dsl.js` | The typed builder. It can only construct a query the API admits, and it refuses the rest locally with the same codes the API would use. |
| `src/questions.js` | The ten questions of §3 as request builders. |
| `src/transport.js` | **The one data layer.** `createQueryApi({transport})` plus `stubTransport`, `httpTransport` and `collectPages`. Nothing else in this package performs I/O. |
| `src/fixtures.js` | Canned envelopes for every result state, and the twelve scenarios the gallery switches between. |
| `src/states.js` | What an envelope *means*: the never-collapse rules, the banners, the coverage and freshness sentences. `measureOf()` is the only way to obtain a value. |
| `src/views.js` | View models for Posture, the ten questions, and the refusal screen. |
| `src/unavailable.js` | Everything this API cannot express, rendered as rows with the reason and the missing source. |
| `src/render.js` | View models to HTML. Pure; every API string is escaped. |
| `src/shell.js` | What every page shares around its content: how the navigation nests the screens (`NAV_GROUPS`), the stored preferences (theme, folded groups), and the navigation panel's controls (Sample/Live, theme, mobile menu, a table row that opens its record). The dashboard and Explore both boot through it. |
| `src/app.js` | Hash routing, the screen table of §11.2, and `boot()`. |
| `tools/index.template.html` to `index.html` | The shell; the generated single-file page. |
| `styles.css`, `explore.css` | The design tokens, shell and components; then what only the search page needs. `explore.html` loads both, in that order. |

### The shell and its conventions

- **Navigation** has five destinations: Overview, Search (`explore.html`), Usage (Tools & data
  classes, Teams, Users), Devices and Audit trail. Screens that were merged are a switch on the
  screen that absorbed them: Usage shows Tools, Data classes or Unsanctioned (`#tools?view=…`), and
  Devices lists all or problems only (`#devices?show=problems`). The old addresses `#classes`,
  `#unsanctioned` and `#degraded` still resolve. Event detail (`#event`), Known gaps
  (`#unavailable`, linked from the footer) and the state gallery (`#gallery`) are not in the
  navigation. The theme and which groups are folded are the only things kept in `localStorage`.
- **Sample / Live** in the navigation panel switches `?transport=live` on the current page and
  keeps the screen. Links between the dashboard and Explore carry the same choice.
- **Charts are drawn from the response only.** Bar sizes are written as unitless custom properties
  (`--w`, `--h`), and the coverage ring prints no figure, because the page must never show a fleet
  percentage (`test/section14.test.mjs`). The segmented breakdown above a table counts the rows
  shown, and says so in its tooltip.
- **Reading notes** sit behind "About this data" at the foot of a screen instead of under every
  table. A row that names a submission opens its Event detail on click or Enter.
- **No remote assets.** Fonts are a system stack; nothing is fetched from another host.

## The one behaviour to understand first

A small cell arrives from the API as
`{result_state: "suppressed", reason: "fewer_than_k_subjects", k: 5, …}` with **no measure key at
all**, while a genuine zero arrives as `submissions: 0`. They are different facts — "there was
something and we are not telling you the number" versus "we looked and there was none" — and the
dashboard must never merge them.

`src/states.js` returns a discriminated result from `measureOf()`, never a number that might be a
lie; `src/render.js` gives the three cases three CSS classes (`.v-number`, `.v-suppressed`,
`.v-absent`); and a total over a set containing a suppressed cell is labelled `≥ n`, because it is
a **floor**. `test/states.test.mjs` and `test/section14.test.mjs` pin all of it.

## What the dashboard must never do

`docs/04` §14 is a list of prohibitions. Each one a client can violate is a test in
`test/section14.test.mjs`, not a comment: no SQL or database driver anywhere in the tree, one
endpoint and one `fetch` seam, no per-person ranking, no content, no export links, no aggregate
without its coverage and freshness state, no suppressed value rendered as zero, no state pair
merged, no health inferred from silence, no clock-skew normalisation, no chart from event rows, and
no unrecognised filter silently ignored.

## Commands

```bash
node --test test/              # 100 tests, offline
node tools/build-index.mjs     # regenerate index.html
node tools/probe.mjs           # render every screen through the built page
node tools/serve.mjs           # serve module.html over http
```

From the repository root, `node tools/verify-all.mjs` includes this package.

## What this dashboard cannot show

It is all rendered in the app under *What we cannot show*, and summarised here because a report is
where a gap belongs:

| Missing | Why |
|---|---|
| **Content search** (`/v1/content-search`) | The read path implements the structured DSL only; there is no search endpoint to call. No search UI is faked and nothing renders as "no matches". |
| **Content retrieval** (§8) | Request, second approval, audit-first reveal: no endpoint. Event detail shows metadata and the `content_state` answer instead. |
| **Exports** (§9, §10) | A job state machine, not a query. No link is rendered. |
| **Settings** (modes, retention, holds, directory sync) | `ops.tenant`, `ops.grant`, `ops.hold`, `ops.policy_bundle` are not registered DSL sources — and a change is an audited act, not a read. |
| **Degraded-collection signals** | `ingest.rejected` (rejected-envelope histogram), `ops.reconciliation_run` (drift) and per-device clock skew are not exposed. Collector state, spool depth and dropped totals *are*, and are shown. |
| **Anchored chain head** | The API verifies a page's hash links but exposes no anchor record. |
| **Window-wide low-merge-confidence share** | No aggregate carries it; the activity screen counts over the page it holds and labels the figure as page-local. |
| **Top-N per bucket** (Q1) | The DSL has no window function; a page limit truncates whole buckets. Reported rather than approximated. |
