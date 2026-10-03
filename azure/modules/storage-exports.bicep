// storage-exports.bicep — the account that holds the scheduled columnar export to the customer's
// own storage (docs/05-platform-delivery.md §2, §12.3, C31/Q11).
//
// A separate account because its retention and access model is the customer's, not the product's:
// exports move to cool at 30 days and are deleted at 90 unless the customer has taken delivery,
// because delivery is the customer's copy (§12.3).

@description('Azure region for the account. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment.')
param baseName string

@description('Redundancy: ZRS in production (§2 inventory, §12.3). Lower environments use LRS to keep cost honest.')
@allowed(['LRS', 'ZRS', 'GRS'])
param redundancy string = 'ZRS'

@description('Container names. One per export format so a lifecycle rule can differ per format.')
param containers array = ['exports']

@description('Days after modification before an export moves to cool. §12.3: 30.')
param coolAfterDays int = 30

@description('Days after modification before an export is deleted. §12.3: 90 unless the customer has taken delivery — delivery is out of band and recorded, so the account stays bounded either way.')
param deleteAfterDays int = 90

@description('Resource id of the private-endpoint subnet, from network.bicep.')
param privateEndpointSubnetId string = ''

@description('Private DNS zone resource ids keyed by zone name, from network.bicep.')
param privateDnsZoneIds object = {}

@description('Tags applied to every resource.')
param tags object = {}

var accountName = toLower(take(replace('${baseName}exports', '-', ''), 24))

resource account 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: accountName
  location: location
  tags: tags
  kind: 'StorageV2'
  sku: {
    name: redundancy
  }
  properties: {
    accessTier: 'Cool'
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false
    publicNetworkAccess: 'Disabled'
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    encryption: {
      requireInfrastructureEncryption: true
      keySource: 'Microsoft.Storage'
      services: {
        blob: {
          enabled: true
        }
      }
    }
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  name: '${account.name}/default'
  properties: {
    deleteRetentionPolicy: {
      enabled: true
      days: 30
    }
    isVersioningEnabled: true
  }
}

resource exportContainers 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = [for container in containers: {
  name: '${account.name}/default/${container}'
  properties: {
    publicAccess: 'None'
  }
  dependsOn: [
    blobService
  ]
}]

resource lifecycle 'Microsoft.Storage/storageAccounts/managementPolicies@2023-05-01' = {
  name: '${account.name}/default'
  properties: {
    policy: {
      rules: [
        {
          enabled: true
          name: 'exports-expiry'
          type: 'Lifecycle'
          definition: {
            actions: {
              baseBlob: {
                tierToCool: {
                  daysAfterModificationGreaterThan: coolAfterDays
                }
                delete: {
                  daysAfterModificationGreaterThan: deleteAfterDays
                }
              }
            }
            filters: {
              blobTypes: [
                'blockBlob'
              ]
            }
          }
        }
      ]
    }
  }
}

module privateEndpoint 'private-endpoints.bicep' = if (privateEndpointSubnetId != '') {
  name: '${baseName}-exports-pe'
  params: {
    location: location
    baseName: '${baseName}-exports'
    subnetId: privateEndpointSubnetId
    privateDnsZoneIds: privateDnsZoneIds
    targets: [
      {
        name: 'exports'
        resourceId: account.id
        groupId: 'blob'
        dnsZoneName: 'privatelink.blob.core.windows.net'
      }
    ]
    tags: tags
  }
}

@description('Resource id of the exports account.')
output accountId string = account.id

@description('The blob endpoint for the export writer, reachable only inside the VNet.')
output blobEndpoint string = account.properties.primaryEndpoints.blob
