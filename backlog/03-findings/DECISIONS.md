# 03. Findings — decisions to confirm before building

Three questions the task asks to decide first. Each has the options, what the repository already
says, and a recommendation. The recommendation is what I will build unless told otherwise.

The goal it serves: a submission that matches a policy rule becomes a `mart.finding` row; an analyst
can confirm or dispute it through an audited write; Search > Findings and the Overview read it.

---

## D1. Are findings raised at ingest time, or by a job over `ingest.submission`?

**Options**

| | A. Evaluation job over `ingest.submission` | B. Raised inside ingest (`ingest.record_event` / ingest-api) |
|---|---|---|
| Where | `aggregation/aggregator` (new pass beside the aggregates) | the device write path |
| Idempotence | native — re-running replaces/ignores by natural key | needs care across batch retries; the ingest path is already retry-free by design but a replayed batch must not double-raise |
| Handles a submission upgraded in place | yes — a later higher-fidelity route can add labels, and a re-evaluation sees them | no — a finding raised once is already written |
| Handles a late spool flush | yes — trailing window | yes, at arrival |
| `mart` stays rebuildable | yes — `DROP mart.finding` and re-run reproduces it | **no** — findings would live only in the write path; dropping `mart` would lose them unless ingest is replayed |
| Rule/service deployed | no device change | every device or the ingest service must carry rule knowledge |
| Latency to visible | ≤ one aggregator interval (30 s in the lab, 5 min default) | immediate |
| Blast radius | a bug re-derives wrong rows, fixable by re-run | a bug rejects or misclassifies a device event at the edge |

**What the repository already decides**

- `database/schema.sql`: "mart — derived, rebuildable, no workflow state"; `mart.finding`'s comment
  repeats it. A finding must be reproducible from `ingest`, which ingests cannot guarantee.
- Brief C28 / `docs/04` §4.3: derived data is recomputed, never incremented, because late arrivals
  and in-place upgrades are normal.
- `aggregation/aggregator` already runs per tenant in one transaction as `sac_ops`, which already
  holds `SELECT, INSERT, UPDATE, DELETE ON mart.finding` (`database/schema.sql` §10). No new role,
  no new grant, no new container.

**Decision: A (built).** Add a second pass to the existing aggregator: for each tenant, derive findings
from `ingest.submission` over the same trailing window, in the same transaction, written with
`INSERT ... ON CONFLICT (tenant_id, submission_id, rule_id) DO NOTHING` so a re-run cannot duplicate
and severity stays as-of-detection (D3). The lab aggregator already ticks every 30 s, which meets the
"visible within one cadence" reading; the freshness block reports the age like every other derived
read. I will not read through to raw events on the read path (that would violate C27), and I will say
plainly in the report that a default deployment's finding visibility is bounded by the 5-minute
cadence, not the 60 s event target.

---

## D2. Where do rule definitions come from?

**Options**

| | A. `ref.rule`, seeded with a minimal set | B. Parse the signed policy bundle | C. Invent rules in the evaluator |
|---|---|---|---|
| Exists today | table exists, **empty**; it is `mart.finding.rule_id`'s FK target and the documented "server-side mirror so a finding can be joined to a human-readable rule" | `ops.policy_bundle` exists but **has no writer** in this build (control-api README, "What is not built yet" #2) and stores scope→mode, not rule bodies | — |
| Matches the FK | yes | needs a writer first | no |
| Changeable without a release | yes (`ref` is reference data) | yes, once built | no |
| Honest about provenance | yes — a rule the classifier did not publish is simply absent | yes | no — server would assert rules it never published |

**What the live lab shows**

The device emits labels whose `rule_id` names a real detection rule. In the auth lab today:
`PCI_PAN_PATTERN`, `GOV_ID_NUMBER`, `PII_CUSTOMER_RECORD`, `SRC_INTERNAL_REPO`, `SECRET_API_KEY`,
`PHI_CLINICAL_TERM`, `LEGAL_CONTRACT_TERMS`, plus `PAYMENT_CARD_PAN` from the Windows device. The
statistical labels (`customer_pii`, `source_code` with no `rule_id`) name no rule. `ref.rule` is empty,
so nothing can be joined or referenced yet.

