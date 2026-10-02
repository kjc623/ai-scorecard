# Shadow AI Capture — Architecture Design Package

**Status:** proposed, not built · **Cloud:** Microsoft Azure · **Data class:** regulated personal data
(GDPR/CCPA) · **Derived from:** `engineering-brief.md` (Shadow AI Capture — Product Requirements &
Engineering Context)

Shadow AI Capture records what employees send to generative AI tools from company-managed devices, and
makes it queryable. The customers are 500–5,000-employee companies that have *not* bought enterprise AI,
so there is no vendor-side API to pull from: if the system does not observe the usage on the device, the
data does not exist anywhere.

By default, content is interpreted where it is observed and **does not leave the machine**:
classification, a digest and dimensions cross the network, and content crosses only on an explicit
per-event grant from the backend. A tenant can trade that property for **content search** — `full_text`
search over prompt text, which requires the vendor to be able to read it — and the trade is made per
tenant and recorded in the database rather than assumed ([ADR 0014](docs/adr/0014-content-search-is-a-per-tenant-capability.md)).

That property is not a privacy feature bolted onto a monitoring system. It is what keeps the breach
surface small enough to sell, and it constrains almost every decision in this package.

---

## Start here

| If you want to know… | Read |
| --- | --- |
| The whole design, the alternatives and why this one won | [docs/00-architecture.md](docs/00-architecture.md) |
| What each piece is written in, and why | [00-architecture.md §4.1](docs/00-architecture.md) |
| Which database, and why not SQLite or Supabase | [ADR 0002](docs/adr/0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md) |
| How nine usage modes get captured on two operating systems | [docs/01-collectors.md](docs/01-collectors.md) |
| How data reaches the cloud, and how content crosses only when granted | [docs/02-ingest-and-transport.md](docs/02-ingest-and-transport.md) |
| The data model, retention, erasure and recovery | [docs/03-data-platform.md](docs/03-data-platform.md) |
| How the ten questions become queries | [docs/04-dashboard-and-query.md](docs/04-dashboard-and-query.md) |
| How it deploys, updates and is kept running | [docs/05-platform-delivery.md](docs/05-platform-delivery.md) |
| The trust model, keys, STRIDE and residual risk | [docs/06-security-and-threat-model.md](docs/06-security-and-threat-model.md) |
| The wire contract | [contracts/event-envelope.schema.json](contracts/event-envelope.schema.json) |
| The database | [db/schema.sql](db/schema.sql) |
| What to build first | [00-architecture.md §6](docs/00-architecture.md) |
| What is still unknown | [00-architecture.md §7](docs/00-architecture.md) |

## Documents

| File | Contents |
| --- | --- |
| [docs/00-architecture.md](docs/00-architecture.md) | **Master document.** Problem, constraints with sources, three structural alternatives, recommendation, main and failure paths, interfaces, data model, build order, open questions, risk register |
| [docs/01-collectors.md](docs/01-collectors.md) | The nine usage modes mapped to providers; capture-core, capture-extension and the classifier host; the egress proxy, loopback broker, process detector and CLI shim; mode enforcement; the spool; Windows and macOS; coverage measurement |
| [docs/02-ingest-and-transport.md](docs/02-ingest-and-transport.md) | Device authentication; the **normative dedup specification** in §4; the five device APIs; the write path; rejection reason codes; the health channel; grant issuance and content upload; retrieval; wire-level failure catalogue |
| [docs/03-data-platform.md](docs/03-data-platform.md) | Storage decision and sizing; schemas; the dedup ladder in storage; the write path; aggregates and freshness; **two independent retention mechanisms and their reconciliation**; erasure and receipts; the content store and key hierarchy; backup, restore and DR; verification; the partitioning trigger |
| [docs/04-dashboard-and-query.md](docs/04-dashboard-and-query.md) | The ten questions mapped to read paths; aggregates as replace-the-bucket upserts; audit-on-read; k-suppression; cursor pagination; the export path; query safety; error semantics |
| [docs/05-platform-delivery.md](docs/05-platform-delivery.md) | Azure resource inventory; environments and Bicep; CI/CD; identity and secrets; endpoint distribution; signing and notarisation; the reputation bake period; update safety and rings; SLOs; **cost model**; DR; runbooks; go-live gates |
| [docs/06-security-and-threat-model.md](docs/06-security-and-threat-model.md) | Trust boundaries; ranked asset inventory; identity; cryptography; **the key model decision**; tenant isolation; STRIDE; the interceptor as a liability; abuse cases; what the legal workstream needs; residual risks; security invariants |
| [contracts/event-envelope.schema.json](contracts/event-envelope.schema.json) | The wire contract (JSON Schema 2020-12). Generates the TypeScript and Go types |
| [db/schema.sql](db/schema.sql) | PostgreSQL 16 DDL: 34 tables, 3 views, 28 row-level security policies, roles, triggers, functions, seed data |
| [db/invariants.test.sql](db/invariants.test.sql) | 38 assertions that the schema's properties actually hold |
| [docs/adr/](docs/adr/) | 15 architecture decision records, one of which (0014) supersedes another |

