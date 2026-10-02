# The cheapest Azure lab that can still run this architecture

**Task:** L1 / task-23. **Write scope:** `docs/lab/` only. **Nothing in this document was deployed, and nothing was priced against Azure.**

---

## 0. What is not verified, stated first

Every number in this document is a **list-price estimate**. It was not retrieved from Azure, not
checked against the pricing calculator, and not confirmed by an invoice, because this host has:

* **no Azure subscription,**
* **no `az` CLI,**
* **no network.**

Three further classes of statement are marked where they appear, because they are the ones a reader
is most likely to mistake for facts:

| Mark | Means |
|---|---|
| **[§11.1]** | The unit price is taken from `infra/cost-model.md`'s own unit-price table, which is itself an estimate on the basis "Azure list prices, East US, pay-as-you-go, no discounts". Arithmetic on an estimate is still an estimate. |
| **[EST]** | The unit price is *not* in this repository. It is my estimate from general Azure list pricing, and it is labelled so it can be wrong in a reviewable way. The two that matter most are the PostgreSQL B1ms compute rate and the Private Link endpoint rate. |
| **[AWAIT]** | A claim about Azure **behaviour** (billing granularity, an auto-restart, a free-tier inclusion) which is stated as published and **must be confirmed on a real subscription before anyone relies on it**. |

**I do not inherit `infra/COST-FINDING.md`'s conclusion.** That file records a contradiction about
whether Front Door's $330 base is per tenant or per region, and the user's answer was "record and
move on". Nothing here depends on which reading wins: **the lab deletes Front Door entirely**, so
the question does not arise in the lab's arithmetic. Where this document touches the production
figure at all, it says so and marks it as under review.

**The production per-tenant figure (≈$498–840/tenant/month) is not a lab figure and is not used as
one.** A lab that inherits a production tenant shape is the failure this document exists to prevent;
§7.7 prices that mistake.

---

## 1. The answer

The question was: *cheapest Azure lab that still runs this architecture.* The answer has two parts,
because there are two different things to test and they have different price tags.

### Part A — the development lab: your own machine. **$0.00/month.**

PostgreSQL 16 in a container (the same engine, applied to `db/schema.sql` unmodified), the two Go
services in the only store mode they currently support, the dashboard as the static file it is, and
the device tier where it already lives: on this Windows host. Docker is available here; that is the
whole footprint.

**Part A is not a consolation prize.** It is what you will use on the two evenings a week, and it is
the only place where the browser tier and the device tier can be tested together today, because the
Azure shape deliberately has no public path to either.

### Part B — the architecture-fidelity lab: **≈$32/month if left running, ≈$1.04/day if used, $0 while deleted.**

A parameterised subset of `infra/`: real PostgreSQL Flexible Server 16 with `publicNetworkAccess`
Disabled, the VNet-injected Container Apps environment with its internal load balancer, three
container apps with `content-vault` on **internal ingress**, managed identities, Key Vault, and a
**private** blob account reached over a private endpoint. Deployed on demand, deleted after the
session.

| | Part A (local) | Part B (Azure) |
|---|---|---|
| Cost while you are using it | $0.00 | ≈$1.04/day |
| Cost while it exists and does nothing | $0.00 | ≈$30.16/month, or ≈$17.75 with PostgreSQL stopped |
| Cost after `az group delete` | $0.00 | **$0.00** |
| What it proves | the code, the schema, the invariants, the device and browser tiers | identity, private networking, the managed database, the private blob path, the internal-only vault |

Every figure in this document is recomputed from its own terms by
`docs/lab/tools/check-lab-cost.mjs` (`node docs/lab/tools/check-lab-cost.mjs`), so a typo here fails
a command rather than a budget.

### The three prerequisites that block Part B from running the services *today*

This is the most important thing in the document, and it is not about cost. **No Azure
configuration, at any price, can currently put `ingest-api` or `content-vault` in front of
PostgreSQL.** Not because of the infrastructure — because of four gaps in the repository, all
verified by reading the files (§9):

1. **There are no Dockerfiles anywhere in the tree.** The "services run as containers" requirement
   has no artifact yet.
2. **Neither Go service can open a PostgreSQL connection.** No driver is compiled in — `sql.Register`
   appears nowhere and neither `go.mod` requires one — and both binaries refuse the SQL store rather
   than half-wiring it: content-vault fails `--store sql` with *"this offline build has none (ADR
   0016)"*, ingest-api requires `-driver` to name a driver *"registered in this binary"*.
   `--key-backend kms` (Azure Key Vault) is likewise **not implemented** and refuses to start.
3. **`container-app.bicep` has no `command`/`args` parameter**, and both services take their listen
   address and store mode from **flags**, not environment variables (ingest-api defaults to
   `127.0.0.1:8443`; content-vault to `127.0.0.1:8090` and refuses a non-loopback bind without
   `--allow-non-loopback`). The module probes `:8080/healthz`. The deployed containers would bind
   loopback on the wrong port and be unreachable.
4. **`main.bicep` passes environment variables no binary reads** (`SAC_PG_HOST`, `SAC_ROLE`,
   `SAC_BLOB_CIPHERTEXT_ENDPOINT`, `SAC_KEYVAULT_URI`), while content-vault reads
   `CONTENT_VAULT_KMS_ENDPOINT`, `CONTENT_VAULT_KMS_MODE`, `CONTENT_VAULT_SCOPE_TIERS` and
   ingest-api reads nothing.

So Part B's honest value **today** is: it applies the schema and runs the invariant suite against a
real managed PostgreSQL 16, exercises Entra authentication to that server, proves the RLS and grant
model as the runtime roles, proves the private blob path, and proves the internal-only ingress of
`content-vault` at the platform level. It does **not** yet prove the services' database or key-vault
paths, because those paths are not implemented in this build. §6 says this again, property by
property, which is where it belongs.

**Recommendation.** Build Part A this week; deploy Part B for a session when you are testing an
identity, network or managed-database property, and delete it afterwards. Close the four gaps in §9
before treating Part B as an end-to-end lab.

---

## 2. The five things the lab must keep, and how it keeps them

Straight from the task, because a cheap lab that cannot run the architecture is worthless.

