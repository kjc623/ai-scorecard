# 0008. There is no server-side full-text search over content, in any key mode

Status: **SUPERSEDED by [ADR 0014](0014-content-search-is-a-per-tenant-capability.md)** on 2026-10-02,
on customer requirement. Content search is now a per-tenant capability with three tiers, and the
key/search incompatibility is enforced per tenant instead of refused globally.

Retained for the reasoning, which ADR 0014 does not discard: brief §3.5's mutual exclusivity is real,
not a preference. What changed is the response to it — a per-tenant choice rather than a global refusal.
Do not implement anything from this record.

Date: 2026-10-02

Resolves: brief risk **R10**, which the brief requires to be decided "before anyone builds a content
search feature, because it is not a feature that can be added later without changing the key model".

## Context

Brief §3.5 states the constraint plainly: **"Customer-held keys and server-side full-text search over
content cannot both exist."** If the vendor cannot read content, the vendor cannot index it.

Both sides of that line are load-bearing:

- Brief §3.3 requires per-tenant keys with customer-held keys supported, because some buyers require
  that the vendor cannot read content at all.
- Brief §3.6 asks "what exactly was sent?" and the natural reading of that question is content search.
- Brief §3.6 also requires "bulk export to the customer's own storage in a columnar format, on a
  schedule", and notes that one customer in ten will want to run their own SQL.

The brief is explicit that this is a decision, not a trade-off to be deferred.

## Decision

**Customer-held keys are permanent. There is no server-side full-text search over content, in any key
mode, ever.**

- **Search is over metadata and labels**, which brief §3.5 confirms is unconstrained. An analyst filters
  by tool, user, date, class, rule, severity and review state, then retrieves the specific events that
  matter.
- **Content is reached only by per-event approved retrieval** (brief §4.4): a case reference, a second
  approver, and an audit entry written before content is returned.
- **The escape hatch is the export.** A customer who needs cross-content search receives the scheduled
  Parquet export in storage they control, with keys they hold, indexed by their own tooling. This
  satisfies both §3.6's "run their own SQL" requirement and the content-search requirement with a single
  mechanism, and it satisfies them without the vendor ever holding plaintext.

The content-search decision is not per-tenant. Offering it to some tenants and not others would mean two
storage paths, two key models and two breach surfaces, and would make the key model retrofittable for
exactly the customers who chose the stronger custody mode.

## Alternatives considered

- **Server-side search, with customer-held keys as a documented limitation.** Rejected: it inverts the
  product's defining property for a feature the brief ranks below it, and it makes the vendor's inability
  to read content conditional, which is precisely the thing buyers in this segment are testing.
- **Searchable encryption or order-preserving encryption.** Rejected. Practical schemes leak
  substantially to the server (access patterns, frequency), the leakage is hard to explain honestly in a
  security questionnaire, and it would introduce a cryptographic dependency whose failure mode is
  catastrophic for a feature that is not the product.
- **Search on request, by unwrapping content server-side on analyst demand.** Rejected for the same
  reason as the first: it makes every analyst query a decryption event, and it means the vendor reads
  content continuously rather than exceptionally.
- **Content search only in `vendor` custody mode.** Rejected because it makes the *stronger* custody mode
  the one with fewer features, so customers would rationally choose the weaker one — the opposite of
  what the design should encourage.

## Consequences

Easier: the key model never has to change, which is what R10 asks for. There is no index over content,
so there is no index to leak and no plaintext content pipeline to secure. The GDPR position is simpler:
the vendor's inability to read content is structural rather than promised.

Harder, and stated plainly rather than buried: **an analyst cannot grep across stored prompts.** They
can filter to the events that matter and retrieve them one at a time through an approved path. For a
"did anyone ever paste this contract number" question, the answer comes from the customer's own exported
copy, not from our dashboard.

We now maintain: the export path as a first-class product surface rather than an afterthought, because it
now carries a requirement that would otherwise need a feature; and a clear script for the sales
conversation, because "no content search" is a limitation a customer will discover and it is better
introduced than discovered.

Revisit if: a customer makes cross-content search a hard commercial gate. That is a product decision,
not an engineering one, and it cannot be absorbed quietly: it would mean giving up customer-held keys
for those tenants and running the two key models on separate storage paths from that point on.
