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
// Deployment scope: what this composition is allowed to deploy.
//
// These exist so a lab is a PARAMETER FILE rather than a second composition. The lab in
// docs/lab/LAB-COST.md drops three resource families — the edge, the dashboard and the export
// storage — because each is either unwanted in a lab or the single largest line in the bill
// (Front Door's base fee is ~$330/month, more than ten times the whole cheap lab). Forking
// main.bicep for that would have produced a parallel tree that drifts from production within a
// month; three switches keep one composition and make the lab's differences explicit and reviewable.
//
// Every default is true, so an existing parameter file deploys exactly what it deployed before.
// A lab sets them false. Nothing else about the composition changes.
// ---------------------------------------------------------------------------------------------

@description('Deploy the edge: Front Door Premium + WAF (analyst) and Application Gateway WAF_v2 + WAF (device). False in the lab: the two edges are the largest fixed lines in the bill (Front Door ~$330/month, Application Gateway ~$321/month), and a lab is reached over the VNet instead.')
param deployEdge bool = true

@description('Deploy the dashboard server: the pages, the sign-in and session (a thin client of control-api), and the forwarding of reads, admin calls and minted retrieval URLs to the apps behind it. False in the lab, which is reached over the VNet without a browser front end.')
param deployDashboard bool = true

@description('Deploy the second storage account used for customer-facing columnar exports. False in the lab: exports are a delivery feature, not something a lab exercises.')
param deployExports bool = true

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

@description('The public device hostname that resolves to Application Gateway (ADR 0020). Empty when the edge is not deployed (lab).')
param deviceFqdn string = ''

@description('Key Vault secret id of the device FQDN TLS certificate. Supplied at deploy time from Key Vault, never in a parameter file (§5.1); empty when the edge is not deployed (lab).')
param deviceTlsCertKeyVaultSecretId string = ''

@description('The public HTTPS origin of the Front Door edge, e.g. https://app.sac.example.com: where browsers load the dashboard, where a sign-in returns (<origin>/callback), the onboarding links control-api prints (<origin>/onboard/...) and the SCIM base URL a customer gives its identity provider (<origin>/scim/v2). A custom domain on Front Door; empty in the lab, which has no public edge.')
param publicUrl string = ''

@description('Client id of the vendor multi-tenant Entra application: sign-in for every Entra customer and the app-only Graph call that checks a device is Intune-managed (contract §3, §5). A public identifier, not a credential; empty until the application is registered.')
param entraAppClientId string = ''

@description('Where control-api finds the generic agent release a tenant package wraps (ShadowAICapture.msi and release.json). A path in the control-api image, put there by the release pipeline, so the release a deployment serves is the one its image digest names rather than whatever a share holds that day.')
param agentReleaseDir string = '/opt/sac/agent-release'

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

@description('The dashboard server runs as this identity. Its one grant is reading the internal token it presents to control-api from Key Vault; it holds no database role and no content key.')
resource identityDashboard 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-dashboard'
  location: location
  tags: tags
}

@description('The Application Gateway runs as this identity to read its TLS certificate from Key Vault. §5.1: every component runs as a user-assigned identity and the grant is the security boundary.')
resource identityGateway 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${baseName}-id-gateway'
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
      {
        name: 'application-gateway'
        principalId: identityGateway.properties.principalId
        roleDefinitionId: 'secretsUser'
      }
      // The internal token it presents on /internal/v1/auth/* (see controlApp).
      {
        name: 'dashboard'
        principalId: identityDashboard.properties.principalId
        roleDefinitionId: 'secretsUser'
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
    // The vault reads ciphertext back for a retrieval as itself (SAC_BLOB_IDENTITY=managed); this is
    // that read, and the only role on the account. Uploads arrive under a per-object grant instead.
    blobReaderPrincipalIds: [
      identityVault.properties.principalId
    ]
    tags: tags
  }
}

// Conditional on deployExports: a lab does not exercise the customer-facing export path, and the
// second storage account plus its lifecycle policy is a resource with no reader in a lab.
module exports 'modules/storage-exports.bicep' = if (deployExports) {
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

// Conditional on the switch AND the quorum: the module already refuses to create a pool without
// three administrators, so the condition here only avoids instantiating a module that would deploy
// nothing. A lab leaves deployManagedHsm false — a pool is ~$3,360/month.
module managedHsm 'modules/managed-hsm.bicep' = if (deployManagedHsm && length(hsmAdministratorObjectIds) == 3) {
  name: 'managed-hsm'
  params: {
    location: location
    baseName: baseName
    hsmSku: 'Custom_B32'
    administratorObjectIds: hsmAdministratorObjectIds
    deployManagedHsm: true
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
    ], deployManagedHsm && length(hsmAdministratorObjectIds) == 3 ? [
      {
        name: 'managed-hsm'
        // Safe access: the managedHsm module is conditional on the same two conditions, so this
        // branch is only reached when its outputs exist.
        resourceId: managedHsm.outputs.?hsmId ?? ''
        groupId: 'managedhsm'
        dnsZoneName: 'privatelink.managedhsm.azure.net'
      }
    ] : [])
    tags: tags
  }
}