| # | Requirement | How the recommended lab keeps it | What it costs |
|---|---|---|---|
| 1 | **PostgreSQL 16 Flexible Server**, `db/schema.sql` unmodified, `db/invariants.test.sql` passing | `modules/postgres.bicep` **unchanged**, with `majorVersion: '16'`, Burstable B1ms, `publicNetworkAccess: Disabled` (hard-coded in the module). No SQLite, no Cosmos, no other major version — those are not this system. | $16.09/month |
| 2 | **Services as containers, `content-vault` internal-only** | `modules/container-app.bicep` **unchanged**; `ingress: 'internal'` for content-vault, and the environment keeps `internalLoadBalancer: true` so the property holds at the environment level as well as the app level. | $0 for the environment; ≈$3.44 of compute, covered by Container Apps' monthly free grant |
| 3 | **Identity and secrets stay real** | Seven user-assigned managed identities, Key Vault with RBAC authorisation, purge protection and 90-day soft delete, **no credential in the repository**. The lab's one deviation is the vault SKU (Standard, not Premium) — §6.4 and §8. | $0.15/month + $0 for the identities |
| 4 | **Blob for ciphertext, private** | `modules/storage-ciphertext.bicep` **unchanged**: `allowBlobPublicAccess: false`, `allowSharedKeyAccess: false`, `publicNetworkAccess: 'Disabled'`, plus **a private endpoint** — which is not optional here, because with public network access disabled and no endpoint the account has no reachable path at all. | $7.30/month (the endpoint) + $0.12 (storage) |
| 5 | **The endpoint side** | **Nothing Azure-side is required.** The agent runs on this Windows host; the extension needs Chromium, which is not installed (a local prerequisite, not an Azure one). The lab's job is the server side plus one enrolled device — and with an internal load balancer, a device outside the VNet cannot reach `ingest-api`, which is why §7.6 offers an explicitly-labelled "open mode". | $0.00 |

---

## 3. The recommended configuration, itemised

### 3.1 Part A — local, $0.00/month

| Item | What runs | Cost |
|---|---|---|
| PostgreSQL 16 | `postgres:16` container, `db/schema.sql` applied unmodified, `db/invariants.test.sql` run as the runtime roles | $0.00 |
| Ciphertext store | Azurite (blob emulator) or a local directory behind the same interface | $0.00 |
| Services | `ingest-api` and `content-vault` built from `cmd/`, run with `-store memory` — the only mode this build supports (§1) | $0.00 |
| Read path | `services/query-api` (Node, zero dependencies) + `apps/dashboard` opened as the static file it is | $0.00 |
| Device tier | The Windows host + Chromium for the extension (**prerequisite: Chromium is not installed**) | $0.00 |
| **Total** | | **$0.00** |

### 3.2 Part B — the Azure fidelity lab, itemised

Basis: **East US, pay-as-you-go, no discounts, 730 hours in a month**, the same basis as
`infra/cost-model.md`. The lab deletes: Front Door, WAF, the Static Web App, the export storage
account, Managed HSM, and 16 of the 18 alerts.

| # | Resource (the `infra/` module that creates it) | Lab specification | Unit price | Monthly |
|---|---|---|---|---|
| 1a | **PostgreSQL Flexible Server** compute (`postgres.bicep`) | Burstable **B1ms**, 1 vCPU / 2 GiB, HA Disabled | $0.0170/hour **[EST]** | **$12.41** |
| 1b | PostgreSQL storage | 32 GiB provisioned | $0.115/GiB-month **[§11.1]** | **$3.68** |
| 1c | PostgreSQL backup | 7-day PITR, no geo-redundant copy | within the free allowance for provisioned size **[AWAIT]** | **$0.00** |
| 2 | **Container Apps environment** (`container-apps-env.bicep`) | Consumption-only, no workload profiles, VNet-injected, internal LB | no fixed fee for a Consumption environment **[AWAIT]** — see §3.4 | **$0.00** |
| 3 | **Container Apps replicas** (`container-app.bicep`) | 3 apps at **`minReplicas: 0`**; ≈30 vCPU-h + 60 GiB-h + 0.5M requests per month | $0.0864/vCPU-h, $0.0108/GiB-h, $0.40/M requests **[§11.1]** | **$3.44** |
| 3b | Container Apps monthly free grant | 180,000 vCPU-s, 360,000 GiB-s, 2M requests per subscription **[AWAIT]** | offsets line 3 at this volume | **−$3.44** |
| 4 | **Container Registry** (`registry.bicep`) | **Basic** SKU, no geo-replication, no private endpoint | ≈$0.167/day **[EST]** | **$5.00** |
| 5 | **Key Vault** (`keyvault.bicep`) | **Standard** SKU, RBAC, purge protection on, 90-day soft delete, ~50k operations | $0.03/10k ops **[§11.1]** | **$0.15** |
| 6 | **Storage — ciphertext** (`storage-ciphertext.bicep`) | StorageV2, **LRS**, HNS, ~1 GiB hot + ~10k transactions | $0.0184/GiB-month **[§11.1]** | **$0.12** |
| 7 | **Private endpoint** for the blob account (`private-endpoints.bicep`) | 1 endpoint, auto-approved by the owning subscription | ≈$0.01/hour **[EST]** | **$7.30** |
| 8 | **Private DNS zones** (`network.bicep`) | **3** of the 8 zones: postgres, blob, vaultcore | $0.50/zone-month (first 25) **[EST]** | **$1.50** |
| 9 | **Log Analytics** (`log-analytics.bicep`) | 30-day retention, archive off, **1 GiB/day cap**, ~0.2 GiB/month ingested | $2.76/GiB **[§11.1]** | **$0.55** |
| 10 | **Container Apps Job** (`container-app-job.bicep`) | Repurposed as the schema-apply / invariants job: ~10 runs × 2 min × 1 vCPU | $0.0864/vCPU-h **[§11.1]** | **$0.03** |
| 11 | **Alerts** (`monitoring.bicep`) | **2** rules instead of 18 (ingest down, database storage) | ≈$0.50/rule-month **[EST]** | **$1.00** |
| 12 | **Budget** (`budget.bicep`) | one budget, **$40**, thresholds 50/80/100% + forecast alert | free **[AWAIT]** | **$0.00** |
| 13 | VNet, subnets, NSG, shared-key-disabled storage, managed identities | — | free | **$0.00** |
| 14 | Egress | a few hundred MB/month of test traffic | first 100 GB/month free **[AWAIT]** | **$0.00** |
| | **Total, left running for a month** | | | **≈$31.74** |

