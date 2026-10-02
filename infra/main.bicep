// main.bicep — the composition (docs/05-platform-delivery.md §3.3).
//
// Thin and environment-specific: the modules own one resource family each, all wiring happens here,
// and no module reaches into another module's resources with `existing`, so a module's inputs are
// visible in one place. Everything an environment can differ on is a parameter (§3.2); everything
// the document fixes (the routes, the ingress modes, the private-only properties) is stated here in
// the clear so a reviewer can check it by reading this file alone.
//
// Two properties this file is responsible for, and the checker asserts both:
//   * content-vault is instantiated with ingress: internal — no public endpoint, no Front Door
//     route (D7, C15). The module takes no default for ingress, so this cannot be forgotten.
//   * nothing here reaches PostgreSQL or a storage account over a public path: the private-only
//     settings live in their modules and the checker refuses a public value anywhere in the tree.

targetScope = 'resourceGroup'

// ---------------------------------------------------------------------------------------------
// Environment identity

@description('Azure region for the environment. §3.1: one production environment per data-residency region; the value is never compiled in.')
param location string

@description('Environment name.')
@allowed(['dev', 'staging', 'prod'])
param environment string

@description('Base name for every resource in the environment, e.g. sac-prod-eastus. Names are derived, never typed per resource.')
param baseName string

@description('Tags applied to every resource. Cost allocation, drift reporting and the §10.3 coverage alerts all scope by tag.')
param tags object = {}

// ---------------------------------------------------------------------------------------------
// Sizing that differs per environment (§3.1, §3.2)

@description('PostgreSQL compute SKU. B_Standard_B2s in dev, D2ds_v5 in staging and production.')
param postgresSkuName string

@description('PostgreSQL compute tier.')
@allowed(['Burstable', 'GeneralPurpose', 'MemoryOptimized'])
param postgresSkuTier string

@description('PostgreSQL provisioned storage in GiB: 32 dev, 64 staging, 128 production.')
param postgresStorageGb int

@description('PostgreSQL high-availability mode: Disabled in dev and staging, ZoneRedundant in production (the availability floor for 99.9% ingest).')
@allowed(['Disabled', 'ZoneRedundant'])
param postgresHaMode string

@description('PostgreSQL PITR retention in days: 7 dev, 14 staging, 35 production.')
@minValue(7)
@maxValue(35)
param postgresBackupRetentionDays int

@description('PostgreSQL administrator login for break-glass. The credential is supplied at deploy time from Key Vault; password authentication is disabled on the server.')
param postgresAdministratorLogin string

@description('PostgreSQL databases to create.')
param postgresDatabases array = ['sac']

@description('Container image tag or digest deployed to every revision. The pipeline resolves a digest; the composition never builds one.')
param imageTag string

@description('Minimum replicas for the API services: 1 dev, 2 staging and production.')
param apiMinReplicas int = 2

@description('Maximum replicas for the API services: 2 dev, 5 staging, 20 production.')
param apiMaxReplicas int = 20

@description('content-vault maximum replicas: 4 per §2 inventory; it has no user-facing endpoint, so it scales on queue depth rather than concurrency.')
param vaultMaxReplicas int = 4

@description('Blob redundancy for the ciphertext account: LRS dev, ZRS staging, RA-GRS production (§12.3).')
@allowed(['LRS', 'ZRS', 'GRS', 'RA-GRS'])
param ciphertextRedundancy string = 'RA-GRS'

@description('WAF mode: Detection dev, Prevention staging and production.')
@allowed(['Detection', 'Prevention'])
param wafMode string = 'Prevention'

@description('Log Analytics interactive retention in days: 30 dev, 90 staging and production.')
param logRetentionDays int = 90

@description('Whether to deploy the Managed HSM pool. Per-contract only (§2, §11.6); false unless a customer contract requires vendor-blind key custody.')
param deployManagedHsm bool = false

@description('Managed HSM administrator object ids. Three, so the 2-of-3 quorum means the vendor alone cannot satisfy the policy.')
param hsmAdministratorObjectIds array = []

@description('Registry login server, e.g. sacprodacr.azurecr.io.')
param registryLoginServer string

