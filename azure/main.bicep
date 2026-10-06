// main.bicep — one environment of Shadow AI Capture.
//
// Devices reach Application Gateway (mutual TLS) → ingest-api and control-api. Browsers reach Front
// Door → the dashboard server, which calls control-api, query-api and content-vault inside the
// environment. Everything runs in an internal Container Apps environment on a private VNet with
// PostgreSQL (VNet-integrated, Entra-only) and Key Vault (private endpoint). Each workload runs as its
// own managed identity, which is also its PostgreSQL login.
//
// bootstrap = true deploys the platform (network, registry, Key Vault, PostgreSQL, the Container Apps
// environment, identities) without apps or edges: on an environment's first deployment, so the
// registry and Key Vault exist before images are pushed and secrets are created; and, with an image
// tag, to create the migrate job before the first apps start.

targetScope = 'resourceGroup'

@description('Azure region; also the residency region tenants in this environment are pinned to.')
param location string

@allowed(['preprod', 'prod'])
param environment string

@description('Prefix for every resource name, e.g. sac-prod-eastus.')
param baseName string

param tags object = {}

@description('First deployment only: create the platform without workloads and edges.')
param bootstrap bool = false

@description('Image tag every app and job runs; the pipeline passes the commit it built.')
param imageTag string = ''

@description('Hostname devices connect to (Application Gateway), e.g. device.sac.example.com.')
param deviceFqdn string

@description('Hostname browsers use (Front Door custom domain), e.g. app.sac.example.com.')
param analystFqdn string

@description('Client id of the vendor multi-tenant Entra application (sign-in and the Intune check).')
param entraAppClientId string

@description('Key id of the policy signing key, pinned by the agent MSI.')
param policySigningKeyId string

param postgresSkuName string

@allowed(['Burstable', 'GeneralPurpose', 'MemoryOptimized'])
param postgresSkuTier string

param postgresStorageGb int

@description('Optional Entra group with break-glass PostgreSQL admin rights: { objectId, name }.')
param postgresOperatorGroup object = {}

@description('Operator IPv4 addresses allowed to reach Key Vault\'s public endpoint to manage secrets. Empty closes it.')
param keyVaultAdminIpRules array = []

@description('Entra users or groups that manage Key Vault secrets and certificates.')
param keyVaultAdminPrincipalIds array = []

@minValue(1)
param apiMinReplicas int

param apiMaxReplicas int

@description('Registry geo-replica region (production), or empty.')
param registryGeoReplicaLocation string = ''

param alertEmails array

param monthlyBudgetAmount int

var prod = environment == 'prod'
var deploy = !bootstrap

// ---------------------------------------------------------------------------------------------
// Identities

var identityNames = ['ingest-api', 'control-api', 'content-vault', 'query-api', 'dashboard', 'jobs', 'migrate', 'gateway']

resource identities 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = [for name in identityNames: {
  name: '${baseName}-id-${name}'
  location: location
  tags: tags
}]

var id = {
  ingest: { id: identities[0].id, clientId: identities[0].properties.clientId, principalId: identities[0].properties.principalId }
  control: { id: identities[1].id, clientId: identities[1].properties.clientId, principalId: identities[1].properties.principalId }
  vault: { id: identities[2].id, clientId: identities[2].properties.clientId, principalId: identities[2].properties.principalId }
  query: { id: identities[3].id, clientId: identities[3].properties.clientId, principalId: identities[3].properties.principalId }
  dashboard: { id: identities[4].id, clientId: identities[4].properties.clientId, principalId: identities[4].properties.principalId }
  jobs: { id: identities[5].id, clientId: identities[5].properties.clientId, principalId: identities[5].properties.principalId }
  migrate: { id: identities[6].id, clientId: identities[6].properties.clientId, principalId: identities[6].properties.principalId, name: identities[6].name }
  gateway: { id: identities[7].id, clientId: identities[7].properties.clientId, principalId: identities[7].properties.principalId }
}

