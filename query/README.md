# `query/` — everything between the database and an analyst's screen

The read half of the system. Events arrive through `ingestion/`; this is how they come back out as
answers.

| Directory | What it is |
|---|---|
| [`query-api/`](query-api/) | The only read path. A closed query DSL compiled server-side to parameterised SQL, plus the HTTP service that serves it. The same service forwards the two content reads to `content-vault`, which it cannot answer itself |
| [`dashboard/`](dashboard/) | The web application the ten questions are answered in, and the Explore page, where an analyst searches prompt text and retrieves an event's content |

## Why it is two directories and not one

They are one feature split across two runtimes and two threat models, and the split *is* the
architecture rather than an accident of language choice.

The browser never speaks SQL ([ADR 0003](../docs/adr/0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md)).
The dashboard sends a structured document from a closed grammar; `query-api` validates it, compiles
it with every value bound as a parameter, and returns an envelope. A dashboard that could build a
query could be made to build a different one — by a compromised page, an extension, or a mistyped
filter — so the capability is not given to it in the first place.

The consequence worth knowing: **the dashboard cannot express anything the DSL does not admit.** If
a question cannot be asked, the fix is a registry entry and a compiler case, not a query in the
frontend.

## How a request flows

1. The dashboard builds a document naming a source or one of the ten templates, with dimensions,
   measures, filters and a window.
2. `query-api` validates it against a closed vocabulary and refuses unknown keys, fields, operators
   and buckets rather than ignoring them.
3. A cost guard rejects a query too broad to serve instead of degrading it.
4. The compiler turns it into SQL whose identifiers came from the registry and whose values are all
   `$n` placeholders.
5. Everything runs in one transaction that commits before anything is served, so a cancelled read
   has served nothing.
6. The audit row is written **before** the answer leaves, and k-suppression is applied to small
   cells.
7. The response is one envelope carrying the data *and* its freshness, coverage and suppression
   state — a number never travels without the conditions it was measured under.

## Content is a different path

No query returns content, and the DSL has no text predicate. Prompt-text search and approved content
retrieval are two separate requests (`POST /v1/content-search`, `POST /v1/content/retrieval`) that
`query-api` forwards to `content-vault` under its own identity, adding who is asking from the session.
The vault decides everything — the search tier, the case reference and second approver, the single-use
grant, the audit row written first — and is the only component that can open content. The dashboard's
Explore page is the surface for both. The retrieval request relays the vault's short-lived, single-use
retrieval URL and no content: the browser fetches that URL from the vault through the analyst web tier,
so content never transits `query-api`.

## Where the specification lives

[`docs/04-dashboard-and-query.md`](../docs/04-dashboard-and-query.md) is the specification; the
frozen grammar for `query_version: "1"` is [`query-api/DSL.md`](query-api/DSL.md). The subsystem
document is long and section-numbered, and both directories cite it by section.

## Running it

`localdev/` brings up `query-api` against a real PostgreSQL with the schema applied, and runs a
closed-DSL query end to end as one of its checks. The dashboard is a static application and is served
by its own tooling. The local auth lab (`localdev/authlab.compose.yaml`) runs both together, with the
content vault behind `query-api`, and serves the Explore page at
<http://127.0.0.1:8787/explore.html?transport=live>.