---

## The design in one page

```
 MANAGED DEVICE (treated as untrusted)
 ┌──────────────────────────────────────────────────────────────────────┐
 │  capture-extension (TypeScript, MV3)   capture-core (Go, service)    │
 │  · webRequest + requestBody            · egress proxy provider       │
 │  · page-context attachment read        · loopback inference broker   │
 │  · inline warn / block                 · process & model detector    │
 │  · wide observation, narrow emission   · CLI trust shim              │
 │         │                              · policy engine               │
 │         │ native messaging             · encrypted spool (bounded)   │
 │         └──────────────┬───────────────┘                             │
 │                        ▼                                             │
 │           classifier-host (Rust → native and wasm32)                 │
 │           rules → validators → model; document parsing in a child    │
 └──────────────────────────────────────────────────────────────────────┘
                    │  one envelope · one spool · one policy
                    │  HTTPS 443 · per-device credential + mTLS binding
                    ▼
          Azure Front Door Premium + WAF
                    │ Private Link
        ┌───────────┴────────────┐
        ▼                        ▼
   ingest-api (Go)         control-api (Go)
   events only, no keys    enrolment · policy · grants · health
        │                        │
        │                        ▼
        │                  content-vault (Go, internal ingress only)
        │                  the only component that can unwrap
        └────────┬───────────────┘
                 ▼
   Azure Database for PostgreSQL 16      Azure Blob (ciphertext at M3)
   ref · ops · ingest · mart             Azure Key Vault / Managed HSM
                 │
                 ▼
        aggregator (job) ──► mart aggregates
                 │
                 ▼
           query-api (TypeScript) ──► dashboard (React)
```

**Seven properties hold the design together:**

1. **Content stays put by default.** The only content-egress path is a per-event grant, so "upload
   everything" is structurally unreachable rather than merely forbidden ([ADR 0008](docs/adr/0008-no-server-side-content-search-in-any-key-mode.md), brief §1.1).
2. **One write path.** Collectors hold no database credential; `ingest.record_event()` owns idempotency,
   the dedup tie-break and retention ([ADR 0001](docs/adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md)).
3. **One read path.** The browser never speaks SQL; a closed DSL compiles to parameterised queries
   ([ADR 0003](docs/adr/0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md)).
4. **Observations are immutable.** History cannot be rewritten, by anyone short of a superuser
   ([ADR 0004](docs/adr/0004-observations-are-immutable-and-the-closed-envelope-is-the-record.md)).
5. **Isolation is structural.** Forced row-level security, tenant-leading keys, and per-tenant content
   keys as an independent second layer.
6. **Content search is a per-tenant capability.** `disabled`, `attachment_names`, or `full_text`; the
   last requires vendor-readable content, and brief §3.5's mutually exclusive pair — customer-held keys
   with server-side content search — is a database check constraint rather than a policy note
   ([ADR 0014](docs/adr/0014-content-search-is-a-per-tenant-capability.md)).
7. **Coverage is honest.** Every collection path reports its own health; a path that is not working
   appears as `degraded` or `absent`, never as an absence of data (brief §7, R11).

## Decisions at a glance

