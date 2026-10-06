# 11. Rules in policy

## Problem

Nothing in the product lets a tenant say "block a prompt that contains a credential" or "warn
when someone uses an unsanctioned coding agent, and point them to the approved one":
- The policy bundle carries no rules, so every device decision is the fixed
  `{rule_id: "policy.default", action: "logged"}`.
- The browser extension has a rule matcher (`device/extension/src/enforce.js`), but nothing feeds
  it.
- Sanction state (`ops.tool.sanctioned_state`) is reporting-only and never reaches a device.

Hooks (tasks 36–41), the proxy (task 46) and the extension (task 13) all need one rule set.

Note: `ref.rule` is the classifier's detection-rule metadata and is unrelated. The new rules are
"enforcement rules".

## Goal

An admin maintains an ordered list of enforcement rules on the Settings page. The signed bundle
carries the list (`rules`) and the tenant's sanctioned tools (`sanctioned_tools`), in the shapes
in `DESIGN.md` §5, and the device decodes and validates both.

## Scope

- **Database** (`services/database/schema.sql`):
  - `ops.enforcement_rule (tenant_id, position int, rule_id text, action text, match jsonb, message text, link text, updated_by, updated_at)`,
    with:
    - PK `(tenant_id, rule_id)` and a unique `(tenant_id, position)`;
    - CHECKs for the `rule_id` pattern, the `action` set (`allow`, `warn`, `block`), `message`
      ≤ 280, and `link` starting with `https://`;
    - a CHECK that `match` has only the five §5 keys, each an array of strings.
  - A trigger refuses `match.labels` values not in `ref.data_class`.
  - Row-level security, grants (`sac_control`), and `invariants.test.sql` cases.
- **control-api**:
  - `internal/settings`:
    - `GET /admin/v1/settings/rules` returns the ordered list;
    - `PUT /admin/v1/settings/rules` replaces the whole ordered list in one transaction (at most
      100 rules), audited with the previous and new lists;
    - admin role, as the existing routes.
  - `internal/policyserve`: `Bundle.Rules` and `Bundle.SanctionedTools` (fingerprints whose
    `ops.tool.sanctioned_state = 'sanctioned'`), filled in `compose()`. A sanction change made
    through either existing sanction route therefore mints a new bundle too, which `current()`'s
    payload comparison already handles.
  - Store reads and writes on the `Store` interface, the `storetest` fake, and statements in
    `sql.go`.
- **Device** (`device/capture-core/policy/bundle.go`):
  - `Rule`, `RuleMatch`, `Bundle.Rules` and `Bundle.SanctionedTools`.
  - `Validate` refuses:
    - an unknown action;
    - a duplicate `rule_id`;
    - a bad pattern;
    - an over-long message;
    - a non-https link;
    - an unknown route in `match.routes`.

  Evaluation is task 12.
- **Dashboard** (`services/dashboard/src`):
  - A Settings card, "Enforcement rules": an ordered table (action, conditions, message) with add,
    edit, delete and move up/down. Saving sends the whole list.
  - The condition pickers:
    - labels from `ref.data_class` (served by the existing settings or vocabulary endpoint; add
      it to `GET /admin/v1/settings` if none serves it);
    - tools from the tenant's known tools;
    - categories from the catalog's category set (§4; task 14 adds the catalog, so until then
      the category picker lists the fixed set from §4);
    - sanction (sanctioned / unsanctioned);
    - routes (the collection routes, by their display names).
  - A rule whose message is empty is allowed for `allow`. `warn` and `block` require a message.
  - Dashboard tests for render, edit and the PUT body.

## Done when

- The drift test (task 01) passes with `rules` and `sanctioned_tools`.
- control-api tests cover replace-list ordering, every refused value, the audit row, and compose
  with rules and sanctioned tools.
- On the lab, in the browser:
  1. Create a rule "block `credential` with message 'Remove the credential and try again.'".
  2. Sanction one tool.
  3. Show that `GET /v1/policy` for the lab device carries both in its payload.
- `node tools/accept.mjs` passes.
