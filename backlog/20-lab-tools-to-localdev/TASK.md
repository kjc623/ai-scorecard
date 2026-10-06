# 20. Move the dashboard's lab tool into `localdev/`

## Problem

`query/dashboard/tools/observe.mjs` drives a headless browser against the lab to screenshot pages and
read their text. It is lab and agent tooling, not part of the dashboard that ships, but it lives inside
the product package, and `query/dashboard/README.md` documents it there.

## Goal

`observe.mjs` lives in `localdev/tools/` with the other lab tools, and `query/dashboard/` contains only
what the dashboard image ships and its tests.

## Scope

- `git mv query/dashboard/tools/observe.mjs localdev/tools/observe.mjs`; fix its relative imports and
  default paths; delete `query/dashboard/tools/` if empty.
- Update every reference (`git grep -n "observe.mjs"`): move its documentation from
  `query/dashboard/README.md` to `localdev/README.md`. The owner's untracked harness files under
  `localdev/harness/` may also refer to it: list those in your report rather than editing them.
- If it needs an npm dependency, declare it in `localdev/package.json`.
- Out of scope: changing what the tool does.

## Done when

- `git grep -n "dashboard/tools"` finds nothing.
- `node --check localdev/tools/observe.mjs` passes, and `npm test` passes in `query/dashboard` and in
  `localdev`.