// ---------------------------------------------------------------------------------------------
// Platform

module network 'modules/network.bicep' = {
  name: 'network'
  params: {
    location: location
    baseName: baseName
    tags: tags
  }
}

module logAnalytics 'modules/log-analytics.bicep' = {
  name: 'log-analytics'
  params: {
    location: location
    baseName: baseName
    retentionInDays: prod ? 90 : 30
    tags: tags
  }
}

module registry 'modules/registry.bicep' = {
  name: 'registry'
  params: {
    location: location
    baseName: baseName
    skuName: prod ? 'Premium' : 'Standard'
    geoReplicaLocation: registryGeoReplicaLocation
    pullPrincipalIds: [id.ingest.principalId, id.control.principalId, id.vault.principalId, id.query.principalId, id.dashboard.principalId, id.jobs.principalId, id.migrate.principalId]
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    tags: tags
  }
}

module keyVault 'modules/keyvault.bicep' = {
  name: 'keyvault'
  params: {
    location: location
    baseName: baseName
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    privateDnsZoneId: network.outputs.keyVaultDnsZoneId
    adminIpRules: keyVaultAdminIpRules
    adminPrincipalIds: keyVaultAdminPrincipalIds
    assignSecretReaders: deploy
    secretReaders: [
      { secret: 'sac-device-ca-cert', principalIds: [id.ingest.principalId, id.control.principalId] }
      { secret: 'sac-device-ca-key', principalIds: [id.control.principalId] }
      { secret: 'sac-session-signing-key', principalIds: [id.control.principalId] }
      { secret: 'sac-policy-signing-key', principalIds: [id.control.principalId] }
      { secret: 'sac-directory-key', principalIds: [id.control.principalId] }
      { secret: 'sac-internal-token', principalIds: [id.control.principalId, id.dashboard.principalId] }
      { secret: 'sac-content-keys', principalIds: [id.vault.principalId] }
      { secret: 'sac-cursor-key', principalIds: [id.query.principalId] }
      { secret: 'sac-device-tls', principalIds: [id.gateway.principalId] }
    ]
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
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
    highAvailabilityMode: prod ? 'ZoneRedundant' : 'Disabled'
    backupRetentionDays: prod ? 35 : 7
    geoRedundantBackup: prod
    delegatedSubnetId: network.outputs.postgresSubnetId
    privateDnsZoneId: network.outputs.postgresDnsZoneId
    migrationAdmin: { principalId: id.migrate.principalId, name: id.migrate.name }
    operatorAdminGroup: postgresOperatorGroup
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    tags: tags
  }
}

