# 07. Data class filter on events

Depends on: nothing. This is an investigation and a fix, and the smallest task here.

## Problem

On the Search page, filtering events to a data class returns nothing. The address
`explore.html?transport=live#events?class=payment_card` shows "Not yet covered: the system cannot
answer for this window" and 0 events. The unfiltered list for the same 7-day window contains rows
whose Classes column shows `payment_card`. The Policy action filter (`#events?action=logged`) works
and returns 50 rows, so the filter plumbing is sound in general.

## What is known

- The dashboard sends `class` as a filter on the `q8_activity` template.
- In `query/query-api/src/registry.js` the `ingest.submission` source defines `class` as a
  filter-only predicate over the `labels` jsonb column, an array of `{class, score}`, not as a
  dimension.

## What is not known

Nothing past that was traced. It has not been ruled out that the answer is a deliberate
`not_yet_covered`, for example a guard that requires an aggregate or an index the lab does not have.

## Do

Reproduce with a direct POST to `query-api`'s `/v1/query`. Read the compiled SQL (`compile.js`,
`plan.js`, `guard.js`). Determine whether this is a bug in the predicate, a cost-guard refusal
reported as the wrong state, or missing data. Fix it. If the refusal is correct, make the response
say why in a way the page can show.

## Done when

The class filter returns the matching events on live lab data, alone and combined with another
filter, and a test pins the compiled predicate. The report gives the root cause in one paragraph.
