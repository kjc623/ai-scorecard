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

Both run against the **stub transport** by default, so every state is visible with no server, no
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

## Layout

| File | What it owns |
|---|---|
| `src/vocab.js` | The client's mirror of the closed DSL: sources, dimensions, measures, operators, templates, result states, state pairs. `test/parity.test.mjs` asserts it equals the read path's registry name for name. |
| `src/dsl.js` | The typed builder. It can only construct a query the API admits, and it refuses the rest locally with the same codes the API would use. |
| `src/questions.js` | The ten questions of §3 as request builders. |
| `src/transport.js` | **The one data layer.** `createQueryApi({transport})` plus `stubTransport`, `httpTransport` and `collectPages`. Nothing else in this package performs I/O. |
| `src/fixtures.js` | Canned envelopes for every result state, and the seventeen scenarios the gallery switches between. |
| `src/states.js` | What an envelope *means*: the never-collapse rules, the banners, the coverage and freshness sentences. `measureOf()` is the only way to obtain a value. |
| `src/views.js` | View models for Posture, the ten questions, and the refusal screen. |
| `src/unavailable.js` | Everything this API cannot express, rendered as rows with the reason and the missing source. |
| `src/render.js` | View models to HTML. Pure; every API string is escaped. |
| `src/app.js` | Hash routing, the screen table of §11.2, and `boot()`. |
| `index.template.html` to `index.html` | The shell; the generated single-file page. |

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
