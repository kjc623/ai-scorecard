// lab.bicepparam — the architecture-fidelity lab (docs/lab/LAB-COST.md §1, Part B).
//
// This is a PARAMETER FILE, not a second composition. The point of that distinction is drift: a
// parallel Bicep tree diverges from production within a month, whereas a file that sets switches
// on the same modules cannot diverge — if main.bicep changes, this file deploys the change.
//
// It exists because the honest lab and the smallest production environment are not the same thing
// and should not be confused: deploying dev.bicepparam as-is costs ≈$674/month, of which Front Door
// alone is $330. The switches below remove what a lab does not need rather than shrinking what it
// does.
//
// NOT VERIFIED: nothing here has been deployed. There is no subscription, no az CLI and no network
// on the machine this was written on, so every figure is a list-price estimate and every property
// below is a design claim rather than an observation. What the lab is expected to prove, and what
// it cannot, is docs/lab/LAB-COST.md §6.

using '../main.bicep'

param location = 'eastus'
param environment = 'dev'
param baseName = 'sac-lab-eastus'

param tags = {
  environment: 'lab'
  workload: 'shadow-ai-capture'
  costCenter: 'platform-engineering'
}

// ---------------------------------------------------------------------------------------------
// Scope: the four switches that make this a lab rather than a deployment.
//
//   deployEdge      false — Front Door Premium ($330/month base) plus its WAF. The largest single
//                           line in the bill and worth more than the whole lab; a lab is reached
//                           over the VNet. CONSEQUENCE: the lab CANNOT test edge routing, the WAF
//                           managed rule set, rate limiting, or a Private Link origin. That is
//                           stated in docs/lab/LAB-COST.md §6 rather than left to be discovered.
//   deployDashboard false — the dashboard is a static file (apps/dashboard/index.html). A Static
//                           Web App is for delivering it, which a lab does by opening the file.
//   deployExports   false — the second storage account for customer-facing columnar exports. No
//                           reader in a lab.
//   deployManagedHsm false — ~$3,360/month per pool, and only a contract requiring
//                           customer-held HSM-resident keys needs it.
// ---------------------------------------------------------------------------------------------
param deployEdge = false
param deployDashboard = false
param deployExports = false
param deployManagedHsm = false

// ---------------------------------------------------------------------------------------------
// Keep the properties the lab exists to exercise.
//
// PostgreSQL: Burstable B1ms with 32 GiB and 7-day PITR. Deliberately NOT a smaller substitute —
//   the schema's forced row-level security, partial unique indices, generated tsvector columns and
//   the single-use trigger on ops.retrieval_grant all require real PostgreSQL 16, which is the
//   reason a containerised Postgres is a different lab (docs/lab/LAB-COST.md §3) rather than a
//   cheaper one.
// publicNetworkAccess stays Disabled and no firewall rule exists anywhere in the modules, so the
//   "database is not reachable from outside the VNet" property is preserved at any size.
// ---------------------------------------------------------------------------------------------
param postgresSkuName = 'B_Standard_B1ms'
param postgresSkuTier = 'Burstable'
param postgresStorageGb = 32
param postgresHaMode = 'Disabled'
param postgresBackupRetentionDays = 7
param postgresAdministratorLogin = 'saclabadmin'
param postgresDatabases = ['sac']

// Replica floors are the second-largest avoidable line: main.bicep pins min 1-2 while §2 says
// "Consumption costs nothing when idle". A lab scales to zero between sessions.
param apiMinReplicas = 0
param apiMaxReplicas = 2
param vaultMaxReplicas = 1

param ciphertextRedundancy = 'LRS'
param wafMode = 'Detection'
param logRetentionDays = 30

param registryLoginServer = 'saclabeastusacr.azurecr.io'
param imageTag = 'lab'

param alertEmails = {}
param alertWebhooks = {}

// A budget alert, not a cap: the resource that surprises people is the one left running. Delete the
// resource group between sessions — a stopped Flexible Server auto-starts on some plans, so
// "stopped" is not a resting state (docs/lab/LAB-COST.md §4).
param monthlyBudgetAmount = 40
