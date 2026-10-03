# `docs/lab/tools/` — checking the lab's arithmetic

`check-lab-cost.mjs` recomputes `../LAB-COST.md`'s cost figures from the unit prices the document
itself quotes, so the numbers cannot drift away from the rates they were derived from.

```
node docs/lab/tools/check-lab-cost.mjs
```

It exists because a cost document is unusually easy to get wrong quietly: a rate is edited, a total is
not, and the headline figure is now arithmetic from a previous draft. Recomputing from the document's
own prices makes that a failure rather than a discrepancy somebody notices later.

It checks **arithmetic against stated prices**. It does not check the prices, which are estimates as of
2 October 2026 and must be re-baselined before any commercial commitment — and it cannot see the
contradiction recorded in [`../../../azure/COST-FINDING.md`](../../../azure/COST-FINDING.md), where
Front Door's base cost is priced per region in one section and per tenant in another. That one is a
product decision, so it stays a finding rather than becoming a check.