// ---------------------------------------------------------------------------------------------
// The services. They differ in identity, scale and ingress, and in nothing else (§3.3).

// The product access-token issuer (contract §2): control-api, at the address the apps inside the
// environment reach it. query-api, the vault and the dashboard compare `iss` to this exactly and fetch
// the JWKS from it. Derived from the environment's domain rather than read from controlApp's output,
// because control-api is told its own issuer and a module cannot take its own output.
var productTokenIssuer = 'https://control-api.${containerAppsEnv.outputs.defaultDomain}'

// Key Vault secrets the services read by reference. The owner creates them before the first deploy
// (azure/README.md); nothing here holds a value.
var keyVaultSecretsUri = '${keyVault.outputs.vaultUri}secrets'

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
      // The identity service (contract §2, §3): the issuer it signs product tokens as, and the public
      // origin its onboarding pages, sign-in callbacks and SCIM base URL are built on.
      { name: 'SAC_AUTH_ISSUER', value: productTokenIssuer }
      { name: 'SAC_PUBLIC_URL', value: publicUrl }
      // The session-signing key (P-256, never the device token key) and the vendor policy key, as
      // files from Key Vault; see keyVaultFiles below.
      { name: 'SAC_SESSION_SIGNING_KEY_FILE', value: '/mnt/secrets/sac-session-signing-key' }
      { name: 'SAC_POLICY_SIGNING_KEY_FILE', value: '/mnt/secrets/sac-policy-signing-key' }
      // Tenant packages (contract §5): the generic release they wrap, and the device endpoint they
      // tell a device to enrol against -- the Application Gateway's hostname.
      { name: 'SAC_AGENT_RELEASE_DIR', value: agentReleaseDir }
      { name: 'SAC_PUBLIC_DEVICE_ENDPOINT', value: deviceFqdn == '' ? '' : 'https://${deviceFqdn}' }
      // The vendor multi-tenant Entra app, for sign-in and for the app-only Graph call of the Intune
      // check. No client secret: control-api presents this identity's own token as the client
      // assertion (a federated credential on the app; see the entraFederatedCredential output).
      { name: 'SAC_ENTRA_CLIENT_ID', value: entraAppClientId }
      { name: 'SAC_ENTRA_FIC', value: 'managed' }
      // Which user-assigned identity to ask the Container Apps identity endpoint for: the app could
      // hold more than one, and the federated credential trusts exactly this one.
      { name: 'AZURE_CLIENT_ID', value: identityControl.properties.clientId }
    ]
    keyVaultEnv: [
      // The shared secret the dashboard server presents on /internal/v1/auth/*. A stand-in for
      // service identity: production should replace it with the dashboard's managed identity token
      // verified by control-api, so there is no shared secret to rotate or leak.
      { name: 'SAC_INTERNAL_TOKEN', keyVaultUrl: '${keyVaultSecretsUri}/sac-internal-token', identity: identityControl.id }
      // Seals every *_enc column (OIDC client secrets, SCIM resources, tenants' user-reference keys).
      // Losing it makes those unreadable, and the user-reference keys cannot be re-provisioned.
      { name: 'SAC_DIRECTORY_KEY', keyVaultUrl: '${keyVaultSecretsUri}/sac-directory-key', identity: identityControl.id }
    ]
    keyVaultFiles: [
      { secretName: 'sac-session-signing-key', keyVaultUrl: '${keyVaultSecretsUri}/sac-session-signing-key', identity: identityControl.id }
      { secretName: 'sac-policy-signing-key', keyVaultUrl: '${keyVaultSecretsUri}/sac-policy-signing-key', identity: identityControl.id }
    ]
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
      // The vault reads ciphertext as itself: an AAD access token from the instance metadata service
      // for its user-assigned identity, never a storage key (docs/06 §5.4). The ciphertext module
      // grants that identity Storage Blob Data Reader on the account (blobReaderPrincipalIds above).
      { name: 'SAC_BLOB_IDENTITY', value: 'managed' }
      { name: 'SAC_KEYVAULT_URI', value: keyVault.outputs.vaultUri }
      { name: 'SAC_INTERNAL_ONLY', value: 'true' }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
      // The vault verifies the product token query-api forwards on a content request itself
      // (contract §2), so a forged tenant or role header is refused here too.
      { name: 'SAC_AUTH_ISSUER', value: productTokenIssuer }
      { name: 'SAC_AUTH_AUDIENCE', value: 'sac-vault' }
      // A minted retrieval URL is built on the public origin: the browser fetches it from the dashboard
      // server, which forwards that one path to the vault inside the environment. The vault keeps
      // internal ingress; no edge has a route to it.
      { name: 'SAC_RETRIEVAL_URL_BASE', value: publicUrl }
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
      // https: the app's ingress refuses plain HTTP (allowInsecure: false) with a redirect, which a
      // forwarded POST does not survive.
      { name: 'SAC_CONTENT_VAULT_URL', value: 'https://${contentVaultApp.outputs.fqdn}' }
      { name: 'SAC_APPINSIGHTS', value: logAnalytics.outputs.appInsightsConnectionString }
      // Only product access tokens are accepted: one issuer, one JWKS, whatever provider signed the
      // person in (contract §2).
      { name: 'SAC_AUTH_ISSUER', value: productTokenIssuer }
      { name: 'SAC_AUTH_AUDIENCE', value: 'sac-query' }
    ]
    keyVaultEnv: []
    tags: tags
  }
}

