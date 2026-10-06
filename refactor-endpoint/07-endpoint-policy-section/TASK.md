# 07. Policy: the endpoint section

## Problem

The new collectors need tenant settings that reach devices through the signed policy bundle:
- each collector on or off;
- each tool's native collectors on or off;
- the OTLP listen addresses;
- the discovery budget.

Today the bundle carries only collection modes, interception hosts and the CLI shim. Its
tenant-set inputs are edited on the dashboard's Settings page.

## Goal

An admin changes an endpoint setting on the Settings page. The next policy poll delivers a new
signed bundle with an `endpoint` section (`DESIGN.md` §5), and the device starts or stops the
affected collector without a restart.

## Scope

- **Database** (`services/database/schema.sql`):
  - `ops.endpoint_setting`: one row per tenant, with boolean columns `inventory`, `processes`,
    `flows`, `otel`, `hooks` and `hooks_managed_only`, and their defaults from §5.
  - `ops.endpoint_tool_setting (tenant_id, tool_key, otel boolean, hooks boolean)`, with
    `tool_key` in the closed set from §5.
  - For both tables:
    - row-level security in the tenant RLS list;
    - grants (`sac_control` reads and writes);
    - audit on change, as `ops.tenant` settings do.
  - A tenant with no row gets the §5 defaults, applied in the compose query rather than by
    inserting rows.
  - `invariants.test.sql` cases for the tool key check and RLS.
- **control-api**:
  - `internal/policyserve/bundle.go`: add `Endpoint` with the §5 JSON names, and fill it in
    `compose()`. The OTLP addresses and `discovery_daily_budget` are server constants
    (`127.0.0.1:47318`, `127.0.0.1:47317`, `200`) next to the existing proxy constants in
    `service.go`, not settings.
  - `internal/store`:
    - read through `PolicyInputs` and a new statement registered in `sql.go` `Statements`;
    - a write method on the `Store` interface;
    - the memory fake in `storetest`.
  - `internal/settings/handler.go`:
    - `PUT /admin/v1/settings/endpoint` with the collector booleans;
    - `PUT /admin/v1/settings/endpoint/tools/{tool_key}` with `{otel, hooks}`;
    - both returned in `GET /admin/v1/settings`;
    - admin role required, audited, as the existing routes are.
- **Device** (`device/capture-core/policy/bundle.go`): `Endpoint` with the same JSON names.
  `Validate` refuses:
  - a non-loopback or malformed OTLP address;
  - an unknown tool key;
  - a negative budget;
  - an interval below 15 minutes.
- **Dashboard** (`services/dashboard/src`):
  - a Settings card "Endpoint collectors": on/off for inventory, processes, flows, OTel and hooks,
    "only managed hooks", and a per-tool table (Claude Code, Codex, Copilot, Cursor; OTel and
    hooks columns, with a cell shown as unavailable where the tool lacks that collector per §5);
  - wired through `vocab.js`, `transport.js`, `settings.js` and `settings-render.js` like the
    existing cards;
  - dashboard tests for the card's render and its two requests.
- **Proof on the device without the real collectors**: use the `inventory` switch. Until task 16
  adds the inventory provider, register no provider for it. Instead, make the drift test (task 01)
  and a capture-core policy test prove the device decodes and validates the section. The live
  toggle is proven on a real provider in task 16's Done when.

## Done when

- The drift test passes with the new section, and fails if one side's JSON name is changed
  (checked, then reverted).
- control-api tests cover compose with defaults, with a tenant row and with a tool row, plus both
  PUT routes and their audit rows.
- The Settings card is observed in the browser on the lab: changing "Inventory" off and back on
  changes `GET /v1/policy`'s `bundle_version`.
- `node tools/accept.mjs` passes.
