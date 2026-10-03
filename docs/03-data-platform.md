# 03 — Data platform

How the cloud-side data is stored, retained, erased and recovered. This document covers the **server**
tier; the device-side spool is in [01-collectors](01-collectors.md) §12, the write path that feeds
this store is in [02-ingest-and-transport](02-ingest-and-transport.md) §6, the read paths over it are
in [04-dashboard-and-query](04-dashboard-and-query.md) §3, and the full DDL is
[database/schema.sql](../database/schema.sql).

---

## 1. Scope and the shape of the problem

Brief §3.1 gives the sizing, and it decides most of this document:

| Quantity | Value |
|---|---|
| Submissions/year per full tenant | ~4.4 M |
| Event row size | 1–2 KB |
| Event data growth | **5–9 GB/year** |
| Attachment content at M3 | 50–500 GB/year |
| Raw process telemetry, unfiltered | orders of magnitude larger — never shipped (R7) |

At 5–9 GB/year this is a small relational workload. The brief's own conclusion — "the event data is
small and does not need a specialist store" — is correct and is the reason there is no warehouse, no
separate search cluster, no message broker and no partitioning in this design. The content search index
of §13 lives in this same database for exactly that reason. The engineering effort belongs
on the endpoint, not here.

What *is* hard on this tier is not volume. It is four things:

1. **Isolation that is structural rather than checked.** Brief C32 requires a cross-tenant read to be
   impossible, not merely unauthorised.
2. **Idempotency that survives overlapping collection routes** without inflating a number a customer
   will challenge (R9).
3. **Two independent deletion paths that are reconciled against each other**, because brief C34 says
   not to trust a single one.
4. **Erasure that produces evidence** rather than an assertion (§3.4), including honest accounting of
   what deliberately survived.

---

## 2. The storage decision

**PostgreSQL 16**, Azure Database for PostgreSQL Flexible Server. Not SQLite, not Supabase. 16 is the
deployment target the infrastructure pins; the local lab and the recorded verification of §10 ran on
PostgreSQL 17, to which the schema applies unmodified.

| Option | Verdict | Reason |
|---|---|---|
| **PostgreSQL 16** | **chosen** | Forced row-level security, custom roles, per-tenant keys alongside the data, point-in-time recovery, private networking, a chosen region. Every one of those is a stated requirement, and all of them are plain PostgreSQL features |
| SQLite (server) | rejected | No network protocol, no row-level security, single-writer concurrency, no managed backup or PITR. It is the design's choice *on the device* as the spool and the wrong one here. **As built**, the device spool is not SQLite either: `endpoint/capture-spool` is an append-only encrypted segment log behind `protocol.Store` (see [endpoint/capture-spool/README.md](../endpoint/capture-spool/README.md)) |
| Supabase | rejected | A managed Postgres with an attached product surface. The requirements are Postgres features; the extra surface is procurement friction in this buyer segment, and it does not change the region or key-custody story |

**ASSUMPTION:** Flexible Server is acceptable to buyers who require customer-held keys, because the
key custody decision (master doc D6) is about Key Vault and the customer's own HSM rather than about
where the rows live.

### 2.1 Two extensions are required, both for the search index

`pg_trgm` and `btree_gin`, and nothing else. Both exist only for the content search index of §13:
`pg_trgm` for substring and fuzzy matching over attachment filenames, `btree_gin` so that a GIN index
can lead with `tenant_id`. [database/schema.sql](../database/schema.sql) creates both, and the server's
`azure.extensions` allow-list parameter names both; their availability in each target region is master
doc Q12.

Everything else runs on a stock instance. `sha256()` is built into PostgreSQL 11+ and
`gen_random_uuid()` into 13+, so the audit hash chain and every surrogate key need no extension, which
removes the `pgcrypto` question from master doc Q12 entirely. `pg_partman` would only matter if
partitioning were introduced (§11), and it is not in the v1 path.

### 2.2 Instance sizing

