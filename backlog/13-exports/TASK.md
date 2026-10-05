# 13. Exports

Depends on: 06 (resolving a person), 11 (roles).

## Problem

Nothing can be taken out of the product. There is no export of an events or findings list, and no
way to produce everything held about one person for a data-subject request. The dashboard's Known
gaps page lists both as having no API.

## Goal

1. **List export.** An analyst exports the current filtered events or findings list as CSV. It is
   bounded in size, generated server-side as a job, downloadable once from a short-lived link, and
   audited with the filters used. Metadata only; no prompt content.
2. **Subject export.** An admin requests everything held about one user reference: events, findings
   and stored content. It resolves the person through `ops.user_dim`, produces an archive, and is
   audited.
3. **Subject erasure,** if the design pairs it with export: removal of a person's data with an
   erasure receipt. The vault already has shredding and receipts (`ops.erasure_receipt`); wire the
   rest.

## Read first

- `docs/04-dashboard-and-query.md` sections 9 and 10: exports and subject export.
- `docs/03` and `docs/06`: erasure, receipts, holds.
- `database/schema.sql`: `ops.erasure_receipt`, and the comments on `ops.user_dim`.
- `query/dashboard/src/unavailable.js`: the gap entries describe what each needs.

## Constraints

An export is the easiest way to turn this product into a ranking of people, which the design
forbids. Exports keep k-suppression for aggregate data, carry no sort by volume across people, and a
list export of per-person rows requires the analyst role. State in the report how each was enforced.

The dashboard tests currently assert that the Search page offers no export or download
(`query/dashboard/test/explore.test.mjs`). Update them deliberately with the new rule rather than
deleting them.

## Done when

The agent verifies, on the device-auth lab:

- The Search page, observed in the browser, offers the export, and starting it there produces a
  download link. The CSV fetched from that link holds the filtered events and no prompt content.
- The export appears in the audit trail with the filters used.
- A subject export for `lab-user`, in the owner's tenant, produces an archive containing that
  user's events and stored prompts. An export reads; it changes nothing there.
- If erasure is built, it is exercised on a simulated person in the sample tenant, and never on
  `lab-user`.
- Tests cover size bounds, link expiry and role checks.

The owner verifies:

- Clicking the export in a desktop browser saves the file.