| Period | Cost |
|---|---|
| Per day (730 h basis) | **≈$1.04** |
| Per week (left up by accident) | **≈$7.30** |
| Per month (left up by accident) | **≈$31.74** |

**A session, rather than a month.** Two evenings a week, four hours each, with the lab deployed only
for those hours — 35 hours a month — costs the metered lines only: PostgreSQL compute **$0.60**
(35 h × $0.0170), the schema job **$0.03**, Log Analytics a few cents. **≈$0.90 of metered usage per
month**, with the Container Apps compute ($3.78 at 35 h of one vCPU and 2 GiB) sitting **inside the
monthly free grant**. The gap between $0.90 and $31.74 is not usage; it is *existence* (§4), which is
why the recommended discipline is deletion rather than stopping.

### 3.3 The floor: what bills while nothing is running

| Line | Billing while idle | Monthly |
|---|---|---|
| PostgreSQL compute | only if the server is **started** | $12.41 (or $0 stopped) |
| PostgreSQL storage | **yes — storage bills whether the server runs or not** | $3.68 |
| Container Apps replicas | **no, at `minReplicas: 0`** | $0.00 |
| Container Apps environment | no fixed fee in Consumption **[AWAIT]** | $0.00 |
| Container Registry Basic | **yes** | $5.00 |
| Private endpoint | **yes** | $7.30 |
| Private DNS zones | **yes** | $1.50 |
| Key Vault, blob storage | effectively yes, at ~$0.27 | $0.27 |
| **Floor, deployed and unused** | | **≈$30.2** |
| **Floor with PostgreSQL stopped** | | **≈$17.8** |
| **Floor after `az group delete`** | | **$0.00** |

**Deploying the lab and never touching it costs 95% of using it.** That is the single most useful
sentence in this section, and it is why §4 recommends deletion.

### 3.4 The four uncertainties in this table

1. **The Container Apps environment: $0 or $50?** `docs/05` §2 says "Consumption-only, no workload
   profiles", and a Consumption environment has no fixed charge. `infra/cost-model.md` §11.3 carries
   "Container Apps environment ~$50" as a shared-regional cost. One of those is a real charge and the
   other is an allocation. **If $50 is real, add it and the lab is ≈$82/month.** This is the largest
   single swing in the document and it is settled by one invoice line.
2. **The B1ms rate ($0.0170/hour, [EST]).** §11.1 prices only D2ds_v5 ($0.252/hour). If B1ms is
   ±20% off, the lab moves by ±$2.50/month.
3. **The free grant and the free tier.** Container Apps' monthly free grant (180,000 vCPU-s,
   360,000 GiB-s, 2M requests) appears to cover the lab's compute entirely, and the Azure free
   account's 12-month offer appears to cover Flexible Server Burstable B1ms for 750 hours/month plus
   32 GiB of storage — which would zero lines 1a, 1b and 3. **As published, not verified
   [AWAIT].** If both hold, the lab's first year is ≈$14/month (ACR, private endpoint, DNS zones,
   alerts) and the $200 sign-up credit covers about fourteen months of that.
4. **The Private Link endpoint rate (≈$0.01/hour, [EST]).** It is the lab's fourth-largest line and
   the one people forget to count when they say "private is free". Private endpoints are not free.

### 3.5 Unit prices used, and where each came from

| Unit | Value | Source |
|---|---|---|
| Container Apps active vCPU-hour | $0.0864 | `infra/cost-model.md` §11.1 **[§11.1]** |
| Container Apps active GiB-hour | $0.0108 | **[§11.1]** |
| Container Apps per million requests | $0.40 | **[§11.1]** |
| PostgreSQL D2ds_v5 hour (production reference only) | $0.252 | **[§11.1]** |
| PostgreSQL storage GiB-month | $0.115 | **[§11.1]** |
| Key Vault per 10k operations | $0.03 | **[§11.1]** |
| Log Analytics per GiB | $2.76 | **[§11.1]** |
| Blob hot GiB-month | $0.0184 | **[§11.1]** |
| Front Door Premium base (production reference only) | $330/month | **[§11.1]** — under review, see §0 |
| Managed HSM hour (production reference only) | $4.60 | **[§11.1]** |
| **PostgreSQL Burstable B1ms hour** | **$0.0170** | **[EST]** — not in the repository |
| **Azure Container Registry Basic** | **≈$0.167/day (≈$5/month)** | **[EST]** — the repository has ACR Premium + geo-replication ≈$60 |
| **Private Link endpoint hour** | **≈$0.01** | **[EST]** — not in the repository |
| **Private DNS zone month (first 25)** | **≈$0.50** | **[EST]** |
| **Monitor alert rule month** | **≈$0.50** | **[EST]** |
| **Front Door Standard base** | **≈$35/month** | **[EST]** — only §11.1's Premium $330 is in the repository |

---

## 4. Start/stop discipline

### 4.1 The trap, in one line

**Every resource in this lab bills by existing, not by being used.** The two the task names are
both real: a PostgreSQL Flexible Server bills storage while it is stopped, and a Container Apps
environment bills for every replica whose minimum is above zero — the environment itself is free in
Consumption, but its `minReplicas` floor is not. Setting `minReplicas: 0` on all three apps is the
largest single cost decision in the lab, and it is a **parameter**, not a code change.

### 4.2 The second trap: "stopped" is not a resting state

**[AWAIT]** A stopped Flexible Server is **automatically started again after seven days**. So
"stop it and forget it" degrades into "paying for it again" on a schedule nobody chose. The response
is not a better stop command; it is **deletion**, which has no timer.

### 4.3 The discipline, and its exact commands

All commands are **unverified** — no `az` CLI on this host. They are written against the module and
parameter names in `infra/`, and `<...>` marks a value you supply.

**Deploy (≈10–15 minutes, plus a first image push):**

```powershell
az group create -n sac-lab-run -l eastus

# The lab subset. Preferred: infra/main.bicep with the three `deploy*` flags off (see §5.1),
# which needs the six-line change to main.bicep. Until that lands, this command cannot be run
# as a subset, and deploying main.bicep as-is costs ≈$674/month (§7.7).
az deployment group create `
  -g sac-lab-run `
  -f infra/main.bicep `
  -p infra/params/lab.bicepparam
