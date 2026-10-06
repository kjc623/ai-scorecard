// keyvault.bicep — the vault holding every secret the services read, and the device TLS certificate.
//
// Access is per secret: each workload identity can read exactly the secrets it is listed against,
// never the whole vault. Workloads reach the vault through its private endpoint. The public endpoint
// is closed unless adminIpRules names the operator addresses allowed to create and rotate secrets.

@description('Azure region.')
param location string

@description('Base name, e.g. sac-prod-eastus.')
param baseName string

@description('Private endpoint subnet.')
param privateEndpointSubnetId string

@description('privatelink.vaultcore.azure.net zone id.')
param privateDnsZoneId string

@description('Operator IPv4 addresses or CIDR ranges allowed through the public endpoint. Empty closes it.')
param adminIpRules array = []

@description('Entra users or groups that create and rotate secrets and certificates.')
param adminPrincipalIds array = []

@description('Per-secret read access: [{ secret: name, principalIds: [...] }]. Applied only once the secrets exist.')
param secretReaders array = []

@description('False on the bootstrap deployment, before the secrets have been created.')
param assignSecretReaders bool

param logAnalyticsWorkspaceId string

param tags object = {}

var secretsUser = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')
var secretsOfficer = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b86a8fe4-44ce-4948-aee5-eccb2c155cd7')
var certificatesOfficer = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'a4417e6f-fecd-4de8-b567-7b0420556985')

var readerAssignments = flatten(map(secretReaders, r => map(r.principalIds, p => { secret: r.secret, principalId: p })))

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: '${baseName}-kv'
  location: location
  tags: tags
  properties: {
    sku: { family: 'A', name: 'standard' }
    tenantId: subscription().tenantId
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 90
    enablePurgeProtection: true
    publicNetworkAccess: empty(adminIpRules) ? 'Disabled' : 'Enabled'
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'None'
      ipRules: [for ip in adminIpRules: { value: ip }]
    }
  }
}

resource endpoint 'Microsoft.Network/privateEndpoints@2024-05-01' = {
  name: '${baseName}-kv-pe'
  location: location
  tags: tags
  properties: {
    subnet: { id: privateEndpointSubnetId }
    privateLinkServiceConnections: [
      {
        name: 'vault'
        properties: {
          privateLinkServiceId: vault.id
          groupIds: ['vault']
        }
      }
    ]
  }
}

resource endpointDns 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2024-05-01' = {
  parent: endpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [
      {
        name: 'vault'
        properties: { privateDnsZoneId: privateDnsZoneId }
      }
    ]
  }
}

resource secrets 'Microsoft.KeyVault/vaults/secrets@2023-07-01' existing = [for a in readerAssignments: {
  parent: vault
  name: a.secret
}]

resource readers 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (a, i) in readerAssignments: if (assignSecretReaders) {
  name: guid(vault.id, a.secret, a.principalId)
  scope: secrets[i]
  properties: {
    roleDefinitionId: secretsUser
    principalId: a.principalId
    principalType: 'ServicePrincipal'
  }
}]

resource adminSecrets 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for p in adminPrincipalIds: {
  name: guid(vault.id, p, secretsOfficer)
  scope: vault
  properties: {
    roleDefinitionId: secretsOfficer
    principalId: p
  }
}]

resource adminCertificates 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for p in adminPrincipalIds: {
  name: guid(vault.id, p, certificatesOfficer)
  scope: vault
  properties: {
    roleDefinitionId: certificatesOfficer
    principalId: p
  }
}]

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: 'law'
  scope: vault
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logs: [
      { category: 'AuditEvent', enabled: true }
    ]
  }
}

output vaultUri string = vault.properties.vaultUri
output vaultName string = vault.name
