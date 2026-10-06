# 13. Exports

## Problem

Nothing can be taken out of the product. There is no export of an events or findings list, and no
way to produce everything held about one person for a data-subject request.

## Goal

1. **List export.** An analyst exports the current filtered events or findings list as CSV. It is
   bounded in size, generated server-side, downloadable once from a short-lived link, and audited
   with the filters used. Metadata only; no prompt content.
2. **Subject export.** An admin requests everything held about one user reference: events, findings
   and stored prompts. It resolves the person through `ops.user_dim`, produces an archive, and is
   audited.
3. **Subject erasure:** removal of a person's events, findings and stored prompts, with an erasure
   receipt (`ops.erasure_receipt`). Stored prompts are deleted by content-vault, the only component
   that can touch them; the `expire` job shows how deletions cascade and how a receipt is written.

## Read first

- `docs/architecture.md`: product rules, data, prompt content.
- `query/query-api/DSL.md` and `src/registry.js`: how list reads are shaped, paged and audited.
- `database/schema.sql`: `ops.erasure_receipt`, `ops.user_dim`, and the foreign keys that cascade
  from `ingest.submission`.
- `jobs/internal/expire`: batched deletion and receipts.

## Constraints

An export is the easiest way to turn this product into a ranking of people, which the product rules
forbid. Exports keep the k = 5 suppression for aggregate data, carry no sort by volume across people,
and a list export of per-person rows requires the analyst role. Say in your report how each is
enforced.

The dashboard tests assert that the Search page offers no export or download. Update them
deliberately with the new rule rather than deleting them.

## Done when

The agent verifies, on the lab:

- The Search page, in the browser, offers the export, and starting it produces a download link. The
  CSV from that link holds the filtered events and no prompt content.
- The export appears in the audit trail with the filters used.
- A subject export for a simulated person in the sample tenant produces an archive with that
  person's events and stored prompts, and changes nothing.
- Erasure, exercised on a simulated person in the sample tenant, removes their data and writes a
  receipt that states what was removed.
- Tests cover size bounds, link expiry and role checks.

The owner verifies:

- Clicking the export in a desktop browser saves the file.