@description('Paired region for Container Registry geo-replication, e.g. centralus for eastus. Empty disables replication: geo-replication is what makes regional failover a redeploy rather than a rebuild (§12.1), and the paired region is a per-environment decision, which is why it is parameterised rather than written into the module.')
param registryGeoReplicaLocation string = ''

@description('Static Web App custom domain for the dashboard. Empty in dev.')
param dashboardCustomDomain string = ''

@description('Entra ID client id for dashboard authentication. A public client identifier, not a credential.')
param dashboardEntraClientId string = ''

@description('Alert email recipients per severity.')
param alertEmails object = {
  sev1: []
  sev2: []
  sev3: []
}

@description('Alert webhook recipients per severity.')
param alertWebhooks object = {
  sev1: []
  sev2: []
  sev3: []
}

@description('Monthly budget for this environment’s resource group, in the billing currency.')
param monthlyBudgetAmount int = 1000

// ---------------------------------------------------------------------------------------------
// Managed identities (§5.1). Every service runs as a user-assigned identity; there is no shared
// secret anywhere in this file, and the grants are assigned per identity by the migration and by
// role-assignment tooling, not by a connection string.

resource identityIngest 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-ingest'
  location: location
  tags: tags
}

resource identityControl 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-control'
  location: location
  tags: tags
}

resource identityVault 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-content-vault'
  location: location
  tags: tags
}

resource identityQuery 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-query'
  location: location
  tags: tags
}

resource identityAggregator 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-aggregator'
  location: location
  tags: tags
}

resource identityReconciler 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-reconciler'
  location: location
  tags: tags
}

@description('DDL only, assumed by the migration job for the duration of a migration and by nothing else (§5.4). It cannot be assumed by an app.')
resource identityMigration 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-migration'
  location: location
  tags: tags
}

// ---------------------------------------------------------------------------------------------
// Platform

module network 'modules/network.bicep' = {
  name: 'network'
  params: {
    location: location
    environment: environment
    baseName: baseName
    tags: tags
  }
}

module logAnalytics 'modules/log-analytics.bicep' = {
  name: 'log-analytics'
  params: {
    location: location
    baseName: baseName
    retentionInDays: logRetentionDays
    archiveRetentionInDays: environment == 'dev' ? 0 : 365
    traceSamplingPercentage: 10
    tags: tags
  }
}

module registry 'modules/registry.bicep' = {
  name: 'registry'
  params: {
    location: location
    baseName: baseName
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    privateDnsZoneIds: network.outputs.privateDnsZoneIds
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    geoReplicaLocation: registryGeoReplicaLocation
    tags: tags
  }
}

module keyVault 'modules/keyvault.bicep' = {
  name: 'keyvault'
  params: {
    location: location
    baseName: baseName
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    privateDnsZoneIds: network.outputs.privateDnsZoneIds
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    operationalRoleAssignments: [
      {
        name: 'control-api'
        principalId: identityControl.properties.principalId
        roleDefinitionId: 'secretsUser'
      }
      {
        name: 'ingest-api'
        principalId: identityIngest.properties.principalId
        roleDefinitionId: 'cryptoServiceEncryption'
      }
    ]
    // Unwrap is held by exactly one identity: content-vault (C15, D7). This one-line list is the whole
    // separation of duties, which is why the checker asserts no other principal appears here.
    unwrapPrincipalIds: [
      identityVault.properties.principalId
    ]
    tags: tags
  }
}

module ciphertext 'modules/storage-ciphertext.bicep' = {
  name: 'storage-ciphertext'
  params: {
    location: location
    baseName: baseName
    redundancy: ciphertextRedundancy
    softDeleteRetentionDays: 30
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    privateDnsZoneIds: network.outputs.privateDnsZoneIds
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    tags: tags
  }
}

module exports 'modules/storage-exports.bicep' = {
  name: 'storage-exports'
  params: {
    location: location
    baseName: baseName
    redundancy: environment == 'prod' ? 'ZRS' : 'LRS'
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    privateDnsZoneIds: network.outputs.privateDnsZoneIds
    tags: tags
  }
}