// The dashboard server (contract §6): the pages, the sign-in and the server-side session as a thin
// client of control-api, and the one place a browser's requests are forwarded from -- /v1/* to
// query-api and /admin/v1/* to control-api with the product token, and a minted retrieval URL to the
// vault inside the environment. A static host could do none of this: it holds no session and cannot
// reach an internal app, which is why it replaces the Static Web App the analyst page used to be.
module dashboardApp 'modules/container-app.bicep' = if (deployDashboard) {
  name: 'dashboard'
  params: {
    location: location
    appName: 'dashboard'
    environmentId: containerAppsEnv.outputs.environmentId
    userAssignedIdentityId: identityDashboard.id
    ingress: 'external'
    targetPort: 8787
    cpu: '0.5'
    memory: '1Gi'
    minReplicas: apiMinReplicas
    maxReplicas: environment == 'dev' ? 2 : 10
    image: '${registryLoginServer}/dashboard:${imageTag}'
    registryLoginServer: registryLoginServer
    // The server answers /healthz only; it has no dependency of its own to be not-ready on.
    readinessPath: '/healthz'
    env: [
      { name: 'SAC_QUERY_API_URL', value: 'https://${queryApp.outputs.fqdn}' }
      { name: 'SAC_CONTROL_URL', value: 'https://${controlApp.outputs.fqdn}' }
      { name: 'SAC_CONTENT_VAULT_URL', value: 'https://${contentVaultApp.outputs.fqdn}' }
      { name: 'SAC_PUBLIC_URL', value: publicUrl }
    ]
    keyVaultEnv: [
      // See controlApp: the same secret, until service identity replaces it.
      { name: 'SAC_INTERNAL_TOKEN', keyVaultUrl: '${keyVaultSecretsUri}/sac-internal-token', identity: identityDashboard.id }
    ]
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

// The edge pair is conditional on deployEdge. Front Door (analyst) and its WAF are one decision, and
// Application Gateway (device) and its WAF are the other. False in the lab, where the two base fees
// alone exceed the whole lab.
module waf 'modules/waf.bicep' = if (deployEdge) {
  name: 'waf'
  params: {
    wafMode: wafMode
    tags: tags
  }
}

// Browser and identity-provider edge: Front Door + Private Link. /analyst/* -> query-api;
// /scim/v2/*, /onboard/*, /.well-known/* -> control-api; everything else -> the dashboard server when
// it is deployed. No route reaches content-vault: a minted retrieval URL is answered by the dashboard
// server inside the environment. The device /v1/* routes are on Application Gateway below (ADR 0020
// decision 1).
module frontDoor 'modules/frontdoor.bicep' = if (deployEdge) {
  name: 'frontdoor'
  params: {
    wafPolicyId: waf.outputs.wafPolicyId
    environmentId: containerAppsEnv.outputs.environmentId
    privateLinkLocation: location
    originHostHeaders: union({
      'query-api': queryApp.outputs.fqdn
      'control-api': controlApp.outputs.fqdn
    }, deployDashboard ? {
      dashboard: dashboardApp.?outputs.fqdn ?? ''
    } : {})
    tags: tags
  }
}

// Device edge: Application Gateway WAF_v2, passthrough client auth, /v1/* -> ingest-api/control-api.
// The backend pool is the internal Container Apps static IP, with per-app Host headers, and the
// origin lock NSG (network.bicep) admits only this subnet to the environment on the device path.
module applicationGateway 'modules/application-gateway.bicep' = if (deployEdge) {
  name: 'application-gateway'
  params: {
    location: location
    baseName: baseName
    gatewaySubnetId: network.outputs.gatewaySubnetId
    deviceFqdn: deviceFqdn
    sslCertificateKeyVaultSecretId: deviceTlsCertKeyVaultSecretId
    gatewayIdentityId: identityGateway.id
    backendStaticIp: containerAppsEnv.outputs.staticIp
    backendHostNames: {
      ingest: ingestApp.outputs.fqdn
      control: controlApp.outputs.fqdn
    }
    wafMode: wafMode
    tags: tags
  }
}

// modules/static-web-app.bicep is no longer instantiated. The dashboard is the dashboardApp server
// above (contract §6): sign-in is control-api's, as the relying party for every customer provider, and
// a static host with its own Entra sign-in would be a second, unconnected sign-in path.

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

@description('The public hostname analysts resolve (Front Door). The device FQDN resolves to Application Gateway instead (ADR 0020). Everything else is private or internal. Empty when deployEdge is false, which is what the lab does — a lab is reached over the VNet, not the internet.')
output frontDoorHostName string = deployEdge ? frontDoor.outputs.endpointHostName : ''

@description('The public IP address devices reach — the only public device endpoint (docs/05 §3.5). Empty when deployEdge is false.')
output applicationGatewayIp string = deployEdge ? applicationGateway.outputs.deviceEndpointIp : ''

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
  gateway: identityGateway.id
  dashboard: identityDashboard.id
}

@description('Whether the Managed HSM pool was deployed. Per-contract only (§2, §11.6).')
output managedHsmDeployed bool = deployManagedHsm && length(hsmAdministratorObjectIds) == 3 ? managedHsm.outputs.?deployed ?? false : false

@description('Whether the edge (Front Door + Application Gateway and their WAF policies) was deployed. False in the lab, where the base fees exceed the whole environment.')
output edgeDeployed bool = deployEdge

@description('Whether the dashboard server was deployed. False in the lab, which has no browser front end.')
output dashboardDeployed bool = deployDashboard

@description('Whether the export storage account was deployed.')
output exportsDeployed bool = deployExports

@description('How many §10.3 alerts are live. §10.3 has 18 rows; a smaller number is a gap the deployment states rather than hides.')
output alertCount int = monitoring.outputs.alertCount

// The vendor multi-tenant Entra app trusts control-api's managed identity as a federated credential,
// so the app-only Graph call of the Intune check (contract §3, §5; SAC_ENTRA_FIC=managed) needs no
// client secret anywhere. This file does not use the Microsoft Graph Bicep extension, so the
// credential is not deployed here: the owner adds it once per environment, on the app registration,
// from this output --
//
//   az ad app federated-credential create --id <entraAppClientId> --parameters '{
//     "name": "<baseName>-control-api",
//     "issuer": "<entraFederatedCredential.issuer>",
//     "subject": "<entraFederatedCredential.subject>",
//     "audiences": ["api://AzureADTokenExchange"],
//     "description": "control-api presents its managed identity token as the client assertion"
//   }'
//
// or, in the portal: App registrations > the app > Certificates & secrets > Federated credentials >
// Add credential > scenario "Managed identity" > the identity <baseName>-id-control. The app must be in
// this deployment's tenant (a managed identity can only be trusted by an app in its own tenant). The
// one Graph application permission, DeviceManagementManagedDevices.Read.All, is granted by each
// customer's admin consent at onboarding; the app asks for no other application permission.
@description('What the owner sets on the vendor multi-tenant Entra app registration: a federated credential with this issuer, subject (the control-api identity object id) and audience. See the comment above for the exact command.')
output entraFederatedCredential object = {
  issuer: '${az.environment().authentication.loginEndpoint}${tenant().tenantId}/v2.0'
  subject: identityControl.properties.principalId
  audiences: [
    'api://AzureADTokenExchange'
  ]
  managedIdentityClientId: identityControl.properties.clientId
  managedIdentityPrincipalId: identityControl.properties.principalId
  appClientId: entraAppClientId
}

@description('The product access-token issuer control-api signs as and query-api and the vault verify (contract §2). The dashboard server verifies none: it holds the session and forwards the token.')
output productTokenIssuer string = productTokenIssuer
