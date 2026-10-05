# 09. Prompt search with filters

Depends on: nothing.

## Problem

On the Search page, prompt-text search and the filter row are separate reads. A text search covers
every uploaded prompt and cannot be limited to a user, a tool, a device or a date range, so the page
hides the filters while text results are showing. Results are capped at 20 with no paging.

## How it works now

The dashboard POSTs `/v1/content-search` to `query-api`, which forwards to `content-vault`
(`query/query-api/src/http/content.js`). The vault searches `ingest.search_text` and returns
submission ids with snippets (`vault/content-vault/internal/vault/search.go`,
`internal/store/sql.go` `SQLSearchTerms`). `query-api` then looks up subject, tool and device for
the hits from `ingest.submission` (`describeHits` in `query/query-api/src/http/server.js`).

The vault holds no submission metadata, and `query-api` may not read `ingest.search_text`. That
separation is deliberate (`docs/06` section 4.4).

## Goal

One search that takes terms plus optional subject, tool, device, received-at window and collection
mode; returns matching prompts newest first, with paging; and stays within the separation above.

Then update the dashboard (`query/dashboard/src/explore-app.js` `searchText`,
`src/explore-render.js` `renderExploreText`): the filter row stays visible and applies to text
results, and results page.

## Propose before building

Write the design into `DECISIONS.md` with the trade-off you chose. Two candidates: the vault accepts
a bounded list of candidate submission ids from `query-api`; or filter metadata is denormalised into
the index row at index time.

## Done when

Searching "Australia" with the Person filter set returns only that person's prompts; the window
switch applies; and more than 20 matches can be paged. The search stays audited, with the filters
recorded. Tests in the vault, `query-api` and the dashboard.
