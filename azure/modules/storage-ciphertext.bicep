// storage-ciphertext.bicep — the account that holds M3 attachment ciphertext (docs/05-platform-
// delivery.md §2, §12.3).
//
// The vendor stores ciphertext only (C15), which makes three settings architectural rather than
// cosmetic: no public blob access, no shared-key access (every access path is a managed identity),
// and no public network access (the private endpoint is the only way in). Versioning and soft delete
// are what make an overwrite recoverable while still letting an erasure complete by deleting
// versions and keys together (§12.3).

@description('Azure region for the account. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment. Storage account names must be globally unique, lower-case and 3–24 characters; the module derives one.')
param baseName string

@description('Redundancy: RA-GRS in production (the readable secondary is the point of RA over GRS), ZRS or LRS in lower environments (§3.1, §12.3).')
@allowed(['LRS', 'ZRS', 'GRS', 'RA-GRS', 'RA-GZRS'])
param redundancy string = 'RA-GRS'

@description('Access tier for new blobs: Hot in production, Cool in dev.')
@allowed(['Hot', 'Cool'])
param accessTier string = 'Hot'

@description('Soft-delete retention in days. 30 per §12.3: an overwrite is recoverable, an erasure is not, because erasure deletes versions and keys together.')
@minValue(1)
@maxValue(365)
param softDeleteRetentionDays int = 30

@description('Container names. One for attachment ciphertext; the local content store on a device is a separate concern (§12).')
param containers array = ['content-ciphertext']

@description('How long ciphertext stays hot before moving to cool, in days. §12.3: 30 days is the window in which an investigation almost always starts.')
param hotToCoolDays int = 30

@description('How long ciphertext stays cool before moving to archive, in days. §12.3: archive at 90 days.')
param coolToArchiveDays int = 90

@description('Whether to apply the §12.3 tiering rules. The parameter exists because tiering is per-tenant retention policy, and a customer contract may keep everything hot.')
param enableLifecycleTiering bool = true

@description('Resource id of the private-endpoint subnet, from network.bicep.')
param privateEndpointSubnetId string = ''

@description('Private DNS zone resource ids keyed by zone name, from network.bicep.')
param privateDnsZoneIds object = {}

@description('Resource id of the Log Analytics workspace for diagnostics. Empty skips diagnostics (dev).')
param logAnalyticsWorkspaceId string = ''

@description('Tags applied to every resource.')
param tags object = {}

var accountName = toLower(take(replace('${baseName}ciphertext', '-', ''), 24))

resource account 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: accountName
  location: location
  tags: tags
  kind: 'StorageV2'
  sku: {
    name: redundancy
  }
  properties: {
    accessTier: accessTier
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false
    allowCrossTenantReplication: false
    publicNetworkAccess: 'Disabled'
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    isHnsEnabled: true
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
      days: softDeleteRetentionDays
    }
    isVersioningEnabled: true
    changeFeed: {
      enabled: true
    }
    containerDeleteRetentionPolicy: {
      enabled: true
      days: softDeleteRetentionDays
    }
  }
}

resource blobContainers 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = [for container in containers: {
  name: '${account.name}/default/${container}'
  properties: {
    publicAccess: 'None'
  }
  dependsOn: [
    blobService
  ]
}]

resource lifecycle 'Microsoft.Storage/storageAccounts/managementPolicies@2023-05-01' = if (enableLifecycleTiering) {
  name: '${account.name}/default'
  properties: {
    policy: {
      rules: [
        {
          enabled: true
          name: 'ciphertext-tiering'
          type: 'Lifecycle'
          definition: {
            actions: {
              baseBlob: {
                tierToCool: {
                  daysAfterModificationGreaterThan: hotToCoolDays
                }
                tierToArchive: {
                  daysAfterModificationGreaterThan: coolToArchiveDays
                }
              }
              version: {
                tierToCool: {
                  daysAfterCreationGreaterThan: hotToCoolDays
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
  name: '${baseName}-ciphertext-pe'
  params: {
    location: location
    baseName: '${baseName}-ciphertext'
    subnetId: privateEndpointSubnetId
    privateDnsZoneIds: privateDnsZoneIds
    targets: [
      {
        name: 'ciphertext'
        resourceId: account.id
        groupId: 'blob'
        dnsZoneName: 'privatelink.blob.core.windows.net'
      }
    ]
    tags: tags
  }
}

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = if (logAnalyticsWorkspaceId != '') {
  name: '${baseName}-ciphertext-diag'
  scope: account
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logs: [
      {
        category: 'StorageRead'
        enabled: true
      }
      {
        category: 'StorageWrite'
        enabled: true
      }
      {
        category: 'StorageDelete'
        enabled: true
      }
    ]
    metrics: [
      {
        category: 'Transaction'
        enabled: true
      }
    ]
  }
}

@description('Resource id of the ciphertext account, for the content-vault identity’s role assignment.')
output accountId string = account.id

@description('The blob endpoint. It resolves only through the private DNS zone once the endpoint exists.')
output blobEndpoint string = account.properties.primaryEndpoints.blob

@description('Whether shared-key access is off. §3.5’s resource-graph assertion reads the live value; the composition reads it here.')
output sharedKeyAccessDisabled bool = false
