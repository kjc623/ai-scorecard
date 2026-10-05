// prod.eastus.bicepparam — live tenancy for one residency region (docs/05-platform-delivery.md
// §2, §3.1, §3.2).
//
// One production environment per data-residency region, not one global production: this file is
// instantiated per region, and a second region is a second file rather than a second template. The
// region is a parameter here and nowhere else, which is what lets residency be reviewed by reading
// this file.

using '../main.bicep'

param location = 'eastus'
param environment = 'prod'
param baseName = 'sac-prod-eastus'

param tags = {
  environment: 'prod'
  workload: 'shadow-ai-capture'
  costCenter: 'platform-engineering'
  residencyRegion: 'eastus'
}

// The recommended production configuration: zone-redundant HA is the availability floor for 99.9%
// ingest, and 35 days of PITR covers an investigation that starts late (§12.2).
param postgresSkuName = 'D2ds_v5'
param postgresSkuTier = 'GeneralPurpose'
param postgresStorageGb = 128
param postgresHaMode = 'ZoneRedundant'
param postgresBackupRetentionDays = 35
param postgresAdministratorLogin = 'sacprodadmin'
param postgresDatabases = ['sac']

param apiMinReplicas = 2
param apiMaxReplicas = 20
param vaultMaxReplicas = 4

param ciphertextRedundancy = 'RA-GRS'
param wafMode = 'Prevention'
param logRetentionDays = 90

// The device edge (Application Gateway) public hostname. Placeholder until a real DNS name exists.
param deviceFqdn = 'device.sac.example.com'

// The browser and identity-provider edge (Front Door custom domain): sign-in callbacks, onboarding
// links and the SCIM base URL are built on it. Placeholder until a real DNS name exists.
param publicUrl = 'https://app.sac.example.com'

// Per-contract only: the HSM pool is ~4× the rest of a tenant, and the three administrators are
// named in the contract that requires it (§2, §5.3). Default false; a contract turns it on.
param deployManagedHsm = false

param registryLoginServer = 'sacprodeastusacr.azurecr.io'
param registryGeoReplicaLocation = 'centralus'
param imageTag = 'prod'

param alertEmails = {
  sev1: ['oncall@example.invalid']
  sev2: ['platform-engineering@example.invalid']
  sev3: ['platform-engineering@example.invalid']
}
param alertWebhooks = {
  sev1: ['https://pager.example.invalid/hooks/sac-sev1']
  sev2: []
  sev3: []
}

param monthlyBudgetAmount = 12000
