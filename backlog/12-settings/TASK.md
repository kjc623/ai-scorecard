# 12. Settings

Depends on: 04 (applied mode per device), 05 (tool catalogue), 11 (the admin role).

## Problem

An administrator cannot change anything from the product. Collection mode (M0 to M3) is fixed at
install time by the installer profile, retention is fixed, and tool sanction decisions cannot be
set. The dashboard used to carry a placeholder Settings page saying no API exists; it was removed.

## Goal

An audited, admin-only API in `control-api`, and a Settings page in the dashboard, for:

- Collection mode per tenant, with an optional narrower override, within the tenant's ceiling. A
  change is delivered to devices through the signed policy bundle and takes effect without a
  reinstall.
- Retention periods for events and for stored content, within the limits the design allows.
- The sanction decision per tool, using the catalogue from task 05.
- The content search tier per scope: disabled, attachment names, full text.

## Read first

- `docs/00-architecture.md` and `docs/02`: collection modes and the policy bundle.
- `control/control-api`: bundle signing and distribution.
- `docs/03`: retention and holds.
- `database/schema.sql`: the constraints tying search tier to collection mode and to key custody.
  Several combinations are refused by the database and must be refused in the page with a clear
  message.

## Requirements

- Every change is audited with actor, old value and new value.
- A mode increase that begins capturing content needs an explicit confirmation step in the page.
- The page shows the mode each device has actually applied (task 04 supplies it), not only the mode
  requested.
- The Settings page is added to the dashboard's navigation (`query/dashboard/src/shell.js`
  `NAV_GROUPS`) and is visible to admins only.

## Done when

Changing the lab tenant from M3 to M1 in the page stops content upload from the lab device within
one policy refresh, and changing it back resumes it, with both changes in the audit trail. Tests
cover the refused combinations and the bundle contents.
