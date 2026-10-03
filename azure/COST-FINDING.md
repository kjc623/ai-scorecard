# Cost-model finding: the Front Door base is regional, not per-tenant

**Raised by:** `azure/tools/check-infra.mjs` while recomputing docs/05 §11 from its own unit prices.
**Verified by:** the Lead, directly against the document (quotes below, with line numbers).
**Status:** the arithmetic finding is confirmed. **The consequence for the headline number is a product
decision for a human, not for an agent — this file states the contradiction and the two resolutions
rather than picking one.**

---

## The contradiction

`docs/05-platform-delivery.md` says three different things about the same $330.

| Line | What it says | Reading |
|---|---|---|
| 52 | "One **regional production environment per data-residency region** … the resources marked region-shared are deployed once per region, the rest are shared across all tenants **in** that region" | Region-scoped resources are shared |
| 831 | `\| Front Door Premium, base \| $330 / month \| Per profile per region \|` | **One profile per region** |
| §11.2 | Front Door enters the per-tenant subtotal at $330, and §11.6 (line 1035) calls it "$330, or **40% of the tenant**, before a single request … Fixed and unavoidable in this design" | One profile per tenant |

The unit-price row and the two sections that charge the price disagree about what the price buys. One of
them is wrong, and the document does not say which.

## What the arithmetic says

§11.3 lists the shared regional costs that are allocated rather than charged, and Front Door's base is
**not among them** — it is charged per tenant in §11.2. If the unit-price definition is the correct one,
then the base belongs in §11.3, where it is divided across the tenants in the region:

```
per-tenant subtotal (§11.2)                        $826.66
  − Front Door base, which is regional               $330.00
  + its allocated share in §11.3 (100 tenants)         $1.45   [approx; 1/n of the base]
  = per tenant per month                             ≈ $498
```

At 100 tenants the figure lands back inside the **$300–700** band that master §1.4 explicitly records as
having been corrected away, and the document's own stated reason for the correction — that the model's
arithmetic does not support the old band — inverts: it is the new figure that the arithmetic does not
support *unless each tenant gets its own Front Door profile*.

## The two resolutions, and what each costs

**A. Front Door is per tenant** (what §11.2 and §11.6 assume). Then §2's opening paragraph and §11.1's
unit-price row are wrong, and the per-tenant figure stands at ≈$830–840. Cost: one profile per tenant
per region, which at 100 tenants is 100 profiles rather than one — a real but defensible position if
per-tenant isolation at the edge is wanted, and it would need to be stated as such, because a reader
currently cannot tell whether it is isolation or an arithmetic slip.

**B. Front Door is per region** (what §11.1's "Per profile per region" and §2's sharing rule say). Then
§11.2 must move the base into §11.3, the per-tenant figure is ≈**$498** at 100 tenants, and master §1.4's
correction must itself be corrected — its conclusion ("≈$830–840, not the $300–700 an earlier draft
assumed") rests on assumption A.

The difference is the headline number of the whole cost model, roughly a factor of 1.7, and it is a product
decision: how much edge isolation each tenant gets. That is why this file does not resolve it.

## Second, smaller finding: an unpriced line item

§11.1 says "HSM-backed keys carry an additional per-key monthly charge" and gives no rate. §11.2 then
charges ~30 keys at about $2 in total, which is ≈$0.067 per key per month. If the real shape is nearer
$5 per key per month, the line becomes ≈$150 and the estimate rises by ≈$147 — an order of magnitude on
that line, from a rate the document never states. **Action: §11.1 needs the unit price**, and the
document should say which custody mode incurs it, since only `customer_held` requires HSM-resident keys.

## What is not in question

The rest of §11's arithmetic is exact: every line recomputes from its own terms, and the model matches the
document line by line within the $1 rounding the document declares. The subtotal is $826.66 against the
document's $828 — inside the stated $3 budget. So this is not a general unreliability of the model; it is
one mis-scoped line and one missing rate.

## Where this is checked

`azure/cost-model.md` carries the recomputation and the findings as data, and `azure/tools/check-infra.mjs`
asserts them, so the finding fails a test if the model stops reproducing it. Whoever resolves the
contradiction should change the document and the model together, and the checker will confirm the two
agree again.

---

## Resolution status: recorded, not decided

The user was asked which reading is intended and answered: *"I'm not sure. Record and move on."* So this
finding stands as a **recorded contradiction**, not a corrected model. Nothing in `azure/cost-model.md`
or `docs/05-platform-delivery.md` has been changed, and the �$830�840 figure remains what the
documents say while this file records why it may be wrong by a factor of ~1.7.

Two consequences, so the open question does not become invisible:

1. **Every cost figure in this repository is now marked as under review**, including those quoted in
   `README.md` and `docs/00-architecture.md` �1.4. A commercial decision should not be taken on
   �$830�840 until �11.1 and �11.2 are made to agree.
2. **A lab-cost task exists (task-23, `docs/lab/LAB-COST.md`)** which is explicitly told not to inherit
   this conclusion as fact. The lab is the near-term need; the production number can wait for a real
   quote, which is the only thing that will settle it.