**Decision: A (built).** Seed `ref.rule` in `database/schema.sql` §11 with the rule ids the classifier
actually publishes, each mapped to its `ref.data_class`, a `detector_kind`, a `severity` and a title
(the dashboard's own sample vocabulary in `query/dashboard/src/explore-stub.js` is the canonical
wording). A label that names a `rule_id` not in `ref.rule` raises **no** finding — inventing a rule
server-side would assert a rule the classifier never published, and the class still appears in
`mart.agg_class_period` via its `ref.data_class.default_severity` fallback. The full rule body lives
with the classifier release; `ref.rule` is metadata only, exactly as its schema comment says.
A tenant-specific rule set is a later task (the policy-bundle writer); the seed is the minimum that
makes the natural key real and the lab demonstrable.

---

## D3. How does a rule change affect history?

**What the design already fixes**

`docs/04` §3.5: "Severity is as-of-detection. `mart.finding.severity` is materialised when the finding
is raised, so reclassifying a rule in `ref.rule` does not retroactively relabel history." The view
`mart.v_finding` joins `ref.rule` only for `title`. So severity/class are frozen; title is live.

**Options** (the owner asked whether a rule change should propagate to past findings)

| | A. Freeze the decision (committed design) | B. Type-1 overwrite — propagate the new value to every past finding | C. Version the rule (SCD Type 2 / bitemporal) |
|---|---|---|---|
| What changes on a rule edit | nothing historical; the view already joins the current `title` | every existing finding's `severity`/`class` is rewritten (via a cascade UPDATE, or by dropping the column and joining live) | a **new rule version row** is inserted; old findings keep pointing at the version they matched |
| "Why was this `high` last month?" | answerable — that is what was recorded | **not answerable** — it now reads `critical`, which was never true | answerable from the finding's rule version |
| "What does policy say now?" | needs a second, clearly-labelled join (proposed below) | shown, but indistinguishable from what was decided | shown, and distinguishable |
| New rule over old events | none — no backfill | none | none |
| Write cost of a rule edit | one row in `ref.rule` | a mass UPDATE across every matching finding (years of them) | one `INSERT` |
| Rebuildable | yes (`mart` derives from `ingest` + `ref`) | derived data is rewritten outside its derivation | yes |
| Schema change | none | none | new `ref.rule_version` (or effective-dated `ref.rule`) + a version column on `mart.finding` |

**What best practice actually says.** This is a solved problem in dimensional modelling and in the
security-tooling domain, and the evidence points the same way as the design:

- Kimball's slowly-changing dimensions: **Type 1 (overwrite) is for corrections; Type 2 (new version
  with effective dates) is for changes that are historically meaningful**, and different columns of
  the same dimension can use different types. A category "the change reflects something real… any
  past sales will suddenly appear as Seattle sales, which is factually wrong" for Type 1 is exactly
  the severity case.
- The rule catalogue would be a *dimension*; findings are *facts* stamped with the dimension version
  in force at the event. Type 2 facts "permanently bound to the time slice" need no update; a Type-1
  overwrite of the dimension forces the aggregate/fact table to be rewritten. Versioning the rule is
  the cheaper option, not the more expensive one.
- Bitemporal history (valid time + transaction time, SQL:2011 system/application time; Fowler):
  "we don't change what we thought we knew… record history itself is append only." Fowler's working
  example is a payroll check that records the salary at the time it was issued — recording the inputs
  of a decision is enough for audit. That is `mart.finding.severity`.
- DLP tooling: Symantec keeps an **incident snapshot** of the detection and an **Incident Attributes
  Change Log** of old/new attribute values rather than overwriting; Microsoft Purview **re-evaluates
  policy on access but does not relabel the historical alert**; and a governance playbook for AI DLP
  states plainly: "Capture… before disabling, editing, or re-scoping the rule. A common audit finding
  is 'the rule was changed before evidence was captured, and the firm cannot reconstruct the failed
  configuration.'" Mutating past findings is the failure mode that guidance exists to prevent.

**Merit in the idea, and the gap.** The instinct is right: an analyst triaging today *does* want to
see findings under the current rule, and normalising the rule must not mean costly propagation.
The defect is only in "every transaction in it is also updated" — that overwrites the decision of
record. Two mechanisms capture the merit without the loss:

