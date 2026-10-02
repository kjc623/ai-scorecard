# 0014. Content search is a per-tenant capability; the key model is chosen with it, not against it

Status: proposed
Date: 2026-10-02

Supersedes: [ADR 0008](0008-no-server-side-content-search-in-any-key-mode.md), which refused
server-side content search in every key mode. Reversed on customer requirement.

## Context

ADR 0008 chose customer-held keys permanently and therefore refused content search entirely. That was
a defensible reading of brief §3.5 in isolation, and it was wrong as a product decision: **customers
require the ability to search prompt content and attachment names.** Customer requirements supersede
internal decision records, and this record exists so the reversal is documented rather than quietly
edited out of the schema.

Brief §3.5 does not actually say "choose keys". It says:

> Customer-held keys and server-side full-text search over content cannot both exist. […] Decide which
> side of that line the product sits on *before* anyone builds a content search feature, because it is
> not a feature that can be added later without changing the key model.

And brief R10 frames the same thing as a risk to resolve, with the resolution being "decide the key
model and the search promise together". A per-tenant decision satisfies that instruction more
faithfully than a global refusal does: the key model and the search promise *are* chosen together, one
tenant at a time.

## Decision

**Content search is a first-class, per-tenant capability with three tiers.**

| Tier | Adds | Key custody permitted | Collection mode required |
|---|---|---|---|
| `disabled` | nothing; structured filtering only — tool, user, date, class, rule, severity, review state | any | any |
| `attachment_names` | substring and fuzzy search over **attachment filenames** | any, **including `customer_held`** | M1 or above |
| `full_text` | full-text search over **prompt text**, with highlighted snippets | `vendor` or `customer_managed` only | M3 for the scope |

**Brief §3.5's mutual exclusivity becomes a database invariant.** A check constraint on `ops.tenant`
makes `content_search = 'full_text'` together with `key_custody = 'customer_held'` unrepresentable. No
configuration path, migration or operator can produce the combination that cannot work. The choice is
made once, per tenant, at onboarding — which is what "cannot be retrofitted" demands.

**Storage:** `ingest.search_text`, one row per searchable unit — the prompt body and each attachment
filename — with a generated `tsvector` column, a GIN index that leads with `tenant_id`, and a partial
trigram index for filenames. Both extensions are on the Azure Flexible Server allow-list.

**Search executes in `content-vault`, not `query-api`.** The vault already holds the only unwrap rights
and has internal-only ingress. `query-api` calls it and is not granted `SELECT` on the index, so the
invariant "exactly one component can read content" survives the change rather than being quietly
retired.

**Scope narrowing.** The tenant tier is a ceiling; the signed policy bundle narrows it per scope (tool,
data class, user population). Search can never be enabled globally across everything, which is what
keeps brief C5's "must not be possible to configure the system into an upload-everything state"
meaningful rather than decorative.

**Audit on every search**, written in the same transaction that serves it, failing closed — because a
search can be subject-scoped and brief §3.6 requires subject-level reads to be audited as they are
served.

**Attachment contents are not indexed.** Only prompt text and filenames. Attachment bytes are the
50–500 GB/year tier in brief §3.1, a different cost and a different breach surface, and indexing them
would be a second decision with its own record.

## The honest consequences, stated once

**1. For a `full_text` tenant, the vendor can read prompt content.** A full-text index over plaintext
is itself plaintext-derived: `tsv` contains the terms. Storing the index in a form the vendor cannot
read would require searchable encryption, which leaks access patterns and query frequencies, is hard to
defend in a security questionnaire, and whose failure mode is catastrophic. This design does not
pretend there is a construction that gives both.

**2. For a `full_text` tenant, the brief §1.1 property changes.** Content no longer stays on the device
by default. The property survives as *the default* (M1, no content egress) and as *a per-tenant choice*,
but it is no longer universal, and the marketing and the security questionnaire both have to say so.

**3. The per-event approval gate no longer protects everything.** Brief C16's second approver continues
to protect **full-content retrieval**. A search returns **bounded highlighted snippets**, so an analyst
with search access can read fragments of content without an approval. A tenant that requires approval
for *any* content exposure should not enable `full_text`; that is a real limitation of the tier and it
is stated in the product documentation rather than discovered.

**4. The index is a new asset with a new blast radius.** It is covered in
[06-security-and-threat-model](../06-security-and-threat-model.md) §3 and §8, and it must be included
in backups, erasure, subject export and the breach-assessment scope.

## Alternatives considered

- **Keep ADR 0008 and decline the requirement.** Rejected: the requirement is a customer requirement,
  and customer requirements supersede internal documents.
- **Search over the M2 excerpt only** (up to 2048 characters, which already crosses the wire). Rejected
  as the primary answer: the M2 excerpt is deliberately *minimised* — a matched span or a redacted
  window — so searching it would return matches against redacted text, which is worse than not
  offering the feature. It also would not answer "find the prompt where someone pasted X", because X is
  exactly what redaction removes.
- **On-device distributed search,** where a query fans out to devices and each searches its local
  spool, preserving customer-held keys. Rejected *for v1* rather than on merit: it is a genuinely
  attractive answer for custody-sensitive tenants, and it is a substantial subsystem (fan-out, partial
  coverage semantics, device availability, result aggregation) whose failure mode — devices offline or
  wiped — makes its results unverifiable in exactly the way this product's honesty principle dislikes.
  It remains the option to revisit if a customer requires both customer-held keys and content search.
- **Searchable encryption or an encrypted index.** Rejected for the reason in consequence 1.
- **A dedicated search cluster (Azure AI Search).** Rejected: prompt text is ~4.4 GB/year, and a second
  copy of plaintext content in another system would widen the breach surface, add a second retention
  path to reconcile, and cost more than the rest of the tenant combined. PostgreSQL full-text search is
  sufficient at this volume, which is the same reasoning that removed the warehouse and the broker.

## Consequences

Easier: the requirement is met without a new datastore. The key/search choice is per tenant and
mechanically enforced, so a tenant's contract, its key custody and its capabilities cannot drift apart.
`attachment_names` gives customers useful search over filenames with **no change to the key model and
no content leaving the device** — which may satisfy more of the requirement than it first appears,
because filenames are frequently what an investigator is searching for.

Harder: the product now has two custody-dependent capability tiers, and the sales and support
conversations have to be able to explain which tenant has which and why. The index adds a retention and
erasure path to reconcile, a backup surface to assess, and a plaintext-derived asset to rank in the
threat model. `content-vault` grows from a key holder into a query executor, which is a real expansion
of the most sensitive component's job.

We now maintain: the index and its two extension dependencies, a search API on `content-vault`, the
audit path for searches, and per-tenant disclosure of which tiers are enabled and by whom — the last of
which brief §3.2's requirement that every configuration change carry an audit entry makes non-optional.

Revisit if: a material customer requires customer-held keys *and* prompt-content search, at which point
on-device distributed search returns to the table as an additional mechanism rather than a replacement
— it would serve that tenant without reopening this decision for everyone else.
