# 20. Move the dashboard's lab tool into `localdev/`

## Problem

`services/dashboard/tools/observe.mjs` drives a headless browser against the lab to screenshot pages and
read their text. It is lab and agent tooling, not part of the dashboard that ships, but it lives inside
the product package, and `services/dashboard/README.md` documents it there.

## Goal

`observe.mjs` lives in `localdev/tools/` with the other lab tools, and `services/dashboard/` contains only
what the dashboard image ships and its tests.

## Scope

- `git mv services/dashboard/tools/observe.mjs localdev/tools/observe.mjs`; fix its relative imports and
  default paths; delete `services/dashboard/tools/` if empty.
- Update every reference (`git grep -n "observe.mjs"`): move its documentation from
  `services/dashboard/README.md` to `localdev/README.md`. The owner's untracked harness files under
  `localdev/harness/` may also refer to it: list those in your report rather than editing them.
- If it needs an npm dependency, declare it in `localdev/package.json`.
- Out of scope: changing what the tool does.

## Done when

- `git grep -n "dashboard/tools"` finds nothing.
- `node --check localdev/tools/observe.mjs` passes, and `npm test` passes in `services/dashboard` and in
  `localdev`.