module postgres 'modules/postgres.bicep' = {
  name: 'postgres'
  params: {
    location: location
    baseName: baseName
    skuName: postgresSkuName
    skuTier: postgresSkuTier
    storageSizeGb: postgresStorageGb
    highAvailabilityMode: postgresHaMode
    backupRetentionDays: postgresBackupRetentionDays
    geoRedundantBackup: environment != 'dev'
    administratorLogin: postgresAdministratorLogin
    delegatedSubnetId: network.outputs.containerAppsSubnetId
    privateDnsZoneId: network.outputs.privateDnsZoneIds['privatelink.postgres.database.azure.com']
    databases: postgresDatabases
    tags: tags
  }
}

module managedHsm 'modules/managed-hsm.bicep' = {
  name: 'managed-hsm'
  params: {
    location: location
    baseName: baseName
    hsmSku: 'Custom_B32'
    administratorObjectIds: hsmAdministratorObjectIds
    deployManagedHsm: deployManagedHsm && length(hsmAdministratorObjectIds) == 3
    tags: tags
  }
}

module containerAppsEnv 'modules/container-apps-env.bicep' = {
  name: 'container-apps-env'
  params: {
    location: location
    baseName: baseName
    infrastructureSubnetId: network.outputs.containerAppsSubnetId
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    zoneRedundant: environment == 'prod'
    tags: tags
  }
}

