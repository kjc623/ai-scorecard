# `docs/` — the design record

The documents that describe what this system is and why it is built this way. Code explains *how*;
this is where the *why* lives, including the alternatives that were rejected.

## The subsystem documents

Read in order, or jump to what you need. Each is a long document with numbered sections, and other
READMEs in this repository link into them by section rather than restating them.

| Document | What it answers |
|---|---|
| [`00-architecture.md`](00-architecture.md) | **The master document.** The problem, the constraints with their sources, three structural alternatives, the recommendation, the main and failure paths, the interfaces, the build order, the open questions and the risk register. Start here |
| [`01-collectors.md`](01-collectors.md) | How nine usage modes get captured on three operating systems, and the device-side components that do it |
| [`02-ingest-and-transport.md`](02-ingest-and-transport.md) | How data reaches the cloud. Contains the **normative dedup specification** in §4 — the tie-break rules other components are implemented against |
| [`03-data-platform.md`](03-data-platform.md) | The data model, the two independent retention mechanisms and their reconciliation, erasure and receipts, backup and DR |
| [`04-dashboard-and-query.md`](04-dashboard-and-query.md) | How the ten questions become queries: the closed DSL, aggregates, audit-on-read, k-suppression, cursor pagination |
| [`05-platform-delivery.md`](05-platform-delivery.md) | Deployment, updates and operations: the Azure inventory, CI/CD, signing, update rings, SLOs and the cost model |
| [`06-security-and-threat-model.md`](06-security-and-threat-model.md) | The trust model, the key hierarchy, STRIDE, abuse cases and residual risk |

## The decision records

[`adr/`](adr/) holds 19 architecture decision records, each stating a decision, the alternatives, and
what would change its mind. **Its [`README.md`](adr/README.md) is the index** — the fastest way to
find out why a specific choice was made. When two documents here disagree, the ADR is the decision
and the subsystem document is the explanation.

## Supporting work

| Directory | What it holds |
|---|---|
| [`lab/`](lab/) | What a zero-cost local lab can and cannot prove, and its cost model. Read alongside [`../localdev/`](../localdev/) |
| [`risks/`](risks/) | The two brief risks that were closed by measurement rather than by design: local-inference capture (R1) and the organisational dimension (Q2) |

`risks/R1-harness/` is a **runnable Go harness**, not prose: it is the instrument that produced the R1
finding, kept so the measurement can be repeated.

## Two things to know before quoting this package

**Everything is `proposed`.** No ADR here has been reviewed by anyone outside this package, and none
has a second author.

**This is not a legal position.** The brief explicitly excludes legal, privacy-policy and
jurisdictional review, and `06-security-and-threat-model.md` §11 states only what the system can
*attest*, not what the law requires. Where a decision rests on something the brief does not say it is
labelled **ASSUMPTION** and listed as an open question with an owner and a way to close it.

## Cost figures

All figures are estimates at Azure East US list price, pay-as-you-go, as of 2 October 2026, and must
be re-baselined before any commercial commitment. A contradiction inside the model — whether Front
Door's base cost is per region or per tenant — is recorded rather than resolved in
[`../azure/COST-FINDING.md`](../azure/COST-FINDING.md), because the resolution is a product decision.
