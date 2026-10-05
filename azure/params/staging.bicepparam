// staging.bicepparam — the promotion gate and rehearsal environment (docs/05-platform-delivery.md
// §3.1, §3.2).
//
// Production-shaped volume and skew, production SKU, no HA: a rehearsal that runs on a different
// shape than production is a rehearsal of something else. Nothing is promoted out of staging until
// the full contract suite, the migration rehearsal against a production-shaped row count, the load
// test at 500 events/s burst and the WAF false-positive pass are green (§3.1).

using '../main.bicep'

param location = 'eastus'
param environment = 'staging'
param baseName = 'sac-staging-eastus'

param tags = {
  environment: 'staging'
  workload: 'shadow-ai-capture'
  costCenter: 'platform-engineering'
}

param postgresSkuName = 'D2ds_v5'
param postgresSkuTier = 'GeneralPurpose'
param postgresStorageGb = 64
param postgresHaMode = 'Disabled'
param postgresBackupRetentionDays = 14
param postgresAdministratorLogin = 'sacstagingadmin'
param postgresDatabases = ['sac']

param apiMinReplicas = 2
param apiMaxReplicas = 5
param vaultMaxReplicas = 2

param ciphertextRedundancy = 'ZRS'
param wafMode = 'Prevention'
param logRetentionDays = 90

// The device edge (Application Gateway) public hostname. Placeholder until a real DNS name exists.
param deviceFqdn = 'device.staging.sac.example.com'

// The browser and identity-provider edge (Front Door custom domain): sign-in callbacks, onboarding
// links and the SCIM base URL are built on it. Placeholder until a real DNS name exists.
param publicUrl = 'https://app.staging.sac.example.com'

param deployManagedHsm = false

param registryLoginServer = 'sacstagingeastusacr.azurecr.io'
param imageTag = 'staging'

// Staging alerts go to a non-paging channel: the rehearsal must exercise the alert logic without
// waking anyone (§3.2).
param alertEmails = {
  sev1: ['platform-engineering@example.invalid']
  sev2: ['platform-engineering@example.invalid']
  sev3: []
}
param alertWebhooks = {
  sev1: []
  sev2: []
  sev3: []
}

param monthlyBudgetAmount = 900
