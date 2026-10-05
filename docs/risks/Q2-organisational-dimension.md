# Q2 — the organisational dimension

**Task:** T7 · **Status:** answered · **Date:** 2026-10-02 · **Evidence:** live catalog and file:line
references below, each checked rather than recalled.

## The question, and why it was open

The brief never says where the organisational dimension — team, department, business unit — comes
from. Three of the ten headline questions need it: **Q2** (which unsanctioned tools, and *by whom*),
**Q3** (how much usage is growing, *per team*) and **Q6** (has a given person's usage changed).

## What is already true (checked, not assumed)

The dimension is **not missing**. It is built and plumbed end to end, with one gap.

| Fact | Evidence |
|---|---|
| `ops.user_dim` exists with `department`, `population`, `manager_ref`, `status`, `synced_at` | live catalog; `database/schema.sql:320` |
| The pre-aggregated read path exists: `mart.agg_org_period` keyed by `department` | live catalog; `database/schema.sql` mart section |
| The query API serves it: `mart.agg_org_period` is a registered DSL source with a `department` dimension | `query/query-api/src/registry.js:354,365` |
| The dashboard can render Q3 today | the source is registered, so no dashboard gap exists for it |
| Unmapped users are handled deliberately: `agg_org_period` is "empty when `department IS NULL`; unmapped users reported, never dropped" | `docs/04-dashboard-and-query.md:591` |
| The mapping to a real person is the sensitive artefact and is protected accordingly: `directory_object_id_enc` is encrypted, is the only column that maps `user_ref` to a person, and is **never exported** | `database/schema.sql:332`; `docs/04-dashboard-and-query.md:963` |

## The actual finding: nothing populates it

**No code in this repository writes a single row to `ops.user_dim`.** Every reference to the table is
a declaration, a grant, a comment or a read:

- `database/schema.sql` — the table, its comment, its entry in the row-level-security table list, and three
  `GRANT`s.
- `query/query-api/src/registry.js:11` — listed as a source.
- `endpoint/capture-core/core/envelope.go:79` — the wire's `user_ref` field. The file carries the
  pseudonymous reference and has no directory identifier and no reference to the table.
- `docs/04-dashboard-and-query.md:157,262` — "requires the Q2 directory sync", "synchronised from the
  customer's directory (Entra ID by default)".

The design says a sync exists. The system has no sync, and no component that could host one:
`control-api` — the component the architecture assigns enrolment, policy and directory work
to — **does not exist**. It has never been built, and it is not in the seven tasks either.

So the honest statement of Q2 is narrower than "where does the dimension come from" and more useful:
**the dimension has a home, a read path and a protection story; what is missing is the component that
fills it.** Until that exists, `mart.agg_org_period` is empty and Q3 renders an empty aggregate — and
because `department IS NULL` is explicitly the "unmapped" case, an empty result is reported as
unmapped rather than as a crash, which is the correct behaviour but also means **the gap looks like a
data state rather than a missing component**. That is the finding worth recording.

## Decision

**The org dimension is synchronised from the customer's directory, and the sync belongs to
`control-api`.** Specifically:

1. **Source: the customer's directory, via an explicit sync the customer enables** — Entra ID
   department and manager attributes for the default case, with a customer-supplied CSV/SCIM feed as
   the fallback for customers whose directory is not Entra. This is the only source that produces
   data the customer would recognise as true; domain heuristics and device-group membership were
   considered and rejected below.
2. **`control-api` owns it**, because that is where enrolment, policy and device identity already
   live, and because a directory sync is a control-plane action with a credential — not something
   the ingest path or a device should be able to reach. `ops.user_dim` already grants `INSERT,
   UPDATE` to the role that will own it (`database/schema.sql:1942`).
3. **Absence is a state, not an error, and it must be visible.** A tenant with no sync has an empty
   dimension; Q3 then renders "not answerable with current data" rather than an empty chart, exactly
   as §591's "unmapped users reported, never dropped" requires for the partial case. The dashboard
   already has the machinery for this — `query/dashboard/src/unavailable.js` is a catalogue of exactly
   this kind of gap, and the directory-sync entry belongs there (it currently says "Settings: …
   directory sync" from the configuration angle rather than the data angle).
4. **`directory_object_id_enc` stays the only mapping to a person**, encrypted, never exported, and
   reached only through the subject-export and erasure paths. The dimension columns (`department`,
   `population`, `manager_ref`) are readable to the query role because they are needed for
   aggregation; the identifier is not.

## Alternatives considered

- **Email-domain heuristics.** Rejected: a domain maps to a company, not a team, so it would answer
  Q3 with a single bucket labelled as an organisation and look like data. It is the failure mode the
  brief warns about — a number nobody can defend.
- **Device group membership.** Rejected: it places a *device* in a group and the question is about a
  *person*. Shared devices, and one person with several devices, both break it, and the mapping would
  silently disagree with the directory the customer uses for everything else.
- **Deriving it from the AI tool's account metadata.** Rejected: the product exists precisely because
  the employer is not the account holder on those tools. There is nothing to read.
- **Populating it from the ingest path.** Rejected: it would put a directory credential on the write
  path whose whole design is that collectors hold no privileged access, and a per-event directory
  lookup is a per-event cost for a dimension that changes monthly.

## Consequences

**Easier.** Q3 works as soon as a sync exists, because the aggregate, the source and the dimension are
already registered and the schema is already keyed for it. Nothing in the read path needs to change.

**Harder, and this is the part to plan for.** A directory sync is customer-side work: someone at the
customer must grant a read of department and manager attributes, and the product must tolerate a
tenant that has not. The erasure path gains a second store to reach, because `ops.user_dim` holds
`synced_at` data about a person that is not an event — and it must be reached without breaking the
"never dropped" rule for aggregates already computed under an old department. A person who changes
team mid-quarter must not have their history rewritten.

**We now maintain.** A distinction between "the dimension is unknown for this user" and "the
customer has no sync at all", because they render differently and an operator will otherwise read an
empty Q3 as a working tenant with unusual data.

## What would close it

`control-api` with a documented sync endpoint, `ops.user_dim` written by it, and a dashboard
state for "no directory sync configured" distinguished from "this user is unmapped". That is a
component, not a decision — which is why this record answers Q2 and leaves the build to the board.

## What this record cannot know

Stated rather than left implicit, because the task asked for it and because a decision record that
implies more certainty than it has is worse than one that admits its edges.

1. **What a real customer's directory will actually return.** Whether Entra ID department attributes
   are populated for a typical 500–5,000-employee buyer is a fact about customers, not about this
   repository. It has never been checked against a tenant. If a material share of users have no
   department set, Q3 is answerable only for the mapped subset, and the aggregate's "unmapped users
   reported" behaviour becomes the normal case rather than an edge — which would make the *denominator*
   the thing to design for, and this record does not.
   *How to close it: one real tenant's directory export, counted by coverage.*
2. **How often it changes, and what a change means for history.** A person who moves team mid-quarter
   has usage attributed to one team for part of the window and another for the rest. Recomputing the
   aggregate would rewrite history; not recomputing leaves two rows that disagree with the directory.
   §591 stops short of naming this. It is the same class as ADR 0004's immutability question, in a
   dimension nobody has thought about yet.
   *How to close it: a decision on whether `agg_org_period` is point-in-time or as-at-now, taken with
   the erasure and retention owners, because it also decides what an erasure receipt must say.*
3. **Whether `manager_ref` can be used at all.** It is a reporting-line attribute about a person, and
   the brief's legal workstream has not spoken to it. This record keeps it readable for aggregation
   and out of exports, following `directory_object_id_enc`'s treatment, but that is a conservative
   default rather than a ruling.
   *How to close it: the same legal workstream that owns the notice acknowledgement. Engineering can
   only attest to what the system does with it.*
4. **The cost of the sync.** A full directory read per tenant per interval is small at this scale,
   but it is a new customer-side credential and a new failure mode (a sync that stops does not look
   like an error — it looks like an unchanged organisation). Nothing in this repository measures it.
   *How to close it: it appears as a line item and a health signal once `control-api` exists.*

## Built (backlog/06, 2026-10-05)

`control-api` now exists, and decision 2 is built as it stands: `control-api sync-directory` fills
`ops.user_dim` for a tenant, from Microsoft Entra ID through Graph (application client credentials)
or from a JSON directory export. It is idempotent and upserts per `user_ref`; a user the directory no
longer returns is retired (`status = 'inactive'`) rather than deleted, so the "never dropped"
behaviour of aggregates already computed survives a person leaving. A user the directory holds with
no department is written with NULL and counted in the explicit `unmapped` series. The mapping from
the endpoint's `user_ref` to a directory identity is a configurable directory attribute
(`onPremisesSamAccountName` by default) whose value the device's `--user-ref` is set to — stated and
tested because the design left it open. `directory_object_id_enc` is sealed with AES-256-GCM under a
per-tenant key derived from a deployment master key.

Still open from this record: nothing measures the sync's cost or staleness (closing note 4), the
directory credential is a command-line/environment secret rather than a managed identity (a
deployment concern), and `manager_ref` remains unused (closing note 3). A person who changes team
still has their earlier usage attributed to the old department until the trailing aggregate window
recomputes (closing note 2) — the sync does not rewrite history, and the aggregate is as-at-now.

