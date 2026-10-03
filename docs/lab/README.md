# `docs/lab/` — what a zero-cost lab can prove

`LAB-COST.md` is the design of a lab that costs nothing: what it stands up, what it deliberately
leaves out, and — the part that matters — **what it therefore cannot tell you**.

Two parts, and the distinction is the whole document:

- **Part A** is the local lab that actually runs: PostgreSQL with the real schema, the Go services in
  containers, and a smoke test that proves they serve. That is implemented in [`../../localdev/`](../../localdev/),
  which is the runnable half.
- **Part B** is architecture fidelity in Azure: the same modules as production, driven by a
  parameter file rather than a second Bicep tree, so it cannot drift. That is
  [`../../azure/params/lab.bicepparam`](../../azure/params/lab.bicepparam).

The reasoning behind "a parameter file, not a second composition" is drift: a parallel Bicep tree
diverges from production within a month, whereas switches on the same modules cannot — if `main.bicep`
changes, the lab deploys the change.

## The cost model, and its disclaimer

`LAB-COST.md` §6 lists what the lab leaves untestable — blob storage, a real certificate authority, a
real cloud KMS, and anything that requires the Azure control plane. Those are not oversights; a fake
blob account would prove nothing about the real one, which is why `localdev/` has none either.

All figures in this directory are estimates at Azure East US list price, pay-as-you-go, as of
2 October 2026, and must be re-baselined before any commercial commitment. A contradiction inside the
model is recorded rather than resolved in [`../../azure/COST-FINDING.md`](../../azure/COST-FINDING.md),
because resolving it is a product decision about how much edge isolation each tenant gets.

[`tools/check-lab-cost.mjs`](tools/) checks the document's arithmetic against its own unit prices.