module environmentModule 'modules/container-apps-env.bicep' = {
  name: 'container-apps-env'
  params: {
    location: location
    baseName: baseName
    infrastructureSubnetId: network.outputs.containerAppsSubnetId
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    zoneRedundant: prod
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// Workloads

var domain = environmentModule.outputs.defaultDomain
var vaultUri = keyVault.outputs.vaultUri
// An app with external ingress answers at <app>.<domain>, an internal one at <app>.internal.<domain>.
var url = {
  control: 'https://control-api.${domain}'
  vault: 'https://content-vault.internal.${domain}'
  query: 'https://query-api.internal.${domain}'
}
var publicUrl = 'https://${analystFqdn}'
var pg = [
  { name: 'SAC_PG_HOST', value: postgres.outputs.fqdn }
  { name: 'SAC_PG_DATABASE', value: postgres.outputs.databaseName }
]

module ingestApp 'modules/container-app.bicep' = if (deploy) {
  name: 'app-ingest-api'
  params: {
    location: location
    appName: 'ingest-api'
    environmentId: environmentModule.outputs.environmentId
    identity: id.ingest
    ingress: 'external'
    image: '${registry.outputs.loginServer}/ingest-api:${imageTag}'
    registryLoginServer: registry.outputs.loginServer
    minReplicas: apiMinReplicas
    maxReplicas: apiMaxReplicas
    vaultUri: vaultUri
    env: concat(pg, [
      { name: 'SAC_PG_USER', value: 'ingest-api' }
      { name: 'SAC_REGION', value: location }
    ])
    secretEnv: [
      { name: 'SAC_CA_CERT_PEM', secret: 'sac-device-ca-cert' }
    ]
    tags: tags
  }
}

module controlApp 'modules/container-app.bicep' = if (deploy) {
  name: 'app-control-api'
  params: {
    location: location
    appName: 'control-api'
    environmentId: environmentModule.outputs.environmentId
    identity: id.control
    ingress: 'external'
    image: '${registry.outputs.loginServer}/control-api:${imageTag}'
    registryLoginServer: registry.outputs.loginServer
    minReplicas: apiMinReplicas
    maxReplicas: apiMaxReplicas
    vaultUri: vaultUri
    env: concat(pg, [
      { name: 'SAC_PG_USER', value: 'control-api' }
      { name: 'SAC_REGION', value: location }
      { name: 'SAC_AUTH_ISSUER', value: url.control }
      { name: 'SAC_PUBLIC_URL', value: publicUrl }
      { name: 'SAC_PUBLIC_DEVICE_ENDPOINT', value: 'https://${deviceFqdn}' }
      { name: 'SAC_CONTENT_VAULT_URL', value: url.vault }
      { name: 'SAC_SESSION_SIGNING_KEY_FILE', value: '/mnt/secrets/sac-session-signing-key' }
      { name: 'SAC_POLICY_SIGNING_KEY_FILE', value: '/mnt/secrets/sac-policy-signing-key' }
      { name: 'SAC_POLICY_SIGNING_KEY_ID', value: policySigningKeyId }
      { name: 'SAC_ENTRA_CLIENT_ID', value: entraAppClientId }
      { name: 'SAC_ENTRA_FIC', value: 'managed' }
    ])
    secretEnv: [
      { name: 'SAC_CA_CERT_PEM', secret: 'sac-device-ca-cert' }
      { name: 'SAC_CA_KEY_PEM', secret: 'sac-device-ca-key' }
      { name: 'SAC_DIRECTORY_KEY', secret: 'sac-directory-key' }
      { name: 'SAC_INTERNAL_TOKEN', secret: 'sac-internal-token' }
    ]
    secretFiles: ['sac-session-signing-key', 'sac-policy-signing-key']
    tags: tags
  }
}

// content-vault is the only component that can decrypt prompt content. Internal ingress: nothing
// outside the environment can reach it, and neither edge routes to it.
module vaultApp 'modules/container-app.bicep' = if (deploy) {
  name: 'app-content-vault'
  params: {
    location: location
    appName: 'content-vault'
    environmentId: environmentModule.outputs.environmentId
    identity: id.vault
    ingress: 'internal'
    image: '${registry.outputs.loginServer}/content-vault:${imageTag}'
    registryLoginServer: registry.outputs.loginServer
    cpu: '1.0'
    memory: '2Gi'
    minReplicas: 1
    maxReplicas: prod ? 4 : 2
    vaultUri: vaultUri
    env: concat(pg, [
      { name: 'SAC_PG_USER', value: 'content-vault' }
      { name: 'SAC_AUTH_ISSUER', value: url.control }
      { name: 'SAC_AUTH_AUDIENCE', value: 'sac-vault' }
      { name: 'SAC_RETRIEVAL_URL_BASE', value: publicUrl }
    ])
    secretEnv: [
      { name: 'SAC_CONTENT_KEYS', secret: 'sac-content-keys' }
    ]
    tags: tags
  }
}

module queryApp 'modules/container-app.bicep' = if (deploy) {
  name: 'app-query-api'
  params: {
    location: location
    appName: 'query-api'
    environmentId: environmentModule.outputs.environmentId
    identity: id.query
    ingress: 'internal'
    image: '${registry.outputs.loginServer}/query-api:${imageTag}'
    registryLoginServer: registry.outputs.loginServer
    minReplicas: apiMinReplicas
    maxReplicas: apiMaxReplicas
    vaultUri: vaultUri
    env: concat(pg, [
      { name: 'SAC_PG_USER', value: 'query-api' }
      { name: 'SAC_AUTH_ISSUER', value: url.control }
      { name: 'SAC_AUTH_AUDIENCE', value: 'sac-query' }
      { name: 'SAC_CONTENT_VAULT_URL', value: url.vault }
    ])
    secretEnv: [
      { name: 'SAC_CURSOR_KEY', secret: 'sac-cursor-key' }
    ]
    tags: tags
  }
}

module dashboardApp 'modules/container-app.bicep' = if (deploy) {
  name: 'app-dashboard'
  params: {
    location: location
    appName: 'dashboard'
    environmentId: environmentModule.outputs.environmentId
    identity: id.dashboard
    ingress: 'external'
    image: '${registry.outputs.loginServer}/dashboard:${imageTag}'
    registryLoginServer: registry.outputs.loginServer
    minReplicas: apiMinReplicas
    maxReplicas: apiMaxReplicas
    vaultUri: vaultUri
    env: [
      { name: 'SAC_PUBLIC_URL', value: publicUrl }
      { name: 'SAC_CONTROL_URL', value: url.control }
      { name: 'SAC_QUERY_API_URL', value: url.query }
      { name: 'SAC_CONTENT_VAULT_URL', value: url.vault }
    ]
    secretEnv: [
      { name: 'SAC_INTERNAL_TOKEN', secret: 'sac-internal-token' }
    ]
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// Jobs

var jobs = {
  aggregate: {
    image: 'jobs'
    identity: id.jobs
    args: ['aggregate']
    cron: '*/5 * * * *'
    env: concat(pg, [{ name: 'SAC_PG_USER', value: 'jobs' }])
    secretEnv: []
  }
  expire: {
    image: 'jobs'
    identity: id.jobs
    args: ['expire']
    cron: '30 2 * * *'
    env: concat(pg, [{ name: 'SAC_PG_USER', value: 'jobs' }])
    secretEnv: []
  }
  migrate: {
    // Applies the schema and maps each workload identity to its database role. Started by the
    // pipeline on every deployment; a run with nothing to apply is a no-op.
    image: 'migrate'
    identity: id.migrate
    args: []
    cron: ''
    env: concat(pg, [
      { name: 'SAC_PG_USER', value: id.migrate.name }
      {
        name: 'SAC_DB_LOGINS'
        value: string([
          { login: 'ingest-api', role: 'sac_ingest', objectId: id.ingest.principalId }
          { login: 'control-api', role: 'sac_control', objectId: id.control.principalId }
          { login: 'content-vault', role: 'sac_vault', objectId: id.vault.principalId }
          { login: 'query-api', role: 'sac_query', objectId: id.query.principalId }
          { login: 'jobs', role: 'sac_ops', objectId: id.jobs.principalId }
        ])
      }
    ])
    secretEnv: []
  }
  'tenant-admin': {
    // Operator commands (tenant create, tenant invite), started by hand with arguments.
    image: 'control-api'
    identity: id.control
    args: ['tenant', '--help']
    cron: ''
    env: concat(pg, [
      { name: 'SAC_PG_USER', value: 'control-api' }
      { name: 'SAC_REGION', value: location }
      { name: 'SAC_PUBLIC_URL', value: publicUrl }
    ])
    secretEnv: []
  }
}

// The migrate job exists whenever there is an image to run, including a bootstrap deployment, so the
// pipeline can apply the schema and create the workload logins before any app starts.
module jobModules 'modules/container-app-job.bicep' = [for name in ['aggregate', 'expire', 'migrate', 'tenant-admin']: if (deploy || (name == 'migrate' && !empty(imageTag))) {
  name: 'job-${name}'
  params: {
    location: location
    jobName: name
    environmentId: environmentModule.outputs.environmentId
    identity: jobs[name].identity
    image: '${registry.outputs.loginServer}/${jobs[name].image}:${imageTag}'
    registryLoginServer: registry.outputs.loginServer
    args: jobs[name].args
    cronExpression: jobs[name].cron
    env: jobs[name].env
    secretEnv: jobs[name].secretEnv
    vaultUri: vaultUri
    tags: tags
  }
}]

// ---------------------------------------------------------------------------------------------
// Edges

module gateway 'modules/application-gateway.bicep' = if (deploy) {
  name: 'application-gateway'
  params: {
    location: location
    baseName: baseName
    gatewaySubnetId: network.outputs.gatewaySubnetId
    deviceFqdn: deviceFqdn
    certificateKeyVaultUri: '${vaultUri}secrets/sac-device-tls'
    identity: id.gateway
    backendIp: environmentModule.outputs.staticIp
    backendHosts: {
      ingest: ingestApp.?outputs.fqdn ?? ''
      control: controlApp.?outputs.fqdn ?? ''
    }
    zoneRedundant: prod
    minCapacity: prod ? 2 : 1
    tags: tags
  }
}

module frontDoor 'modules/frontdoor.bicep' = if (deploy) {
  name: 'frontdoor'
  params: {
    baseName: baseName
    analystFqdn: analystFqdn
    environmentId: environmentModule.outputs.environmentId
    environmentLocation: location
    originHosts: {
      dashboard: dashboardApp.?outputs.fqdn ?? ''
      control: controlApp.?outputs.fqdn ?? ''
    }
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// Operations

module monitoring 'modules/monitoring.bicep' = {
  name: 'monitoring'
  params: {
    location: location
    baseName: baseName
    logAnalyticsWorkspaceId: logAnalytics.outputs.workspaceId
    postgresServerId: postgres.outputs.serverId
    containerAppIds: deploy ? [
      ingestApp.?outputs.id ?? ''
      controlApp.?outputs.id ?? ''
      vaultApp.?outputs.id ?? ''
      queryApp.?outputs.id ?? ''
      dashboardApp.?outputs.id ?? ''
    ] : []
    applicationGatewayId: deploy ? resourceId('Microsoft.Network/applicationGateways', '${baseName}-agw') : ''
    frontDoorProfileId: deploy ? (frontDoor.?outputs.profileId ?? '') : ''
    alertEmails: alertEmails
    tags: tags
  }
  dependsOn: [gateway]
}

module budget 'modules/budget.bicep' = {
  name: 'budget'
  params: {
    baseName: baseName
    amount: monthlyBudgetAmount
    contactEmails: alertEmails
  }
}

// ---------------------------------------------------------------------------------------------
// Outputs the operator and the pipeline need

output registryLoginServer string = registry.outputs.loginServer
output keyVaultName string = keyVault.outputs.vaultName
output postgresFqdn string = postgres.outputs.fqdn
output deviceEdgeIp string = deploy ? (gateway.?outputs.publicIp ?? '') : ''
output frontDoorEndpoint string = deploy ? (frontDoor.?outputs.endpointHostName ?? '') : ''
output analystDomainValidationToken string = deploy ? (frontDoor.?outputs.domainValidationToken ?? '') : ''
output containerAppsEnvironmentId string = environmentModule.outputs.environmentId

@description('The federated credential to add to the vendor Entra app so control-api can act as it without a secret.')
output entraFederatedCredential object = {
  issuer: '${az.environment().authentication.loginEndpoint}${tenant().tenantId}/v2.0'
  subject: id.control.principalId
  audiences: ['api://AzureADTokenExchange']
}
