// registry.bicep — Container Registry Premium with a private endpoint and geo-replication to the
// paired region (docs/05-platform-delivery.md §2, §12.1).
//
// Images are pulled over Private Link, so the registry is not publicly reachable. Geo-replication is
// what makes regional failover a redeploy rather than a rebuild (§12.1), which is why the replica
// region is a parameter rather than a constant.

@description('Azure region for the registry. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment.')
param baseName string

@description('Registry SKU. Premium is required for private endpoints and geo-replication, which §2 requires; the value is still a parameter so cost can be modelled per environment.')
@allowed(['Basic', 'Standard', 'Premium'])
param skuName string = 'Premium'

@description('Paired region for geo-replication, e.g. centralus for eastus. Empty disables replication, which is the dev setting.')
param geoReplicaLocation string = ''

@description('Whether the registry is zone-redundant. Zone redundancy requires the Premium tier, so the composition states both rather than deriving one from the SKU inside the module.')
param zoneRedundant bool = true

@description('Resource id of the private-endpoint subnet from network.bicep.')
param privateEndpointSubnetId string = ''

@description('Private DNS zone resource ids keyed by zone name, from network.bicep.')
param privateDnsZoneIds object = {}

@description('Resource id of the Log Analytics workspace for diagnostic settings. Empty skips diagnostics (dev).')
param logAnalyticsWorkspaceId string = ''

@description('Tags applied to every resource.')
param tags object = {}

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' = {
  name: replace('${baseName}acr', '-', '')
  location: location
  tags: tags
  sku: {
    name: skuName
  }
  properties: {
    adminUserEnabled: false // no registry password exists: pushes use the federated CI identity (§5.2)
    publicNetworkAccess: 'Disabled'
    networkRuleBypassOptions: 'AzureServices'
    policies: {
      quarantinePolicy: {
        status: 'disabled'
      }
      trustPolicy: {
        status: 'enabled' // signed images only: the deployment identity and the image content are separate trust decisions
        type: 'Notary'
      }
    }
    zoneRedundancy: zoneRedundant ? 'Enabled' : 'Disabled'
  }
}

resource geoReplica 'Microsoft.ContainerRegistry/registries/replications@2023-07-01' = if (geoReplicaLocation != '') {
  name: '${replace('${baseName}acr', '-', '')}/${geoReplicaLocation}'
  location: geoReplicaLocation
  tags: tags
  properties: {
    regionEndpointEnabled: true
    zoneRedundancy: 'Enabled'
  }
}

module privateEndpoint 'private-endpoints.bicep' = if (privateEndpointSubnetId != '') {
  name: '${baseName}-acr-pe'
  params: {
    location: location
    baseName: '${baseName}-acr'
    subnetId: privateEndpointSubnetId
    privateDnsZoneIds: privateDnsZoneIds
    targets: [
      {
        name: 'acr'
        resourceId: registry.id
        groupId: 'registry'
        dnsZoneName: 'privatelink.azurecr.io'
      }
    ]
    tags: tags
  }
}

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = if (logAnalyticsWorkspaceId != '') {
  name: '${baseName}-acr-diag'
  scope: registry
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logs: [
      {
        category: 'ContainerRegistryRepositoryEvents'
        enabled: true
      }
    ]
    metrics: [
      {
        category: 'AllMetrics'
        enabled: true
      }
    ]
  }
}

@description('Resource id of the registry, for the Container Apps environment’s image pulls.')
output registryId string = registry.id

@description('The registry login server, e.g. sacprodacr.azurecr.io. Services authenticate by managed identity, not by a registry password.')
output loginServer string = registry.properties.loginServer