1. **Show both values, labelled (cheap, recommended for 03).** Keep `mart.finding.severity` as
   detected (A), and add a present-tense `current_severity` to `mart.v_finding` joined from
   `ref.rule`. A finding then reads "raised high · policy now critical". This is the Symantec
   "snapshot + current status" pattern, is a view-only change, and needs no propagation at all.
   The dashboard's sample data already has the notion of a rule title changing independently.
2. **Version the rule catalogue (proper, follow-up).** `ref.rule` gains an effective-dated version
   keyed by `(rule_id, classifier_release)`; `mart.finding` records the version it matched (the
   submission already carries `classifier_version`, and `ref.rule.introduced_in`/`retired_in` already
   reference `ref.classifier_release`). Then "as decided" and "as of any date" are both exact.
   This is a real schema change and more than this task needs, so I would record it as a follow-up
   unless you want it now.

**Decision (owner, 2026-10-05): current severity only — no as-of-detection machinery.**

> "I don't think the findings feature is worth this much complexity. We don't need to worry about
> severity changing because we'll only ever care about the current severity as that's how the
> customer will create their rules."

Taken literally, which is the simpler model:

- `mart.finding` no longer stores `severity` or `class_code`. A finding is `(submission, rule, when,
  how)` — the match, not the rule's attributes.
- `mart.v_finding` reads `severity`, `class_code` and `title` from the **current** `ref.rule` at query
  time. This is present-tense configuration joined at read, the same shape `mart.v_tool_usage`
  already uses for `ops.tool.sanctioned_state`.
- No rule versioning, no `current_severity` column, no backfill, no propagation job.

The trade-off is on the record: editing a rule's severity changes how old findings read. That is the
owner's accepted behaviour, because the customer authors the rules and wants their current definition
to govern. `docs/04` §3.5, §3.11 and §4.1 and the schema comments are updated to say so rather than
to keep claiming as-of-detection.

The write is still `ON CONFLICT (tenant_id, submission_id, rule_id) DO NOTHING`: idempotence is
required by the task, and re-evaluation must never duplicate a finding.

One consequence I will state in the report: if a submission is later upgraded and a label it once
carried is replaced, the old finding row stays. That is the conservative direction (a finding is a
record that a rule matched, and that is true of the moment it was raised). Deleting on re-evaluation
would make an analyst's reviewed finding vanish under them, which is worse.

---

## Not in the three, but part of the goal: the review write path

There is **no** write path today. `query-api` is read-only (`src/http/server.js` serves `/healthz`,
`/readyz`, `POST /v1/query` and the two content reads) even though `sac_query` already holds
`SELECT, INSERT, UPDATE ON ops.finding_review` (`database/schema.sql` §10). Plan unless told
otherwise:

- a new `POST /v1/finding-review` in `query-api`, transport-only like the rest: tenant and actor from
  the session, never the body; it writes `ops.finding_review` **and** one `ops.audit` row
  (`finding.review`) in the same transaction, then returns the new state;
- the body names `submission_id`, `rule_id`, `review_state` (`confirmed` / `disputed`) and an optional
  `note`; `open` is not settable — it is the absence of a review, not an action;
- the dashboard reads the new state already (`q5_findings` returns `review_state`), so the visible
  half is a confirm/dispute control on a finding row that calls the new endpoint.

---

## As built (2026-10-05)

- **D1:** a findings pass in `aggregation/aggregator`, over the trailing day window, in the same
  per-tenant transaction as the aggregates. Insert-only.
- **D2:** `ref.rule` seeded in `database/schema.sql` §11 with the eight rule ids the classifier
  publishes; a label naming any other rule raises nothing.
- **D3:** `mart.finding` no longer stores `severity`/`class_code`; `mart.v_finding` reads them from the
  current `ref.rule`. `docs/04` §3.5/§3.11/§4.1 and the schema comments were updated to match.
- **Review:** `POST /v1/finding-review` in query-api writes `ops.finding_review` and one `ops.audit`
  row in one transaction; the dashboard reads the new state. An in-page confirm/dispute control was
  **not** built — the task's Done-when does not require one and it would add a fourth browser
  endpoint; recorded as an open item in `REPORT.md`.

The existing-database migration is `MIGRATION.sql` in this folder, and is described in `REPORT.md`.
