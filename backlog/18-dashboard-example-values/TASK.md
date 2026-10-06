# 18. Use real identifiers in the dashboard's hints and tests

## Problem

The Search page's hints, a code comment and the dashboard's test data use identifiers that no longer
exist anywhere in the product:

- `claude_web` as the example tool fingerprint (`services/dashboard/src/explore-model.js` lines ~7, 75,
  113, 385; `test/explore-fake.mjs`; `test/explore.test.mjs`). The tool catalogue's identifiers look
  like `tls_b6681b043244c43f` (Claude Code) and, from the extension, `tf1:…`.
- `PCI_PAN_PATTERN` as the example rule code (`explore-model.js` ~111, `test/explore-fake.mjs`). The
  shipped rule ids are those in `ref.rule` in `services/database/schema.sql`, e.g. `PAYMENT_CARD_PAN`.

An analyst typing the hinted value finds nothing.

## Goal

Every example the page shows, and every identifier the tests rely on, is one the product really
produces.

## Scope

- `services/dashboard/src/explore-model.js` hints and comments.
- `services/dashboard/test/` fixtures and assertions: use real catalogue fingerprints and rule ids
  (copy them from `services/database/schema.sql`; if task 17 has landed, a `tf1:` web fingerprint is the
  better tool example).
- Out of scope: any behaviour change.

## Done when

- `git grep -n -E "claude_web|chatgpt_web|copilot_chat|PCI_PAN_PATTERN|GOV_ID_NUMBER" -- query/`
  finds nothing.
- `npm test` passes in `services/dashboard`.
