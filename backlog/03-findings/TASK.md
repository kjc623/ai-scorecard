# 03. Findings

Depends on: nothing.

## Problem

Search > Findings and the Overview "Findings" table are always empty ("Not yet covered").
`mart.finding` has no rows, although `ingest.submission` rows carry labels such as `payment_card`,
`source_code` and `customer_pii`. Nothing in the repository writes `mart.finding`.

## Goal

A finding is raised when a submission matches a policy rule, for example a payment card number sent
to an unsanctioned tool. Implement the evaluation that turns classified submissions into
`mart.finding` rows: rule id and title, severity as of detection, class, subject, tool, the
submission it refers to, and a review state that starts at `open`.

Also implement the review write path if it does not exist: an analyst marks a finding confirmed or
disputed (`ops.finding_review`) through an audited endpoint, and the dashboard reads the new state.

## Read first

- `docs/04-dashboard-and-query.md` section 3 (`q5_findings`), and the findings parts of `docs/03`.
- `database/schema.sql`: `mart.finding`, `mart.v_finding`, `ops.finding_review`.
- `control/control-api`: where the signed policy bundle and its rules are defined.
- `query/dashboard/src/explore-model.js`: the findings dataset, its columns and filters.

## Propose before building

Write these into `DECISIONS.md` with your recommendation:

- Whether findings are raised at ingest time or by a job over `ingest.submission`.
- Where rule definitions come from: the policy bundle if it has them, otherwise a minimal rule set
  and where it is stored.
- How a rule change affects history. The design says severity is as-of-detection.

## Done when

Sending a prompt containing a test card number from the lab device produces a finding visible in
Search > Findings and on the Overview; opening it shows the event; and "Open findings" counts it.
Tests cover rule matching and idempotence: re-evaluating does not duplicate a finding.