```

**Grant yourself database access — required, because the module creates no administrator:**

```powershell
# postgres.bicep sets passwordAuth: Disabled and activeDirectoryAuth: Enabled, and creates no
# Entra administrator, so the server has no usable credential until this runs.
az postgres flexible-server ad-admin create `
  -g sac-lab-run -s sac-lab-pg `
  -u sac-lab-admin -i <your-entra-object-id> --type User

# Inside the VNet only — see §4.4:
$env:PGPASSWORD = az account get-access-token --resource-type oss-rdbms --query accessToken -o tsv
psql "host=sac-lab-pg.postgres.database.azure.com dbname=sac user=sac-lab-admin sslmode=require" `
     -f db/schema.sql -v ON_ERROR_STOP=1
psql "... same ..." -f db/invariants.test.sql -v ON_ERROR_STOP=1
```

**Tear down — the recommended discipline, $0 floor:**

```powershell
az group delete -n sac-lab-run --yes --no-wait
```

**Or keep the database between two evenings — ≈$17.8/month floor:**

```powershell
az postgres flexible-server stop -g sac-lab-run -n sac-lab-pg   # saves $12.41/month; auto-starts after 7 days [AWAIT]
az containerapp list -g sac-lab-run -o table                    # confirm every app reports minReplicas 0
```

**Confirm nothing is still billing (do this, do not assume):**

```powershell
az resource list -g sac-lab-run -o table
az consumption usage list --query "[?contains(instanceName, 'sac-lab')].{name:instanceName, cost:pretaxCost}" -o table
```

### 4.4 Reaching a lab that is deliberately private

PostgreSQL is `publicNetworkAccess: Disabled` with a delegated subnet and a private DNS zone, the
blob account is `publicNetworkAccess: Disabled` behind a private endpoint, and the environment's load
balancer is internal. **Your laptop cannot reach any of it, and that is the point.** Three ways in,
in cost order:

| Way in | Cost | What it gives you |
|---|---|---|
| **A Container Apps Job** (reuse `container-app-job.bicep`) running a `postgres:16` image with `db/*.sql` baked in | ≈$0.03/month | the schema apply, the invariant suite, `psql` as the runtime roles, and an Entra-token connection — **from inside the VNet, over the same path the services use**. This is the recommended answer. |
| **A shell container app** (`minReplicas: 0`) + `az containerapp exec` | ≈$0.09/hour while you are in it | an interactive prompt inside the environment |
| **A B1s jump VM** in the VNet | ≈$16/month (VM $7.59 **[EST]** + OS disk ≈$4.80 **[EST]** + public IP ≈$3.65 **[EST]**) | a place to run the device agent and a browser against the APIs. **Lab-only**: `Microsoft.Compute/virtualMachines` is on `infra/inventory.json`'s deliberately-absent list for production. |

Not offered: **Azure Bastion** (≈$140/month **[EST]** — 4× the whole lab) and a **VPN Gateway**
(≈$26/month **[EST]** for Basic, plus setup). Both are correct tools for a private network and both
cost more than the thing they protect. If you need your laptop *inside* the VNet regularly, §7.6's
open mode is the cheaper answer, with the deviation it names.

### 4.5 The cost of forgetting, in the numbers that decide whether this lab is usable

| Left up by accident | Per week | Per month | What it means for someone working two evenings a week |
|---|---|---|---|
| **Recommended lab, all up** | **≈$7.30** | ≈$31.74 | less than one lunch; annoying, not damaging |
| Recommended lab, PostgreSQL stopped | ≈$4.08 | ≈$17.75 | the storage/registry/endpoint floor |
| Recommended lab, deleted | **$0.00** | **$0.00** | the recommended resting state |
| **`dev.bicepparam` deployed as a lab** | **≈$155** | ≈$674 | this is the mistake the question is really about |
| **`prod.eastus.bicepparam` deployed as a lab** | **≈$267** | ≈$1,161 | one forgotten week costs more than three years of the honest lab |
| + Managed HSM (per-contract only) | ≈$773 | $3,358 | never in a lab |