| Component | Choice | Record |
| --- | --- | --- |
| Browser extension | TypeScript, Manifest V3, Chromium (Chrome + Edge) | [01](docs/01-collectors.md) |
| Desktop capture core | Go, one static binary per platform, MSI/PKG via Intune/Jamf | [01](docs/01-collectors.md) |
| Classifier | Rust, one source compiled to native **and** `wasm32` | [01](docs/01-collectors.md) §9 |
| Ingest API | Go on Azure Container Apps | [ADR 0001](docs/adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md) |
| Content vault | Go, internal ingress only | [ADR 0006](docs/adr/0006-content-is-ciphertext-under-per-object-keys-wrapped-by-a-per-tenant-key.md) |
| Query API | TypeScript / Node on Azure Container Apps | [ADR 0003](docs/adr/0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md) |
| Dashboard | TypeScript + React, static | [04](docs/04-dashboard-and-query.md) |
| Database | PostgreSQL 16, Azure Flexible Server — **not** SQLite, **not** Supabase | [ADR 0002](docs/adr/0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md) |
| Content store | Azure Blob, ciphertext only, per-object key wrapped by a per-tenant key | [ADR 0006](docs/adr/0006-content-is-ciphertext-under-per-object-keys-wrapped-by-a-per-tenant-key.md) |
| Device spool | SQLite (WAL), application-level encryption, bounded | [01](docs/01-collectors.md) §12 |
| Infrastructure | Bicep | [05](docs/05-platform-delivery.md) |
| Idempotency | Two-tier dedup enforced by partial unique indices | [02](docs/02-ingest-and-transport.md) §4 |
| Erasure | Delete events; destroy content keys and ciphertext; receipt states what survived | [ADR 0012](docs/adr/0012-erasure-deletes-events-and-destroys-content-keys.md) |
| Content search | Per-tenant: `disabled` / `attachment_names` / `full_text`; index in PostgreSQL, searched by `content-vault` | [ADR 0014](docs/adr/0014-content-search-is-a-per-tenant-capability.md) |
| Suspension | Two gates (`ingest_enabled`, `read_enabled`), human-decided, attributed, with impact shown before acting | [ADR 0015](docs/adr/0015-suspension-is-a-human-decision-and-usage-is-metered-forward.md) |
| Billing metering | Forward-written daily ledger, counts only, independent of erasure; basis held as data | [ADR 0015](docs/adr/0015-suspension-is-a-human-decision-and-usage-is-metered-forward.md) |

---

## How to verify this package

The schema is not a claim. It was executed against a real PostgreSQL server, and its properties are
asserted as the runtime roles rather than as a superuser — a superuser bypasses row-level security and
would therefore prove nothing about it:

```
psql -v ON_ERROR_STOP=1 -f db/schema.sql
psql -v ON_ERROR_STOP=1 -f db/invariants.test.sql
```

The 38 assertions cover the collection-mode boundary, tenant isolation (including fail-closed behaviour
with no tenant set), the two-tier dedup ladder, the policy ceiling, the audit hash chain, append-only
enforcement, retention materialisation, and the states brief §3.2 says must never be merged.

## Traceability

Every constraint in [00-architecture.md §2](docs/00-architecture.md) cites the brief section it comes
from. Where a decision rests on something the brief does not say, it is labelled **ASSUMPTION** and
listed in §7 as an open question with an owner and a way to close it. Nothing in this package should be
read as a legal position: the brief explicitly excludes legal, privacy-policy and jurisdictional review,
and [06-security-and-threat-model.md](docs/06-security-and-threat-model.md) §11 states only what the
system can *attest*, not what the law requires.

## Known state and what is not settled

- **This is a design package, not a built system.** No component exists.
- **All cost figures are estimates** at Azure East US list price, pay-as-you-go, as of 2 October 2026,
  and must be re-baselined before any commercial commitment. The recommended production configuration is
  ≈$830–840 per tenant per month, not the $300–700 an earlier draft of the master document assumed; the
  correction is recorded in [00-architecture.md §1.4](docs/00-architecture.md) rather than left as a
  discrepancy between documents.
- **Three brief risks are on the critical path and are validation tasks, not design tasks:** R1
  (local-inference capture, which gates build step 8 only), Q1 (residency, which the brief never
  mentions), and Q2 (the organisational dimension, without which three of the ten headline questions
  cannot be answered).
- **Two items the legal workstream owns** are represented here only as capabilities: the notice
  acknowledgement record and the per-tenant collection ceiling.
- **Everything is `proposed`.** No ADR has been reviewed by anyone outside this package, and none has a
  second author.

## A note on `archive/`

`archive/pre-brief-generic-collector/` holds a previous design package produced before the brief was
available. It describes a different product — a generic employee-activity monitor — and was built on an
invented domain rather than a stated one. It is retained for comparison, not as part of this design. Do
not read anything in it as current.
