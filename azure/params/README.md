# `azure/params/` — where environments differ

The module templates in `../modules/` are identical in every environment. These files are what an
environment changes, and they are the **only** place a region, SKU or replica count is allowed to
appear — `../tools/check-infra.mjs` fails the build if one is hard-coded into a template.

| File | Environment | What it is for |
|---|---|---|
| `dev.bicepparam` | Development | The breakable one. Cheap SKUs, no HA, torn down freely |
| `staging.bicepparam` | Staging | Production-shaped volume and skew, production SKU, no HA. A rehearsal on a different shape is a rehearsal of something else |
| `prod.eastus.bicepparam` | Production, East US | Live tenancy for one residency region. A second region is a second file, not a second template |
| `lab.bicepparam` | The architecture-fidelity lab | Sets switches on the *same* modules so the lab cannot drift from production. See [`../../docs/lab/LAB-COST.md`](../../docs/lab/LAB-COST.md) |

## No secret lives here

The PostgreSQL break-glass login in these files is a **name**. Its credential is supplied at deploy
time from Key Vault, and `passwordAuth` is disabled on the server, so the name alone proves nothing.
Nothing in this directory is encrypted, because nothing here is a secret — and the checker treats a
secret appearing in a parameter file as a finding.

## The deploy commands

`../README.md` has the full sequence and its preconditions. In short: `az deployment group validate`,
then `what-if`, then `create` — always reading the diff before applying it.