// Private endpoints for the resources that do not create their own: Log Analytics, and Managed HSM
// when a contract needs it (§2: "one endpoint per PaaS resource").
module platformPrivateEndpoints 'modules/private-endpoints.bicep' = {
  name: 'platform-private-endpoints'
  params: {
    location: location
    baseName: baseName
    subnetId: network.outputs.privateEndpointSubnetId
    privateDnsZoneIds: network.outputs.privateDnsZoneIds
    targets: concat([
      {
        name: 'log-analytics'
        resourceId: logAnalytics.outputs.workspaceId
        groupId: 'azuremonitor'
        dnsZoneName: 'privatelink.monitor.azure.com'
      }
    ], deployManagedHsm ? [
      {
        name: 'managed-hsm'
        resourceId: managedHsm.outputs.hsmId
        groupId: 'managedhsm'
        dnsZoneName: 'privatelink.managedhsm.azure.net'
      }
    ] : [])
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// The four services. They differ in identity, scale and ingress, and in nothing else (§3.3).

module ingestApp 'modules/container-app.bicep' = {
  name: 'ingest-api'
  params: {
    location: location
    appName: 'ingest-api'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityIngest.id
    ingress: 'external'
    targetPort: 8080
    cpu: '0.5'
    memory: '1Gi'
    minReplicas: apiMinReplicas
    maxReplicas: apiMaxReplicas
    image: '${registryLoginServer}/ingest-api:${imageTag}'
    registryLoginServer: registryLoginServer
    env: [
      { name: 'SAC_ROLE', value: 'ingest-api' }
      { name: 'SAC_PG_HOST', value: postgres.outputs.serverFqdn }
      { name: 'SAC_PG_DATABASE', value: postgresDatabases[0] }
      { name: 'SAC_BLOB_CIPHERTEXT_ENDPOINT', value: ciphertext.outputs.blobEndpoint }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
    ]
    keyVaultEnv: []
    tags: tags
  }
}

module controlApp 'modules/container-app.bicep' = {
  name: 'control-api'
  params: {
    location: location
    appName: 'control-api'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityControl.id
    ingress: 'external'
    targetPort: 8080
    cpu: '0.5'
    memory: '1Gi'
    minReplicas: apiMinReplicas
    maxReplicas: environment == 'dev' ? 2 : 10
    image: '${registryLoginServer}/control-api:${imageTag}'
    registryLoginServer: registryLoginServer
    env: [
      { name: 'SAC_ROLE', value: 'control-api' }
      { name: 'SAC_PG_HOST', value: postgres.outputs.serverFqdn }
      { name: 'SAC_PG_DATABASE', value: postgresDatabases[0] }
      { name: 'SAC_KEYVAULT_URI', value: keyVault.outputs.vaultUri }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
    ]
    keyVaultEnv: []
    tags: tags
  }
}

// content-vault. **ingress: internal** — the structural control of C15/D7. It has no user-facing
// endpoint at all: neither a device nor a browser can reach it, and Front Door has no route to it.
// The module takes no default for ingress, so this value cannot be omitted by accident.
module contentVaultApp 'modules/container-app.bicep' = {
  name: 'content-vault'
  params: {
    location: location
    appName: 'content-vault'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityVault.id
    ingress: 'internal'
    targetPort: 8080
    cpu: '1.0'
    memory: '2Gi'
    minReplicas: 1
    maxReplicas: vaultMaxReplicas
    image: '${registryLoginServer}/content-vault:${imageTag}'
    registryLoginServer: registryLoginServer
    env: [
      { name: 'SAC_ROLE', value: 'content-vault' }
      { name: 'SAC_PG_HOST', value: postgres.outputs.serverFqdn }
      { name: 'SAC_PG_DATABASE', value: postgresDatabases[0] }
      { name: 'SAC_BLOB_CIPHERTEXT_ENDPOINT', value: ciphertext.outputs.blobEndpoint }
      { name: 'SAC_KEYVAULT_URI', value: keyVault.outputs.vaultUri }
      { name: 'SAC_INTERNAL_ONLY', value: 'true' }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
    ]
    keyVaultEnv: []
    tags: tags
  }
}

module queryApp 'modules/container-app.bicep' = {
  name: 'query-api'
  params: {
    location: location
    appName: 'query-api'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityQuery.id
    ingress: 'external'
    targetPort: 8080
    cpu: '0.5'
    memory: '1Gi'
    minReplicas: apiMinReplicas
    maxReplicas: environment == 'dev' ? 2 : 10
    image: '${registryLoginServer}/query-api:${imageTag}'
    registryLoginServer: registryLoginServer
    env: [
      { name: 'SAC_ROLE', value: 'query-api' }
      { name: 'SAC_PG_HOST', value: postgres.outputs.serverFqdn }
      { name: 'SAC_PG_DATABASE', value: postgresDatabases[0] }
      { name: 'SAC_CONTENT_VAULT_URL', value: 'http://${contentVaultApp.outputs.fqdn}' }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
    ]
    keyVaultEnv: []
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// The jobs. One replica each: the work is set-based SQL over the same buckets (§2).

module aggregatorJob 'modules/container-app-job.bicep' = {
  name: 'aggregator'
  params: {
    location: location
    jobName: 'aggregator'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityAggregator.id
    image: '${registryLoginServer}/aggregator:${imageTag}'
    registryLoginServer: registryLoginServer
    cronExpression: '*/5 * * * *'
    args: [
      'aggregate'
    ]
    env: [
      { name: 'SAC_ROLE', value: 'aggregator' }
      { name: 'SAC_PG_HOST', value: postgres.outputs.serverFqdn }
      { name: 'SAC_PG_DATABASE', value: postgresDatabases[0] }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
    ]
    tags: tags
  }
}

module reconcilerJob 'modules/container-app-job.bicep' = {
  name: 'reconciler'
  params: {
    location: location
    jobName: 'reconciler'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityReconciler.id
    image: '${registryLoginServer}/reconciler:${imageTag}'
    registryLoginServer: registryLoginServer
    cronExpression: '0 2 * * *'
    args: [
      'reconcile'
    ]
    env: [
      { name: 'SAC_ROLE', value: 'reconciler' }
      { name: 'SAC_PG_HOST', value: postgres.outputs.serverFqdn }
      { name: 'SAC_PG_DATABASE', value: postgresDatabases[0] }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
    ]
    tags: tags
  }
}

// The migration job runs under its own identity (§5.4): DDL only, and only for the duration of a
// migration. It carries the migration image, not a service image, and the pipeline starts it before
// the new revision takes traffic (§3.4).
module migrationJob 'modules/container-app-job.bicep' = {
  name: 'migration'
  params: {
    location: location
    jobName: 'migration'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityMigration.id
    image: '${registryLoginServer}/migrations:${imageTag}'
    registryLoginServer: registryLoginServer
    cronExpression: '0 0 1 1 *' // annually: the job is started manually by the pipeline, never by its schedule
    args: [
      'migrate'
    ]
    env: [
      { name: 'SAC_ROLE', value: 'migration' }
      { name: 'SAC_PG_HOST', value: postgres.outputs.serverFqdn }
      { name: 'SAC_PG_DATABASE', value: postgresDatabases[0] }
      // §3.4: a DDL lock must not stall ingest.
      { name: 'SAC_PG_LOCK_TIMEOUT_MS', value: '5000' }
      { name: 'SAC_PG_STATEMENT_TIMEOUT_MS', value: '30000' }
    ]
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// Edge, dashboard, monitoring and budget

module waf 'modules/waf.bicep' = {
  name: 'waf'
  params: {
    wafMode: wafMode
    tags: tags
  }
}

module frontDoor 'modules/frontdoor.bicep' = {
  name: 'frontdoor'
  params: {
    wafPolicyId: waf.outputs.wafPolicyId
    environmentId: containerAppsEnv.outputs.environmentId
    environmentFqdn: ingestApp.outputs.fqdn
    originHostHeaders: {
      'ingest-api': ingestApp.outputs.fqdn
      'control-api': controlApp.outputs.fqdn
      'query-api': queryApp.outputs.fqdn
    }
    tags: tags
  }
}

module dashboard 'modules/static-web-app.bicep' = {
  name: 'static-web-app'
  params: {
    location: location
    baseName: baseName
    customDomain: dashboardCustomDomain
    entraClientId: dashboardEntraClientId
    tags: tags
  }
}

module monitoring 'modules/monitoring.bicep' = {
  name: 'monitoring'
  params: {
    location: location
    baseName: baseName
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    appInsightsId: logAnalytics.outputs.appInsightsId
    emailReceivers: alertEmails
    webhookReceivers: alertWebhooks
    tags: tags
  }
}

module budget 'modules/budget.bicep' = {
  name: 'budget'
  params: {
    baseName: baseName
    amount: monthlyBudgetAmount
    resourceGroupFilter: resourceGroup().id
    contactEmails: alertEmails.sev1
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// The assertions a reviewer should be able to read here rather than derive.
//
// content-vault's ingress mode is echoed by the module, so this composition can state the property
// as a value instead of a comment; the CI policy scan (infra/pipelines/policy-scan.yml) fails the
// build if it is ever external, and infra/tools/check-infra.mjs asserts it statically.

@description('The one public hostname in the deployment. Everything else is private or internal.')
output frontDoorHostName string = frontDoor.outputs.endpointHostName

@description('content-vault ingress mode. Must be "internal": it has no public endpoint and no Front Door route (C15, D7).')
output contentVaultIngress string = contentVaultApp.outputs.ingressMode

@description('The internal FQDN of content-vault. It is reachable only from inside the environment, which is why query-api holds its URL and nothing else does.')
output contentVaultInternalFqdn string = contentVaultApp.outputs.fqdn

@description('The PostgreSQL FQDN. It resolves only through the private DNS zone linked to the VNet, and the server accepts no public connection.')
output postgresFqdn string = postgres.outputs.serverFqdn

@description('Whether zone-redundant HA is on. Production must be true (§3.1).')
output postgresZoneRedundantHa bool = postgres.outputs.zoneRedundantHa

@description('The ciphertext account endpoint, reachable only over its private endpoint.')
output ciphertextBlobEndpoint string = ciphertext.outputs.blobEndpoint

@description('The identities and what each may do. The grants themselves are assigned by role-assignment tooling per §5.1; this output exists so the inventory of identities is reviewable from the deployment.')
output identities object = {
  ingest: identityIngest.id
  control: identityControl.id
  contentVault: identityVault.id
  query: identityQuery.id
  aggregator: identityAggregator.id
  reconciler: identityReconciler.id
  migration: identityMigration.id
}

@description('Whether the Managed HSM pool was deployed. Per-contract only (§2, §11.6).')
output managedHsmDeployed bool = managedHsm.outputs.deployed

@description('How many §10.3 alerts are live. §10.3 has 18 rows; a smaller number is a gap the deployment states rather than hides.')
output alertCount int = monitoring.outputs.alertCount