The recommended lab is **21× cheaper than deploying the `dev` parameter file**, and the reason is
not PostgreSQL. It is Front Door ($330/month) and the replica floors (≈$315/month at production's
replica minimums, ≈$197 at dev's) — see §9 F6.

The `prod` row needs its scope stated, because it is not the number in `infra/cost-model.md`: it is
**what one deployment of the production parameter file costs to run**, with no tenants in it —
dominated by the zone-redundant database pair ($392), Front Door ($331) and always-on replicas
($315). The per-tenant figure the repository quotes is a different question (allocation of shared
regional costs across tenants) and it is under review for reasons recorded in
`infra/COST-FINDING.md`, which §0 explains I do not inherit.

---

## 5. Reuse, replace, drop — against `infra/` module by module

The lab is a **parameterised subset**, not a fork. Every module below is imported unchanged; the
differences are parameter values, except where a note says otherwise.

| `infra/` module | Lab | Why |
|---|---|---|
| `modules/network.bicep` | **Reuse**, `privateDnsZones` reduced from 8 to 3 | Required: both `container-apps-env` and `postgres` need a subnet. A VNet is free; 3 zones cost $1.50 instead of $4.00. |
| `modules/log-analytics.bicep` | **Reuse**, `retentionInDays: 30`, `archiveRetentionInDays: 0`, **`dailyQuotaGb: 1`** | The daily cap is what turns a logging defect into a cost anomaly instead of a monthly surprise. |
| `modules/registry.bicep` | **Reuse**, `skuName: 'Basic'`, `geoReplicaLocation: ''`, `zoneRedundant: false`, no private endpoint | The module already allows all of this. **Deviation:** Basic cannot have a private endpoint (that needs Premium), so images are pulled over the public registry endpoint using managed identity. |
| `modules/keyvault.bicep` | **Reuse**, `skuName: 'standard'`, no private endpoint, purge protection on | The module allows `standard`. **Deviation:** `docs/05` §2 requires Premium (HSM-backed KEKs). See §6.4. |
| `modules/storage-ciphertext.bicep` | **Reuse**, `redundancy: 'LRS'`, **private endpoint ON**, `logAnalyticsWorkspaceId` set | The one Private Link the lab keeps, because requirement #4 is about the data path. `publicNetworkAccess: 'Disabled'` is hard-coded in the module, so with no endpoint the vault would have no path at all — the endpoint is cheaper than forking the module. |
| `modules/postgres.bicep` | **Reuse unchanged**, `B_Standard_B1ms` / `Burstable` / 32 GiB / `Disabled` HA / 7-day PITR / `geoRedundantBackup: false` | The dev ladder's own SKU. `publicNetworkAccess: 'Disabled'`, the delegated subnet and the private DNS zone are all preserved. |
| `modules/container-apps-env.bicep` | **Reuse unchanged**, `zoneRedundant: false`, `internalLoadBalancer: true` | Consumption-only → no fixed fee; internal LB preserved. |
| `modules/container-app.bicep` | **Reuse unchanged**, `minReplicas: 0`, `ingress: 'internal'` for content-vault | This module is the one that carries the architectural control, and it is used exactly as production uses it. |
| `modules/container-app-job.bicep` | **Reuse, repurposed** as the schema/invariants job | The aggregator does not exist; this module is the lab's only way to run a container inside the VNet on demand and pay nothing while it is not running. |
| `modules/budget.bicep` | **Reuse**, `amount: 40`, forecast alert on | The control that makes §4.3's accidental week visible within a day rather than on an invoice. |
| `modules/monitoring.bicep` | **Replace the 18-alert default with 2** (`alerts` is a parameter) | 18 rules ≈ $9/month for a lab nobody watches; 2 catch the two failures that make the lab useless. |
| `modules/private-endpoints.bicep` | **Reuse for the blob endpoint only** (invoked by `storage-ciphertext.bicep`) | Each endpoint is ≈$7.30/month. The lab keeps exactly one. |
| `modules/frontdoor.bicep` | **Drop** | $330/month for a lab with one user. Its properties are exactly what the lab cannot test (§6.5). |
| `modules/waf.bicep` | **Drop** | Exists only to feed Front Door. |
| `modules/static-web-app.bicep` | **Drop** | The dashboard is a static file in this repository. Deploying it adds a step and tests nothing. |
| `modules/storage-exports.bicep` | **Drop** | No export job exists; the path is untested in any environment. |
| `modules/managed-hsm.bicep` | **Drop** | $3,358/month, per-contract only. |
| `main.bicep` | **Parameterise, do not fork** — see §5.1 | A parallel composition drifts within a month; a `deploy*` flag does not. |

### 5.1 The one change this document asks of `infra/` (reported, not made)

`main.bicep` has no way to omit Front Door, the WAF, the dashboard or the export account — only
`deployManagedHsm` is conditional. A lab therefore needs either a **second composition**, which
drifts, or **three booleans in the existing one**:

```bicep
@description('Deploy the edge: Front Door Premium + WAF. true in every served environment; false only in a lab.')
param deployEdge bool = true

@description('Deploy the dashboard Static Web App. true in every served environment; false in a lab, where the dashboard is a file.')
param deployDashboard bool = true

@description('Deploy the export storage account. true in production; false in a lab and in dev.')
param deployExports bool = true
```

with `module frontDoor ... = if (deployEdge) { ... }`, the same for `waf`, `static-web-app` and
`storage-exports`, and `frontDoorHostName` becoming conditional. **That is the whole change**, and it
turns this document's §3.2 table into `infra/params/lab.bicepparam` — one parameter file, no second
tree, and `infra/tools/check-infra.mjs` keeps checking everything that matters (it asserts the
content-vault ingress and the private-only settings, which the lab does not touch).

I did not make this change: my write scope is `docs/lab/` only. It is finding **F9** in §9.

---

## 6. What the lab can and cannot test, property by property

Read this as the honest half of the document. A lab that tests half of these is genuinely useful; an
unstated half is how someone concludes "it works" from evidence that never covered it.

| Architecture property | Testable in Part B? | Why / what is missing |
|---|---|---|
| `db/schema.sql` applies unmodified on PostgreSQL 16 | **Yes** | Real Flexible Server 16 with `azure.extensions: PG_TRGM,BTREE_GIN`, `row_security: on`, `lock_timeout`, `statement_timeout` — the parameters `postgres.bicep` sets. Requires the Entra-admin step (§4.3), which the module omits. |
| `db/invariants.test.sql` passes | **Yes** | Run as the job (§4.4). This is the highest-value thing the Azure lab buys, because it is the one claim that a container cannot fully make. |
| **RLS as the runtime roles** (`sac_ingest`, `sac_query`, `sac_ops`, `sac_vault`) | **Yes** | The roles are created by the schema; the job's `psql` can `SET ROLE` and prove the predicate, the grants and the `FORCE ROW LEVEL SECURITY` behaviour on the managed service. This is the test that matters most and it needs a real server. |
| Entra ID authentication to PostgreSQL | **Yes** | `passwordAuth: 'Disabled'` is hard-coded in `postgres.bicep`, so the lab *must* use a token — the real path, not a password shortcut. |
| **Internal-only ingress for `content-vault`** | **Yes**, at both levels | `ingress: 'internal'` is asserted by the module, and the lab keeps `internalLoadBalancer: true`, so the environment-level property holds too. **No path from outside the VNet exists for any app**, which is the fidelity that §7.6 would trade away. |
| **Private blob storage** (no public access, no shared key, no public network path) | **Yes** | All three settings are preserved, and the private endpoint gives a real Private Link resource to approve. |
| **Managed identity** | **Partly** | The seven user-assigned identities deploy and can be role-assigned; a job or shell can acquire a token. **But** the services do not read their configuration from the environment (§9 F3–F4), so *the services' use of their identity* is untestable until that seam is closed. |
| **Key Vault** (RBAC, purge protection, soft delete 90 d) | **Yes**, with one deviation | The vault deploys and behaves. The lab uses the **Standard** SKU; production requires **Premium** (HSM-backed KEKs), and `content-vault`'s `--key-backend kms` is **not implemented in this build**, so nothing unwraps anything. |
| **Key rotation** | **No** | Two reasons: nothing in the build reads a key, and the HSM-backed key type needs Premium. Rotating a key nothing reads proves nothing. |
| **Private Link approval** | **Partly** | One endpoint with an auto-approval flow exercised once. Production has ~7 endpoints (PostgreSQL, two storage accounts, Key Vault, ACR, Log Analytics, Front Door origins) and a cross-subscription approval to rehearse. |
| **Front Door + WAF behaviour** | **No** | Dropped. No edge routing, no managed rule set, no custom rules, no rate limiting, no Private Link origin, no single public hostname. **This is the largest untestable area in the lab**, and it is the direct consequence of deleting a $330/month resource. |
| **Residency pinning** | **Partly** | One region is deployed, so "everything is in eastus" is observable. The *refusal* path (`ingest-api --region`) is a local code test. Multi-region failover is not testable at any lab price. |
| **Erasure receipts / retention expiry** | **Partly** | `ops.erasure_receipt`, its triggers and the erasure's effect on aggregates are all in the schema, so a scripted erasure can be exercised end to end **against the database**. The components that would perform one in production (retention job, reconciler) **do not exist in this repository**, so the *flow* is not testable anywhere. |
| **Aggregate freshness / the watermark** | **No** | The aggregator does not exist (§9 F8). |
| **Timed jobs** | **Yes (mechanism)** | `container-app-job.bicep` deploys and runs on a schedule; only the schema job uses it today. |
| **Backup and PITR restore** | **Partly** | 7-day PITR exists. A restore test costs a second server for as long as it exists (≈$16/month) and is worth doing exactly once, deliberately. |
| **Zone-redundant HA / failover** | **No** | `Disabled` in the lab. **[AWAIT]** Burstable may not support zone-redundant HA at all; if it does, enabling it roughly doubles line 1a. |
| **Managed HSM, quorum, customer-held keys** | **No** | $3,358/month and per-contract. |
| **Scale-out, concurrency limits, p95 under load** | **No** | One replica, no load generator: the lab tests correctness, never capacity. |
| **The device tier** | **Locally, not in Azure** | The agent runs on the Windows host; **Chromium is not installed**. With an internal load balancer, a device outside the VNet cannot reach `ingest-api` — see §7.6. |

### 6.1 The honest summary

**The lab proves:** the schema and its invariants on the real managed service; RLS and the grant
model as the runtime roles; Entra authentication; the private blob path; the internal-only ingress of
the one component that can unwrap content; that the deployment is reproducible from `infra/`.

**The lab does not prove:** anything about the edge (Front Door, WAF, rate limiting, private
origins); anything about scale or latency; key rotation; HSM custody; and — today — anything about
the services talking to PostgreSQL or Key Vault, because that code path is not implemented.

---

## 7. Cheaper alternatives considered, and what was rejected, and why

### 7.1 Local-only with Docker Compose — **adopted as Part A, not rejected**

It is $0, it runs the schema and the invariants against PostgreSQL 16, and it is where the browser
and device tiers can actually be exercised together. What it cannot prove is everything in §6 marked
"No" or "Partly" that depends on the *platform*: Entra authentication, managed identity, Key Vault
RBAC, the private blob path, the internal-load-balancer environment, and the managed service's own
parameter and extension surface. **Rejected as the only lab; adopted as the default one.**

### 7.2 Container Apps Consumption vs a single small VM — **Consumption wins**

A B1s Linux VM with a public IP and a 64 GiB disk is ≈$16/month **[EST]** and could run all three
services and a database in containers. It is *cheaper than the lab's fixed floor of $17.8* — and it
is the wrong answer, because a VM deletes precisely what the lab exists to test: managed identity,
Key Vault RBAC with no credential in the tree, the VNet-injected environment with an internal load
balancer, the private blob path, and Entra-authenticated PostgreSQL. It is also a resource the
production inventory lists as deliberately absent. **Kept only as an access host in §4.4, never as
the platform.**

### 7.3 Flexible Server Burstable B1ms vs containerised PostgreSQL in the lab — **both, for different jobs**

The container is $0 and is the **same engine**: PostgreSQL 16, `pg_trgm` and `btree_gin` available,
`gen_random_uuid()`, `sha256()`, generated `tsvector` columns, `ALTER DEFAULT PRIVILEGES`, partial
unique indexes, `FORCE ROW LEVEL SECURITY` — everything `db/schema.sql` needs, applied unmodified,
invariants included. That is why Part A is credible.

What a container **cannot** test is the part that is Azure rather than PostgreSQL: `azure.extensions`
allow-listing, `passwordAuth: Disabled` with Entra-token authentication, the delegated-subnet and
private-DNS path, server parameters enforced by the platform, PITR and backup, and the platform's own
`row_security` default. **B1ms at ≈$12.41/month is what buys exactly that**, and it is cheap enough
that the honest recommendation is both: container by default, managed server when testing the managed
properties.

### 7.4 Free-tier App Service for the static dashboard — **rejected**

App Service's Free tier exists ($0), and Azure Static Web Apps' Free tier is the production-shaped
answer if a hosted dashboard is ever wanted (production uses Standard, $9/month **[§11.1]**). For the
lab it is a step that tests nothing: `apps/dashboard/index.html` opens from the filesystem with no
build step and no server, and the API it would call is internal to the VNet anyway. **Rejected** —
with the note that the Static Web App module is already parameterised for `Free`, so a hosted
dashboard is a parameter change on the day it is needed.

### 7.5 Front Door **Standard** (~$35/month **[EST]**) instead of Premium ($330 **[§11.1]**) — **rejected**

Standard is roughly a tenth of the price and would exercise routing, caching and some WAF semantics.
It cannot front a **Private Link origin**, which means the origin would have to be public — the exact
property the architecture is built on, and the one that makes `content-vault`'s internal ingress
meaningful. **Rejected for fidelity.** If the only question is "does a Front Door route work at all",
Standard is the cheap way to answer it, and the lab should say in its notes that it was answering
that question and not the architecture's.

### 7.6 "Open mode": drop the internal load balancer to reach the APIs from the laptop — **offered, clearly labelled**

Setting `internalLoadBalancer: false` gives `ingest-api` and `query-api` public FQDNs, so the device
agent on this host and a browser can reach them. It costs **$0 extra** — it *saves* the ≈$16/month
jump VM — and it is a **real deviation**: the deployment stops proving "no container app holds a
public IP", and `content-vault`'s internal ingress becomes an app-level setting inside an
environment that has a public address. `docs/05` §2's own reasoning is that this weakens the control.
**Offer it as a separate, explicitly-named mode for endpoint-side work, never as the lab default, and
never as evidence for the vault's unreachability.**

### 7.7 Deploying `dev.bicepparam` and calling it a lab — **rejected, and this is the trap**

It is the obvious thing to do and it costs **≈$674/month** (≈$155/week): Front Door Premium $330,
warm replicas ≈$197 (four services at `minReplicas: 1`), ACR Premium + geo ≈$60, five private
endpoints ≈$37, eight DNS zones, 18 alerts, a Standard Static Web App, and a 32 GiB B2s database.
The production parameter file deployed the same way is **≈$1,161/month** (≈$267/week), because the
zone-redundant database pair alone is ≈$392 and the replica floor ≈$315.

The lab above is **21× cheaper than the dev file and 37× cheaper than the prod file**, and it keeps
every property that matters. The difference is almost entirely two decisions — **delete the edge,
and scale to zero** — neither of which is a code change.

### 7.8 Managed HSM — **rejected outright**

$3,358/month **[§11.6]**, per-contract only, and the capability it buys (vendor-blind custody,
2-of-3 quorum) cannot be exercised by any code in this repository. Never in a lab.

### 7.9 The free account and subscription credit — **check this first**

**[AWAIT]** The Azure free account's 12-month offer appears to include Flexible Server Burstable
B1ms (750 h/month) with 32 GiB of storage, and Container Apps' monthly free grant appears to cover
the lab's container compute; a Visual Studio Enterprise subscription appears to carry ≈$150/month of
Azure credit. If any of those applies, the lab's first year is ≈$14/month or less. **This is one
page of a subscription portal and it changes the answer more than any design decision in this
document**, so it is the first thing to check and I could not check it.

---

## 8. Migration path: lab to production without re-architecting

**What stays identical**, which is the whole reason to reuse `infra/` instead of forking it:

* every module — the same files that are deployed in the lab are deployed in production;
* `db/schema.sql` and the migration path, unmodified;
* the container images and the identity-per-service model;
* the ingress modes: `content-vault` internal, the rest external-via-Private-Link-origin;
* the private-only posture of PostgreSQL and blob storage, which the lab never relaxes;
* the parameter *names* — a lab parameter file and a production parameter file are the same shape.

**What changes, and only this:**

| Lab value | Production value | Where |
|---|---|---|
| ACR **Basic**, public pull, no geo-replication | Premium, private endpoint, geo-replicated to the paired region | `registry.bicep` parameters |
| Key Vault **Standard**, public endpoint | Premium (HSM-backed KEKs), private endpoint | `keyvault.bicep` parameters |
| 3 private DNS zones | 8 | `network.bicep` |
| 2 alerts | 18 (`docs/05` §10.3) | `monitoring.bicep` `alerts` |
| **No Front Door, no WAF** | Front Door Premium + WAF in Prevention | `deployEdge: true` |
| No Static Web App | Standard, custom domain, Entra auth | `deployDashboard: true` |
| No export storage account | ZRS export account with lifecycle rules | `deployExports: true` |
| PostgreSQL B1ms, 32 GiB, no HA, 7-day PITR | D2ds_v5, 128 GiB, zone-redundant HA, 35-day PITR | `postgres.bicep` parameters |
| `minReplicas: 0` on every app | 2/2/2/1 (the availability floor for 99.9%) | `container-app.bicep` parameters |
| LRS ciphertext, RA-GRS not used | RA-GRS, geo-redundant backup | parameter |
| Budget $40, no alert recipients | Budget sized per region, sev1/2/3 routing | `budget.bicep`, `monitoring.bicep` |

**Where the lab's shortcuts become defects if they shipped** — the list someone must read before
promoting anything:

1. **A public ACR pull.** Production requires a private endpoint and managed-identity pulls; a Basic
   registry's public endpoint is a supply-chain surface, not just a cost decision.
2. **Key Vault on the public endpoint, Standard SKU.** Production requires Premium for HSM-backed
   KEKs and a private endpoint. The `--key-backend kms` path must exist *and* be tested before this
   matters — today it does not exist at all (§9 F4).
3. **No edge.** Shipping without Front Door means no WAF in front of an endpoint that accepts
   traffic from machines the vendor does not control, no rate limiting sized for a fleet flush, and
   four public container apps instead of one public hostname.
4. **`minReplicas: 0`.** Correct for a lab, a defect in production: a cold start on the ingest path
   during a fleet flush, against a 99.9% availability floor. It is also the line that makes the
   production cost model's Container Apps figure look 10× smaller than the deployment implies
   (§9 F6).
5. **`internalLoadBalancer: false`** (open mode only). This is the one deviation that silently
   changes a security property rather than a cost one, and `infra/tools/check-infra.mjs` asserts the
   production value is `true`. Never promote a parameter file that sets it false.
6. **A hand-edited firewall rule or a `publicNetworkAccess` flipped for convenience.**
   `postgres.bicep` and `storage-ciphertext.bicep` hard-code the private values precisely so this
   cannot be done by accident, and the checker refuses a firewall-rule resource anywhere in the tree.
   If someone does it in the portal, the lab stops being evidence for the architecture — which is a
   statement about the lab's *conclusions*, not about the deployment.
7. **The lab parameter file itself.** It must never be the file a production deployment runs from.
   Name it `lab.bicepparam`, keep it in `infra/params/`, and let the `deploy*` flags default to
   `true` so a lab is opt-in rather than a production deployment being opt-out.

---

## 9. Findings to report (each needs a decision or an `infra/` change I may not make)

| # | Finding | Evidence |
|---|---|---|
| **F1** | **No Dockerfiles exist anywhere in the repository.** "The services run as containers" has no artifact yet. | `Get-ChildItem -Recurse -Filter Dockerfile*` → nothing |
| **F2** | **`container-app.bicep` cannot pass a command or arguments**, and both Go services are configured by **flags**, not environment variables. `targetPort` is 8080; `ingest-api` defaults to `127.0.0.1:8443` and `content-vault` to `127.0.0.1:8090` and **refuses a non-loopback bind** without `--allow-non-loopback`. A container built from these binaries would bind loopback on the wrong port and be unreachable. | `container-app.bicep` params; `services/*/cmd/*/main.go` flag definitions; `checkBindAddress` |
| **F3** | **`main.bicep` passes environment variables no binary reads** (`SAC_PG_HOST`, `SAC_ROLE`, `SAC_BLOB_CIPHERTEXT_ENDPOINT`, `SAC_KEYVAULT_URI`, `SAC_APPINSIGHTS`). content-vault reads `CONTENT_VAULT_KMS_ENDPOINT`, `CONTENT_VAULT_KMS_MODE`, `CONTENT_VAULT_SCOPE_TIERS`; ingest-api reads nothing. | `main.bicep` env blocks; `grep SAC_` over `services/**/*.go` |
| **F4** | **Neither service can talk to PostgreSQL or Key Vault.** No driver is compiled in (`sql.Register` absent; no pgx/lib/pq in either `go.mod`) and both refuse the SQL store; `--key-backend kms` is unimplemented and refuses to start. So **no Azure configuration, at any price, currently runs these services against the database or the vault.** | `content-vault/cmd/content-vault/main.go` (`--store sql`, `--key-backend kms` error paths) |
| **F5** | **`postgres.bicep` creates no Entra administrator**, while setting `passwordAuth: 'Disabled'`. The server has no usable credential until `az postgres flexible-server ad-admin create` runs, and nothing in the repository runs it — so the migration job that applies `db/schema.sql` could not connect either. | `postgres.bicep` `authConfig` (no `administrators` child resource) |
| **F6** | **Idle replicas bill.** `main.bicep` pins `apiMinReplicas`/`vaultMaxReplicas` floors of 1–2 while `docs/05` §2 says "Consumption costs nothing when idle". At `prod` defaults that is 4 vCPU + 8 GiB continuously ≈ **$315/month**; at `dev` defaults 2.5 vCPU + 5 GiB ≈ **$197/month**; against §11.2's $27 Container Apps line. **Needs invoice verification** — Container Apps billing granularity is the one behaviour I cannot check here. *This is not the Front Door question.* | `docs/05` §2 lines 58–62; `infra/params/*.bicepparam`; §11.1 unit prices |
| **F7** | **§11.3's "~$50 Container Apps environment" contradicts §2's "Consumption-only, no workload profiles"** (no fixed fee). The lab's total swings by $50/month on which is right. | `infra/cost-model.md` §11.3; `docs/05` §2 |
| **F8** | **The aggregator and reconciler do not exist.** The freshness watermark, the drift panel and the erasure flow are untestable in *any* environment, lab or production. | no `aggregator`/`reconciler` source anywhere in the tree |
| **F9** | **`main.bicep` needs three `deploy*` booleans** (§5.1) so the lab is a parameter file rather than a second composition. Six lines. | `main.bicep` — only `deployManagedHsm` is conditional |
| **F10** | **Chromium is not installed on this host**, so the extension half of the endpoint tier cannot be tested at all until it is. | local prerequisite, not an Azure one |

---

## 10. Appendix — the shape of the lab parameter file

Not created here (write scope is `docs/lab/`), and shown so the change is reviewable. Every value is
a module or composition parameter that already exists, except the three `deploy*` flags of §5.1.

```bicep
using '../main.bicep'

param location = 'eastus'
param environment = 'dev'
param baseName = 'sac-lab-eastus'
param tags = { environment: 'lab', workload: 'shadow-ai-capture', costCenter: 'lab' }

// The production shape, minus everything a lab cannot test.
param deployEdge = false          // no Front Door, no WAF: $330/month, and untestable anyway (§6)
param deployDashboard = false     // the dashboard is a file in this repository
param deployExports = false       // no export job exists
param deployManagedHsm = false    // $3,358/month, per-contract

// The database: the dev ladder's own SKU, still private-only.
param postgresSkuName = 'B_Standard_B1ms'
param postgresSkuTier = 'Burstable'
param postgresStorageGb = 32
param postgresHaMode = 'Disabled'
param postgresBackupRetentionDays = 7
param postgresAdministratorLogin = 'saclabadmin'   // a name; password auth is disabled (F5)
param postgresDatabases = ['sac']

// Scale to zero: the largest single cost decision in the lab (§4).
param apiMinReplicas = 0
param apiMaxReplicas = 2
param vaultMaxReplicas = 2

param ciphertextRedundancy = 'LRS'
param logRetentionDays = 30
param registryLoginServer = 'saclabacr.azurecr.io'
param imageTag = 'lab'

// Alerts route nowhere in a lab; the budget is the alert that matters.
param alertEmails = { sev1: [], sev2: [], sev3: [] }
param alertWebhooks = { sev1: [], sev2: [], sev3: [] }
param monthlyBudgetAmount = 40
```

And the companion parameters that live in the modules rather than the composition:
`registry.bicep` `skuName: 'Basic'` / `geoReplicaLocation: ''` / `zoneRedundant: false`;
`keyvault.bicep` `skuName: 'standard'` and no private-endpoint subnet;
`log-analytics.bicep` `dailyQuotaGb: 1` / `archiveRetentionInDays: 0`;
`network.bicep` `privateDnsZones` reduced to the three the lab uses;
`monitoring.bicep` `alerts` reduced to two.

---

## 11. Sources

Every claim in this document is sourced from a file in this repository, except the prices marked
**[EST]** and the behaviours marked **[AWAIT]**, which are general Azure knowledge and are labelled
because they cannot be checked here.

| Claim | File |
|---|---|
| Module parameter surfaces, ingress modes, private-only settings, compile-time defaults | `infra/modules/*.bicep` (17 modules) |
| Composition, identities, replica floors, env blocks, Front Door route set | `infra/main.bicep` |
| Deployment parameters, the dev/prod ladder | `infra/params/dev.bicepparam`, `staging.bicepparam`, `prod.eastus.bicepparam` |
| The §2 resource inventory, module-to-row mapping, deliberately-absent resources | `infra/inventory.json` |
| Unit prices, the recomputation, the findings | `infra/cost-model.md` |
| The Front Door contradiction (**not** inherited as fact) | `infra/COST-FINDING.md` |
| Service flags, store modes, key backends, bind guards, env variables | `services/ingest-api/cmd/ingest-api/main.go`, `services/content-vault/cmd/content-vault/main.go`, `services/*/go.mod` |
| The read path and the dashboard the lab serves | `services/query-api/`, `apps/dashboard/` |
| Schema and invariants the lab must run unmodified | `db/schema.sql`, `db/invariants.test.sql` |
| Consumed-only environment, replica floors, the "costs nothing when idle" claim, the private-origin requirement | `docs/05-platform-delivery.md` §2 |
| The data the dashboard shows and the states it must keep | `docs/04-dashboard-and-query.md` |
