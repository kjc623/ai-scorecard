// registry.bicep — the container registry the pipeline pushes to and every app and job pulls from.
//
// The admin user is disabled: the pipeline pushes with its federated identity and the workloads pull
// with their managed identities, so no registry password exists. The registry keeps its public
// endpoint because hosted CI runners push to it; every request still needs an Entra identity with a
// role on the registry.

@description('Azure region.')
param location string

@description('Base name, e.g. sac-prod-eastus. The registry name is derived from it without hyphens.')
param baseName string

@description('Premium adds zone redundancy and geo-replication.')
@allowed(['Standard', 'Premium'])
param skuName string

@description('Region to geo-replicate to (Premium only), or empty for none.')
param geoReplicaLocation string = ''

@description('Principal ids that pull images (the workload identities).')
param pullPrincipalIds array

param logAnalyticsWorkspaceId string

param tags object = {}

var acrPull = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' = {
  name: replace('${baseName}acr', '-', '')
  location: location
  tags: tags
  sku: { name: skuName }
  properties: {
    adminUserEnabled: false
    zoneRedundancy: skuName == 'Premium' ? 'Enabled' : 'Disabled'
  }
}

resource replica 'Microsoft.ContainerRegistry/registries/replications@2023-07-01' = if (skuName == 'Premium' && geoReplicaLocation != '') {
  parent: registry
  name: empty(geoReplicaLocation) ? 'none' : geoReplicaLocation
  location: empty(geoReplicaLocation) ? location : geoReplicaLocation
  tags: tags
  properties: {
    regionEndpointEnabled: true
    zoneRedundancy: 'Enabled'
  }
}

resource pullAssignments 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for principalId in pullPrincipalIds: {
  name: guid(registry.id, principalId, acrPull)
  scope: registry
  properties: {
    roleDefinitionId: acrPull
    principalId: principalId
    principalType: 'ServicePrincipal'
  }
}]

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: 'law'
  scope: registry
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logs: [
      { category: 'ContainerRegistryRepositoryEvents', enabled: true }
      { category: 'ContainerRegistryLoginEvents', enabled: true }
    ]
  }
}

output loginServer string = registry.properties.loginServer
