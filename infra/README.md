# infra/ — the Azure delivery of docs/05-platform-delivery.md

This directory is the deployment as code: one Bicep composition per environment, modules that own one
resource family each, parameter files that carry everything an environment may differ on, three
pipelines, and a static checker that validates the whole thing **without an Azure subscription**.

## Deployment is NOT VERIFIED

Nothing in this directory has ever been deployed. There is no Azure subscription, no `az` CLI, no
Bicep compiler and no network on the machine where it was written, so:

- the Bicep has **not** been compiled, so a syntax error or a wrong resource property is a real
  possibility;
- no resource has been created, so no `what-if` output, no idempotency check and no `existing`-resource
  behaviour has been observed;
- the prices in `cost-model.md` have **not** been checked against the Azure pricing calculator;
- the private DNS, Private Link approval, managed-identity role assignment and Container Apps
  revision behaviours have not been observed — they are stated as the document requires, and only a
  deployment can confirm them.

**What is verified here** is what can be verified without Azure: every parameter is typed and
described, no region or SKU is hard-coded outside a parameter file, no secret is in the repository,
the private-only properties are declared as such, content-vault is internal-only with no Front Door
route, the database has no public path and no firewall rules, and every row of the document's §2
inventory is implemented. Those checks run in `node --test infra/tools/` and they fail the build when
a property regresses.

### The exact commands a human would run

Preconditions, all of which must hold before the commands mean anything:

1. `az` CLI **2.60+** with the Bicep CLI installed (`az bicep install`), and a login under the
   **workload-federated deployment identity** — never a stored credential (§5.2). In CI this is an OIDC
   token for the repository, branch and environment; locally it is a human operator's Entra ID
   principal with a PIM-eligible role.
2. The subscription has the resource providers registered: `Microsoft.App`, `Microsoft.DBforPostgreSQL`,
   `Microsoft.Storage`, `Microsoft.KeyVault`, `Microsoft.Network`, `Microsoft.Cdn`,
   `Microsoft.OperationalInsights`, `Microsoft.Insights`, `Microsoft.ContainerRegistry`,
   `Microsoft.Web`, `Microsoft.ManagedIdentity`, `Microsoft.Consumption`.
3. The deployment identity holds **resource-group deployment rights only** (§5.4). It must not hold Key
   Vault data-plane or database access; if the deployment fails asking for those, the identity is wrong,
   not the template.
4. The PostgreSQL break-glass login name comes from the pipeline (a variable), and its credential is
   supplied at deploy time from Key Vault. `passwordAuth` is disabled on the server, so the name alone
   proves nothing.
5. The container images named by `imageTag` already exist in the registry — the platform pulls over the
   registry's private endpoint, and a missing image is a failed revision, not a failed deployment.

```powershell
# 1. The resource group and the environment-level checks the template cannot do itself.
az group create --name rg-sac-prod-eastus --location eastus
az deployment group validate `
  --resource-group rg-sac-prod-eastus `
  --template-file infra/main.bicep `
  --parameters infra/params/prod.eastus.bicepparam `
  --parameters postgresAdministratorLogin=$env:SAC_PG_ADMIN_LOGIN

# 2. Always look at the diff before applying it.
az deployment group what-if `
  --resource-group rg-sac-prod-eastus `
  --template-file infra/main.bicep `
  --parameters infra/params/prod.eastus.bicepparam `
  --parameters postgresAdministratorLogin=$env:SAC_PG_ADMIN_LOGIN

# 3. Apply. The pipeline does this from `infra/pipelines/infra.yml`; a human does it only during a
#    declared incident.
az deployment group create `
  --resource-group rg-sac-prod-eastus `
  --template-file infra/main.bicep `
  --parameters infra/params/prod.eastus.bicepparam `
  --parameters postgresAdministratorLogin=$env:SAC_PG_ADMIN_LOGIN

# 4. Approve the Front Door private-link connection to the Container Apps environment. It is created
#    pending approval on purpose: an origin that appears without a human is an origin nobody approved.
az network private-endpoint-connection list --id <container-apps-environment-id>
```

After a deployment, the properties that only exist at runtime are asserted by the hourly resource-graph
queries in §3.5 (no PaaS resource with public network access, no storage account with shared-key access,
content-vault ingress internal, every container app with a user-assigned identity, every vault with
purge protection). Those queries are not in this directory yet: `infra/pipelines/drift.yml` runs the
`what-if` half, and the resource-graph half is a documented gap (see "Gaps" below).

## How to check the infrastructure without Azure

```powershell
node --test infra/tools/index.mjs        # the suite: 38 checks
node --test infra/tools/check-infra.test.mjs   # the same suite, named directly
node infra/tools/check-infra.mjs         # the same checks as a report, exit 1 on a finding
```

Both invocations above were run and pass on Node 22.23.1 (Windows). `node --test infra/tools/` — the
directory form — resolves the directory as a module on this Node build and fails with
`MODULE_NOT_FOUND` before running anything, which is why `index.mjs` exists and is named explicitly
here; `tools/verify-all.mjs` discovers `check-infra.test.mjs` by name and reports `infra/tools` PASS.
A glob also works: `node --test 'infra/tools/*.test.mjs'`.

The checker has no dependencies and never makes a network call. It reads `infra/**/*.bicep`,
`infra/params/*.bicepparam`, `infra/inventory.json`, `infra/cost-model.md` and
`docs/05-platform-delivery.md`.

## What is where

| Path | What it is |
|---|---|
| `main.bicep` | The composition: modules, identity bindings, and the two architectural assertions stated in the clear |
| `modules/` | One resource family per module, matching §3.3's layout exactly |
| `params/` | `dev`, `staging`, `prod.eastus`: the only place an environment differs (§3.2) |
| `pipelines/` | `infra.yml` (deploy), `drift.yml` (nightly `what-if`), `policy-scan.yml` (the properties that must never regress) |
| `inventory.json` | Every §2 inventory row mapped to the module that implements it; §2.2's deliberately-absent list with the resource types that must never appear |
| `cost-model.md` | §11's arithmetic recomputed from its own unit prices, with the disagreements stated as findings |
| `tools/` | The zero-dependency checker and its suite |

## The inventory diff against docs/05 §2

All **18** rows of §2 are implemented, and `inventory.json` records which module implements each. The
checker diffs in both directions, so the state is not a claim in prose:

- a §2 row with no module is reported as `inventory-missing` (**currently none**);
- a module with no §2 row is reported as `inventory-extra` (**currently none**);
- a §3.3 module missing from disk is `layout-missing` (**currently none**);
- a module outside §3.3's layout is `layout-extra` (**currently none**);
- a §2.2 deliberately-absent resource type appearing anywhere is `deliberately-absent`
  (**currently none** — no Service Bus, Event Hubs, AI Search, Synapse, Databricks, Fabric, AKS,
  virtual machines or reservations).

## Gaps, stated rather than implied

1. **The hourly resource-graph assertions of §3.5 are not implemented.** `drift.yml` runs the nightly
   `what-if`; the resource-graph queries (the ones that catch a portal change after the fact) need an
   Azure connection and are a documented gap.
2. **No environment has been deployed**, so nothing in this directory has been observed against a real
   API version. The checker proves the properties are *declared*; only a deployment proves they are
   *effective*.
3. **The cost model is list-price arithmetic, not a bill.** Two findings in `cost-model.md` change the
   figure materially (Front Door's base is regional in this architecture, not per-tenant; the Key Vault
   key charge has no unit price in §11.1).
