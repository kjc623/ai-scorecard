// dev.bicepparam — the breakable environment (docs/05-platform-delivery.md §3.1, §3.2).
//
// The module templates are identical in every environment; only this file differs. No secret lives
// here: the PostgreSQL break-glass login is a name, and its credential is supplied at deploy time
// from Key Vault (§5.1).

using '../main.bicep'

param location = 'eastus'
param environment = 'dev'
param baseName = 'sac-dev-eastus'

param tags = {
  environment: 'dev'
  workload: 'shadow-ai-capture'
  costCenter: 'platform-engineering'
}

// Dev is intentionally small and non-redundant: the whole point of the ladder is that a failure here
// costs nothing (§3.1).
param postgresSkuName = 'B_Standard_B2s'
param postgresSkuTier = 'Burstable'
param postgresStorageGb = 32
param postgresHaMode = 'Disabled'
param postgresBackupRetentionDays = 7
param postgresAdministratorLogin = 'sacdevadmin'
param postgresDatabases = ['sac']

param apiMinReplicas = 1
param apiMaxReplicas = 2
param vaultMaxReplicas = 2

param ciphertextRedundancy = 'LRS'
param wafMode = 'Detection'
param logRetentionDays = 30

// Dev never deploys the HSM pool, and never has three administrators to give it.
param deployManagedHsm = false

param registryLoginServer = 'sacdeveastusacr.azurecr.io'
param imageTag = 'dev'

// Dev routes alerts nowhere: an alert that pages nobody in dev still trains people to ignore alerts.
param alertEmails = {
  sev1: []
  sev2: []
  sev3: []
}
param alertWebhooks = {
  sev1: []
  sev2: []
  sev3: []
}

param monthlyBudgetAmount = 250