| Setting | v1 | Note |
|---|---|---|
| SKU | `D2ds_v5` general purpose, zone-redundant HA | The database is the system of record. `B2s` is cheaper and adequate on throughput, and is rejected because a burstable instance credits its way into trouble under a fleet-wide backfill |
| Storage | 128 GB, autogrow on | Four years of a full tenant is under 40 GB of events; the headroom is for indices, bloat between vacuums, and `ingest.rejected` |
| PITR | 35 days | Brief §3.4's expiry obligations are met in the live system; PITR exists for operational recovery, not as a retention mechanism |
| Connections | PgBouncer in transaction mode in front of the app tier | Every runtime connects through it. Long-running aggregations use a dedicated direct connection so a pool cannot be held hostage by a 20-minute rollup. **As built:** no PgBouncer is provisioned or configured anywhere in `azure/` or the local lab, and `query-api` carries its own in-process connection pool |

---

## 3. Schemas

Four schemas, and the split is deliberate:

| Schema | Contents | Mutability |
|---|---|---|
| `ref` | Data classes, classifier releases, rule metadata, route fidelity, collectors, retention classes | Shared, not tenant-scoped, no RLS |
| `ops` | Tenants (including the two enforcement gates and the collection/search ceilings), the user directory dimension, devices, credentials, collector state, policy, tools, notice acknowledgements, retention policy, holds, audit, grants, retrieval grants, content objects, finding review, erasure receipts, reconciliation, watermarks, coverage, **subscription and the usage ledger** | Mutable configuration and append-only evidence; the usage ledger is forward-written and never recomputed |
| `ingest` | `observation`, `submission`, `rejected`, and the content search index `search_text` (§13) | Immutable except whole-row retention expiry; a submission is folded in place as further routes report it (§4) |
| `mart` | Aggregates, findings, views | Derived. Droppable and rebuildable |

**The rule that shapes the split: nothing in `mart` holds human workflow state.** A finding's review
decision lives in `ops.finding_review`, keyed by the same natural key as `mart.finding`. If it lived
in `mart`, then dropping and rebuilding the derived layer — which must always be safe, and is the
whole point of calling it derived — would silently destroy an analyst's judgement about whether a
finding was confirmed or disputed.

### 3.1 Why `observation` and `submission` are separate tables

One table cannot satisfy R9 honestly. Consider a submission observed by both the browser extension
and the egress proxy:

