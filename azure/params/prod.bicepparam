// Production, East US: zone-redundant PostgreSQL, gateway and environment; geo-replicated registry.
using '../main.bicep'

// Values that belong to the environment's external setup come from the environment: the deploy
// workflow sets them from the GitHub environment's variables (see azure/README.md).
param bootstrap = bool(readEnvironmentVariable('SAC_BOOTSTRAP', 'false'))
param imageTag = readEnvironmentVariable('SAC_IMAGE_TAG', '')
param keyVaultAdminIpRules = filter(split(readEnvironmentVariable('SAC_KEYVAULT_ADMIN_IPS', ''), ','), e => !empty(e))
param keyVaultAdminPrincipalIds = filter(split(readEnvironmentVariable('SAC_KEYVAULT_ADMIN_PRINCIPALS', ''), ','), e => !empty(e))

param location = 'eastus'
param environment = 'prod'
param baseName = 'sac-prod-eastus'
param tags = {
  environment: 'prod'
  workload: 'shadow-ai-capture'
}

param deviceFqdn = readEnvironmentVariable('SAC_DEVICE_FQDN')
param analystFqdn = readEnvironmentVariable('SAC_ANALYST_FQDN')
param entraAppClientId = readEnvironmentVariable('SAC_ENTRA_APP_CLIENT_ID')
param policySigningKeyId = 'policy-key-1'

param postgresSkuName = 'Standard_D2ds_v5'
param postgresSkuTier = 'GeneralPurpose'
param postgresStorageGb = 128

param apiMinReplicas = 2
param apiMaxReplicas = 20
param registryGeoReplicaLocation = 'centralus'

param alertEmails = filter(split(readEnvironmentVariable('SAC_ALERT_EMAILS'), ','), e => !empty(e))
param monthlyBudgetAmount = 6000
