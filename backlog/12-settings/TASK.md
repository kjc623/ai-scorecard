# 12. Settings

## Problem

An administrator cannot change anything from the product. Collection mode (M0 to M3) is fixed per
tenant at creation, retention is fixed, and tool sanction decisions and the content search tier
cannot be set.

## Goal

An audited, admin-only API in `control-api`, and a Settings page in the dashboard, for:

- Collection mode per tenant, with an optional narrower override, within the tenant's ceiling. A
  change reaches devices through the signed policy bundle and takes effect without a reinstall.
- Retention periods for events and for stored content, within the retention classes the schema
  defines (`ref.retention_class`, `ops.retention_policy`).
- The sanction decision per tool, using the tool catalogue (`ref.tool_catalogue`, `ops.tool`).
- The content search tier: disabled, attachment names, full text.

## Read first

- `docs/architecture.md`: collection modes, the policy bundle, prompt content and search.
- `control/control-api/internal/policyserve`: how the bundle is built and signed.
- `database/schema.sql`: `ops.tenant`, `ops.retention_policy`, `ops.tool`, and the constraints on
  mode and search tier. A combination the database refuses must be refused in the page with a clear
  message.

## Requirements

- Every change is audited with actor, old value and new value.
- A mode increase that begins capturing content needs an explicit confirmation step in the page.
- The page shows the mode each device has actually applied (devices report it), not only the mode
  requested.
- The Settings page is in the dashboard's navigation (`query/dashboard/src/shell.js`) and visible to
  admins only.

## Done when

The agent verifies, on the lab:

- Changing the sample tenant's mode in its Settings page, in the browser, is accepted, and the page
  then shows the new requested mode. Changing it back does the same.
- After each change, the signed bundle a device is served carries the new mode.
- Both changes are in the audit trail with actor, old value and new value.
- A combination the database refuses is refused in the page with a clear message.
- Tests cover the refused combinations and the bundle contents.

The owner verifies, on a real device:

- After changing a tenant from M3 to M1 in the page, content upload stops within one policy refresh,
  with no reinstall; after the change back it resumes.
- The Settings page shows the mode the device has actually applied.