- **One table, keyed on the event** → two rows → every count the customer sees is inflated.
- **One table, keyed on the dedup key** → the second observation is either dropped (losing the fact
  that two routes saw it, so a route's coverage cannot be reconciled) or overwrites the first
  (losing which route supplied the content).

Two tables give both facts at once. `ingest.observation` is one row per observation and is the
immutable record of what each route saw; `ingest.submission` is one row per logical submission and is
what aggregates read. A challenged count can be answered with "this is one submission, two routes saw
it, here they are, and here is why the higher-fidelity one supplied the content" — which is the
difference between a number a customer can argue with and a number they have to take on trust.

### 3.2 The dedup ladder

See [00-architecture](00-architecture.md) §5.3 for the design and
[02-ingest-and-transport](02-ingest-and-transport.md) §4 for the canonicalisation. The storage
consequences:

| Object | Rule |
|---|---|
| `ingest.submission.dedup_key` | nullable; unique among non-null values, per tenant |
| `ingest.submission.dedup_weak_key` | not null; unique **only** among rows where `dedup_key IS NULL` |
| `ingest.submission.kind` | carried here rather than joined, because every rollup needs the prompt/rollup/detection split |
| `ingest.submission.observation_count` | how many observations folded in, so a merged row is distinguishable from a single-route one |
| `ingest.submission.merge_confidence` | `low` while the row has no exact key. Brief §7 requires an undercount to be visible, and this is where the residual uncertainty lives |

Both uniqueness rules are partial indices, and the asymmetry is load-bearing. Exact keys are unique
among themselves so two confident-but-different digests stay two rows — canonicalisation divergence
is a defect that must be visible. The weak key binds only exact-key-less rows, so a weak observation
can never drag a confident one into a merge it does not belong in.

`kind` is part of the weak key because rollups and detections carry no payload size at all; without
it, a `usage_rollup` and a `model_detection` for the same tool in the same bucket would collide and
produce a record corresponding to nothing. This was found by test, not by reading: T27 in
[database/invariants.test.sql](../database/invariants.test.sql).

---

## 4. The write path

`ingest.record_event(envelope jsonb, received_at timestamptz)` is the single entry point, and it is a
database function rather than application code for one reason: brief C12 requires idempotency to be
"enforced by the store", and a rule implemented in four clients is four rules.

It performs, in one transaction:

1. **Record the observation** with `INSERT … ON CONFLICT (tenant_id, event_id) DO NOTHING`. A replayed
   batch inserts nothing. `ROW_COUNT` distinguishes a first write from a replay, which is what makes
   retries free rather than merely tolerable.
2. **Resolve retention** from `ops.retention_policy` matched on the highest-severity class present in
   the label set, falling back to the tenant mode default and then to the `standard` class.
   Materialised onto the row at write time so a later policy change cannot silently retro-apply to
   data collected under a different promise.
3. **Fold the observation into the logical submission**, applying the fidelity tie-break
   (`ref.route_fidelity.fidelity_rank`, lower is better), the weak-key adoption rule, and the field
   replacement rule: a better route replaces the content-bearing fields wholesale and never merges
   them, because a blend of two observations would correspond to neither.

### 4.1 What the caller must not do

The transport layer must not compute the dedup decision, the fidelity comparison or the retention
date. If it does, the store's guarantees become advisory. The contract is: validate, then call the
function once per envelope, then report the returned outcome.

---

## 5. Aggregates and the freshness contract

Every aggregate in `mart` is written with `INSERT … ON CONFLICT DO UPDATE` that **replaces** the
bucket. Brief C28 states the rule directly — "aggregates are upserts, never increments" — because
devices go offline and flush in bursts, so late-arriving events are the normal case. The primary keys
the upserts conflict on are in [database/schema.sql](../database/schema.sql); the statements themselves are in
[04-dashboard-and-query](04-dashboard-and-query.md) §4.

### 5.1 Why replace rather than increment

There are three reasons and the third is decisive:

1. **Idempotency.** A retried aggregation run cannot double a bucket.
2. **Late arrivals.** Recomputing a lookback window absorbs an event that arrived after its bucket
   was first computed.
3. **Erasure must be able to decrease a count.** Brief §3.4 requires subject erasure to actually
   remove data. An increment-only aggregate is structurally incapable of honouring that: deleting a
   submission would leave its contribution in every bucket it touched, and the customer's dashboard
   would keep reporting usage by a person who has been erased. A recomputing aggregate can subtract.

That third reason is why the aggregator is not just a convenience layer.

### 5.2 Buckets are cut on receive time, not device time

Brief C26 makes server time authoritative for display and retains device time for ordering and skew.
Cutting buckets on `received_at` also solves a practical problem: a device offline for three days
flushes into the *current* bucket rather than backfilling three buckets, so the lookback window never
has to grow with device downtime.

### 5.3 Freshness is a first-class output

`ops.aggregate_watermark` records the last complete bucket and the last run. Brief C27 forbids
presenting a number without the ability to say how current it is, so the freshness travels with every
aggregate response; [04-dashboard-and-query](04-dashboard-and-query.md) §13 specifies `stale_aggregate`
as an explicit result state rather than a footnote.

### 5.4 The aggregation job

| Property | Value |
|---|---|
| Runner | Azure Container Apps Job, one instance per region, scheduled every 5 minutes |
| Lookback | 7 days for day buckets, 48 hours for hour buckets, recomputed each run |
| Isolation | `sac_ops` role; sets `app.tenant_id` per tenant and iterates. It never runs with an unset tenant, because that would read zero rows and silently produce empty aggregates |
| Failure | A failed run leaves the previous bucket contents in place and advances no watermark. Partial success is impossible: each tenant's buckets commit in one transaction |

**ASSUMPTION:** a 5-minute cadence satisfies brief §8's "event visible in the query layer < 60 s" for
the event list, with aggregates lagging by up to one cadence and always reporting their own freshness.
The brief's target is met by the direct `ingest.submission` read path, not by the aggregates, and the
freshness field is what keeps the two honest.

---

## 6. Retention: two independent mechanisms, reconciled

Brief C34: "Time-based expiry for events and for content, enforced by at least two independent
mechanisms that are periodically reconciled against each other. Do not trust a single deletion path."

Mechanisms 1 and 2 below are that pair. Mechanism 3 is key destruction, which is not time-based expiry
but is reconciled alongside them.

| # | Mechanism | Applies to | How it works | Who runs it |
|---|---|---|---|---|
| 1 | Row expiry | `ingest.observation`, `ingest.submission`, `ingest.rejected` | Delete rows where `expires_at < now()`, skipping any covered by an active hold. Sets `sac.retention_delete = on` for the transaction, which is the only way past the append-only trigger | `reconciler` job (Go), `sac_ops` role |
| 2 | Blob lifecycle | Content ciphertext in Azure Blob Storage | An Azure lifecycle management rule deletes blobs by age **without consulting the database at all** | The storage account |
| 3 | Key destruction | Content objects whose tenant is offboarded or whose data is subject-erased | In one statement, sets `ops.content_object.state = 'shredded'` with its reason and timestamp and overwrites `wrapped_dek` with a single zero byte — the column is `NOT NULL`, and a value that can open nothing is the stored form of "the key is gone". The ciphertext becomes undecryptable even if it still exists | `content-vault` (`POST /v1/content/shred`), `sac_vault` role |

Mechanism 2 is independent by construction: it is a rule in the storage account that does not know or
care what the database thinks. That is exactly what makes it a useful control and exactly why it must
be reconciled — the two can disagree in either direction.

### 6.1 Reconciliation

`ops.reconciliation_run` records each run and its findings; drift is **recorded and alerted, never
auto-corrected**. Six checks:

| Check | Drift means |
|---|---|
| Blobs past `expires_at` still present | Mechanism 2 has not run, or the lifecycle rule is misconfigured |
| `ops.content_object` rows whose blob is absent | Mechanism 2 ran ahead of mechanism 1, or a blob was deleted out of band |
| `state = 'shredded'` rows whose blob still exists | The key was destroyed but the ciphertext was not removed. Not a confidentiality failure, but the receipt claimed removal |
| A usable `wrapped_dek` (anything but the zero-byte tombstone) on a row past `expires_at` | Mechanism 3 did not run for that object |
| **Observations whose submission no longer exists** | A deletion path removed one side of a submission and not the other. `ingest.search_text` cascades from the submission, but `ingest.observation` has **no foreign key** to it — the observation is written first and is immutable, so the link cannot be set at insert time. This check is what stands in for the constraint that cannot exist, and it must be able to fail rather than being a formality |
| `observation_count` disagreeing with the row count in `ingest.observation` | Two concurrent retries of the same `event_id`: the loser of the insert race may still have incremented the counter. The value is derived and the reconciler recomputes it, which is precisely why it is a reporting number and never a count of submissions |

Auto-correction would be the wrong instinct here. A systematic misconfiguration should surface once
and loudly, not be silently repaired every night while the underlying fault persists.

### 6.2 Holds

`ops.hold` has a structured `scope` (tool, user, class or time range), an actor, and a mandatory
`expires_at` — mandatory precisely so a hold cannot become permanent by omission. The reconciler skips
held rows in mechanism 1 and records them in the erasure receipt's `remaining_counts`, so an erasure
that was partially suspended says so instead of reporting a clean total.

---

## 7. Erasure

Master doc D1 draws the line, and it is worth restating because it is a deliberate departure from the
common design:

**Events are deleted, not crypto-shredded.** At 4.4M events/year a subject's events number in the
hundreds to low thousands. Deleting them is cheap and produces a *literal* receipt — "these 1,247
rows were removed" — which is far more usable to a customer than "a key was destroyed". Cryptography
is the right tool when data is too large or too replicated to delete; here it is neither.

**Content is destroyed by key, and also deleted.** Both, because either alone leaves a gap: deleting
the ciphertext without destroying the wrapped key leaves the key sitting in a database backup, and
destroying the key without deleting the ciphertext leaves an object that the receipt cannot honestly
call removed.

**Tenant offboarding destroys the tenant key.** That is the one place where key destruction is the
*primary* mechanism, because it is the only practical way to render an entire tenant's content
unreadable at once.

### 7.1 The receipt

`ops.erasure_receipt` records scope, actor, request time, completion time, the mechanisms that ran,
`removed_counts` per table and blob prefix, and `remaining_counts` for what deliberately survived.

The receipt carries a hash. It does not carry an enumeration of every object id: at the volumes in
brief §3.1 a subject's erasure touches hundreds of rows and tens of objects, and the receipt records
counts by mechanism with the reconciliation run that verified them. **ASSUMPTION:** counts plus
verification are sufficient evidence; if a customer requires per-object enumeration, the export path
in [04-dashboard-and-query](04-dashboard-and-query.md) §9 is where it belongs. This is master doc Q11's
neighbour and it is flagged rather than decided.

### 7.2 What erasure cannot reach

Two honest limitations, both surfaced in the receipt rather than hidden:

- **Backups.** PITR snapshots are not selectively editable. The promise is: immediate in the live
  system, complete within the backup window, and the erasure ledger is replayed onto any restored
  database *before it is promoted*. A restore that skipped that step would resurrect erased data, so
  it is a runbook gate, not a note.
- **The audit log.** `ops.audit` is append-only by three mechanisms, and an erasure request is itself
  an auditable act. Audit rows therefore survive with a pseudonymous `subject_ref` and no content.
  This is recorded in `remaining_counts` with its reason, because a receipt that claimed total removal
  while the audit log retained the subject reference would be false.

---

## 8. The content store

| Property | Value |
|---|---|
| Service | Azure Blob Storage, a container per residency region |
| Content | Ciphertext only. Never plaintext, at any point, at any tier |
| Key | Per-object AES-256-GCM data key, generated by `content-vault` on grant approval |
| Key wrapping | The data key is wrapped by the per-tenant KEK in Key Vault (or the customer's vault) and only the wrapped form is stored, in `ops.content_object.wrapped_dek` |
| Naming | `{tenant}/{yyyy}/{mm}/{object_id}` — tenant-first, so a prefix-level operation can never cross a tenant boundary |
| Access | No shared access signature is ever issued to a browser. The upload SAS is single-object, short-lived and issued to the device by `control-api`; retrieval is streamed by `content-vault` after approval |
| Lifecycle | Hot for the retention class, then delete. Mechanism 2 of §6 |
| Redundancy | ZRS in-region; no cross-region replication by default, because brief §3.3's residency requirement and a replication decision are the same decision |

### 8.1 Where the plaintext exists

A short and complete list, because this is the question a security reviewer asks first:

1. **On the device**, in the process that observed it, for as long as it takes to classify it.
2. **On the device**, in the classifier host's memory, during classification.
3. **In the document parser child process**, for the duration of parsing an attachment.
4. **Nowhere else.** Not in `ingest-api` (it never sees content), not in `query-api` (it cannot see
   even a wrapped key), not in Blob Storage, not in a log, not in `ingest.rejected`.

`content-vault` is the only component that holds unwrap rights, and it has internal-only ingress: it
is not reachable from a device or a browser. Master doc D7 is the reason the service boundary is drawn
where it is.

---

## 9. Backup, restore and disaster recovery

| Property | Target | Reasoning |
|---|---|---|
| Backup | Automated, 35-day PITR, geo-redundant backup storage | |
| RPO, in-region | ≤ 5 minutes | PITR granularity; events are also re-sendable from device spools, so the practical RPO is better than the database's |
| RTO, in-region | 60 minutes | Failover to the standby |
| RPO, cross-region | ≤ 15 minutes | Geo-restore from the latest available backup |
| RTO, cross-region | 4 hours **as a target the drill must earn** | Not a promise until a rehearsal demonstrates it. [05-platform-delivery](05-platform-delivery.md) §14.3 forbids quoting it before then |

### 9.1 What is recoverable and what is not

This distinction is a product promise and must be stated precisely:

- **Event metadata, labels, findings, aggregates, audit** — recoverable to the RPO.
- **Content whose per-tenant key has been destroyed** — **never recoverable, by design.** Master doc
  D6 makes key destruction the mechanism behind customer-held keys, and a mechanism that could be
  undone by restoring a backup would be worthless.
- **Local-only content on a device that is wiped** — gone. `ingest.submission.content_state` says
  `local_only`, which is the system correctly reporting that it never held the content. **As built:**
  nothing sets `local_only` — every submission is inserted as `not_captured` and no service writes the
  column — so such a row reads `not_captured` today.

### 9.2 The restore gate

A restore is not complete when the database is up. Before promotion:

1. Restore to a point in time.
2. **Replay the erasure ledger** — every `ops.erasure_receipt` completed after the restore point is
   re-applied, and the replay is itself recorded as a reconciliation run.
3. Verify the audit hash chain still validates from the last anchored digest.
4. Only then promote.

Skipping step 2 is how a system that deletes data correctly ends up restoring it.

---

## 10. Verification

A schema that has only been read is a claim. Two artefacts make this one checkable:

```
psql -v ON_ERROR_STOP=1 -f database/schema.sql
psql -v ON_ERROR_STOP=1 -f database/invariants.test.sql
```

[database/invariants.test.sql](../database/invariants.test.sql) runs as the runtime roles, not as a superuser,
because a superuser bypasses row-level security and would therefore prove nothing about it. The
recorded run was against PostgreSQL 17, the local lab's version. The 47 assertions cover:

| Group | What it proves |
|---|---|
| T1–T4 | A policy bundle exceeding the tenant ceiling, containing an unrecognised mode, or specifying no scope at all is refused. Brief C3 as a database invariant rather than a validation the configuration API is trusted to perform |
| T5–T8 | A replayed event is a duplicate; two routes observing one submission produce two observations and one submission; the higher-fidelity route takes over the content fields |
| T9 | Retention is materialised from `ops.retention_policy` at write time |
| T25–T27 | The dedup ladder: two M0 routes collapse and are flagged low-confidence; an exact observation adopts a weak-only row instead of double-counting; a rollup and a detection stay separate |
| T10–T11 | The mode boundary: an M0 row carrying a content digest is refused, and an M1 prompt without classifier output is refused |
| T12–T15 | Tenant isolation: a second tenant sees zero rows, an unset tenant sees zero rows, and a cross-tenant write is refused by the row-level security policy |
| T16–T18 | The audit hash chain links consecutive rows, and both UPDATE and DELETE are refused |
| T19–T21 | Observations cannot be updated in place or deleted outside the retention path; quarantine refuses stored content |
| T22–T24 | A shredded object requires a reason; an unattributed sanction decision is refused; a newly discovered tool defaults to `unknown`, not `unsanctioned` |
| T28–T32 | Content search: `full_text` with customer-held keys is refused; a search tier without the collection ceiling it needs is refused; index rows are written and matched; erasing a submission removes its index entries in the same transaction |
| T33–T35 | Commercial state: a gate closure without attribution is refused; the usage ledger is unchanged by an erasure; the suspension-impact view reports enrolled devices and spooled events |
| T36–T38 | An equal-rank exact observation adopts a weak-only row and the adopted row is no longer flagged low; the quarantine reason vocabulary is pinned |
| T39–T43 | Each kind refuses every field the contract forbids it and still accepts a valid record; an M0 record refuses every content-derived field, including `confidence` |
| T44–T47 | A retrieval grant is single-use, whole and not self-approved; grant and classifier-release digests are lowercase sha256 or refused |

What these tests do **not** prove: performance at scale, behaviour under concurrency beyond the
advisory lock in the audit chain, and the plpgsql bodies of functions that no test exercises. The
limitations are listed here rather than left implied.

---

## 11. Capacity and the partitioning trigger

Master doc D2 records the decision and **ADR 0009** records it as a deliberate deviation from a
literal reading of C32:

- **No partitioning in v1.** Tenant is the leading column of every primary key and every index, so
  isolation and access paths are already tenant-first, but there are no partitions to speak of.
- **The trigger:** a single tenant exceeding **50M event rows**, roughly eleven years at full scale or
  a 30× larger customer.
- **The migration:** monthly range partitioning on `received_at`, introduced without an application
  change because every access path is already expressed through a tenant-leading key. This is what
  the two tables and the explicit `expires_at` column were chosen to make possible.

At that point partitioning also becomes the third retention mechanism — detach instead of delete —
which strengthens rather than replaces §6.

---

## 12. What is deliberately absent

| Absent | Why |
|---|---|
| A data warehouse | 5–9 GB/year. The aggregates live in the same database as the facts, and the customer's own SQL goes to the export path, not to our warehouse |
| A separate search cluster | Prompt text is ~4.4 GB/year per tenant. A second copy of plaintext content in another system would widen the breach surface, add a second retention path to reconcile, and cost more than the rest of the tenant combined. The index is `ingest.search_text` in this database; see §13 |
| A message broker | Aggregation is a scheduled set-based recompute (C28), not a stream. A broker would add a component, a failure mode and a consistency question to solve a problem that recomputation already solves |
| Attachment **content** in the index | `ingest.search_text` holds prompt bodies and attachment **filenames**. Indexing attachment bytes is the 50–500 GB/year tier in brief §3.1, with its own cost and breach surface, and it is a separate decision (ADR 0014) |
| A second copy of the envelope as `jsonb` | The wire contract is closed (`additionalProperties: false`), so every field is a real column — with one exception, `attachments`, whose store-side home is `ingest.search_text` — and a blob copy would only be a way for the two to disagree |
| Partitioning, row-level compression tuning, and columnar storage | Nothing at this volume justifies them; see §11 for the trigger that changes that |

---

## 13. The content search index

Content search is a required product capability ([ADR 0014](adr/0014-content-search-is-a-per-tenant-capability.md)).
It is stored here rather than in a search cluster, for the same reason there is no warehouse: at this
volume a second system would cost more, widen the breach surface, and add a retention path to reconcile.

### 13.1 What is indexed, and for whom

| `ops.tenant.content_search` | `prompt_body` rows | `attachment_name` rows |
|---|---|---|
| `disabled` | none | none |
| `attachment_names` | none | yes |
| `full_text` | yes | yes |

The tenant value is a **ceiling**; the signed policy bundle narrows it per scope (tool, data class, user
population), so enabling `full_text` for a tenant does not enable it for every tool that tenant uses.
This is the same ceiling-and-narrow pattern as the collection mode, and it is what keeps brief C5's
"must not be possible to configure an upload-everything state" meaningful.

### 13.2 The table

`ingest.search_text`, one row per searchable unit, keyed
`(tenant_id, submission_id, unit_kind, unit_index)`:

| Column | Note |
|---|---|
| `unit_kind` | `prompt_body` or `attachment_name` |
| `unit_index` | 0 for the prompt body; 0..n for each attachment filename |
| `body` | 1–65536 characters |
| `tsv` | **Generated column**, `to_tsvector('simple'::regconfig, body)`. The two-argument form is the only IMMUTABLE one, so it is the only form legal in a generated column — the one-argument form depends on `default_text_search_config` and would make the stored vector depend on session state. `simple` rather than a language config because the product is multi-lingual and stemming the wrong language is worse than not stemming |
| `expires_at` | Swept by the ordinary retention path |

Two GIN indexes, both leading with `tenant_id` per brief C32 — which requires `btree_gin`, because a GIN
index over `tsvector` cannot otherwise carry a uuid column. The trigram index is **partial on
`unit_kind = 'attachment_name'`**: full-text search is the right tool for prose, trigrams are the right
tool for "find the file called something like Q3-contract", and a trigram index over every prompt body
would be large for no benefit.

### 13.3 Who writes and reads it

**`content-vault` does both.** It writes index rows when it stores content, and it executes searches when
`query-api` asks. `query-api` is **not** granted `SELECT` on this table.

That is deliberate. The alternative — letting the query tier read an index of plaintext prompt content —
would quietly retire the invariant that exactly one component can read content, which is the property the
whole content tier is built around. Search is a content read, so it belongs where content reads live.

### 13.4 Retention, erasure and the receipt

The index dies with the row it describes: the foreign key to `ingest.submission` cascades, so a subject
erasure or a retention expiry that removes a submission removes its index entries **in the same
transaction**. There is no second deletion path that could be forgotten or drift out of step.

This matters more than it looks. An index that outlived its content would be two failures at once: a
search result the analyst cannot open, and — worse — terms from erased content still sitting in a
searchable structure after the customer has been given a deletion receipt. The cascade makes that
structurally impossible rather than a thing someone has to remember. The receipt must still count these
rows, so the reconciler's per-table counts include `ingest.search_text`.

### 13.5 Volume and cost

| Quantity | Value |
|---|---|
| Prompt bodies | ~4.4M/year × ~1 KB ≈ **4.4 GB/year** per full tenant |
| `tsvector` | roughly 30–50% of the text |
| GIN index | comparable to the vector |
| Attachment **names** | negligible; 255 bytes maximum each |

So a `full_text` tenant adds on the order of 10–15 GB/year to a database that already holds 5–9 GB/year
of events. That is a real increase and it belongs in the capacity model, but it is the same order of
magnitude as what is there, and it is why no separate system is warranted.

### 13.6 The property this gives up, recorded in the data platform document

`tsv` is derived from plaintext and is therefore itself readable by anyone who can read the table. **A
search index over content is a second copy of that content's terms.** There is no cryptographic
construction that provides search without leakage, and this design does not pretend otherwise.

Two consequences belong here rather than only in the security document, because they are storage
decisions:

1. The index is inside the backup and point-in-time-recovery scope, so erasure completeness depends on
   the erasure-ledger replay described in §9.2 — the same mechanism that covers the event tables.
2. It is inside the breach-assessment scope. An incident that exposes `ingest.search_text` exposes terms
   from prompt content even if the content objects themselves were never decrypted.

---

## 14. Commercial state: the usage ledger

The product is operated by the vendor as a multi-tenant service and sold monthly on a flat per-tenant
plus per-enrolled-device basis ([ADR 0015](adr/0015-suspension-is-a-human-decision-and-usage-is-metered-forward.md)).
Two tables carry that, and one of them is the only table in this design that **must never be recomputed
from source**.

### 14.1 `ops.usage_daily` — written forward, never derived

One row per tenant per day: `events_accepted`, `content_bytes_added`, `devices_enrolled`,
`devices_reporting`, `snapshot_at`. It holds **no subject reference and no device identifier**, so it is
not personal data and is outside the scope of a subject erasure.

The rule that matters is the one stated on the table: **the counters are never recomputed.** This is not
tidiness, it is the resolution of a direct conflict between two requirements:

| Requirement | Wants |
|---|---|
| Brief §3.4 — subject erasure removes data, and §5.1 above makes aggregates *replace* their bucket so a count can fall | Stored counts go **down** when a subject is erased |
| The invoice | The bill must **not** move when a subject is erased — the tenant consumed the service |

A usage figure derived from `ingest.submission` would silently understate the invoice after every
erasure, and nobody would notice until a customer asked why their usage graph had a step in it. Worse, a
tenant could reduce their bill by exercising erasure rights.

So `ingest-api` increments the ledger in the same transaction that accepts the events. A retried batch
adds nothing (duplicates are rejected before the counter moves); an erasure subtracts nothing.

**As built:** that increment is not implemented. The table, its constraints and the grants that would
let `sac_ingest` write it exist, but `ingest-api` issues no statement against `ops.usage_daily` and
`ingest.record_event()` does not touch it, so the ledger is empty until a row is inserted by hand. T34
inserts its own ledger row for exactly that reason.

Test T34
in [invariants.test.sql](../database/invariants.test.sql) asserts exactly this: it erases every submission and
observation for a tenant and confirms the ledger is unchanged.

`content_bytes_added` is recorded even though nothing bills on it, because content is the only unbounded
cost in the system (brief §3.1) and this is how a flat price is discovered to be unprofitable for a
particular tenant. It carries no content and no digest — only a byte count.

### 14.2 Why device counts are a snapshot, not a live read

`devices_enrolled` is taken at end of day from `ops.device`, and `devices_reporting` beside it. Reading
`ops.device` live would make "how many devices in March" a question whose answer changes in April when a
device is removed. The snapshot also keeps two different facts apart, which the dashboard then cannot
merge: **enrolled** means covered and billable, **reporting** means heard from recently. A device that
has gone quiet is still enrolled.

### 14.3 `ops.subscription`

Plan, `billing_basis` as data (`tenant`, `device`, and optionally `events` or `content_bytes`), the
period anchor, and `billed_through`. No price: the system produces billable usage and invoicing happens
elsewhere. `billed_through` freezes a period once invoiced, because a month that has been billed must
not change because a device was removed afterwards.

### 14.4 What this costs the rest of the design

- One extra row write per batch on the ingest path, in the same transaction. A failure there must fail
  the batch rather than silently lose a billing fact.
- A nightly job to take the device snapshot, and a period-close job to advance `billed_through`.
- The ledger is inside the backup and recovery scope like everything else, and its daily rows are
  immutable once written — so a restore that predates them must not be allowed to lose them. It is in
  the erasure-ledger replay's *exclusion* list rather than its target list: erasure never applies to it.
