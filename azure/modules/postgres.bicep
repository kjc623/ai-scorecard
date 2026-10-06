// postgres.bicep — Azure Database for PostgreSQL Flexible Server 16.
//
// VNet-integrated (no public endpoint, no firewall rules) and Entra-only: there is no password. The
// migration job's managed identity is the Entra administrator; it applies the schema and maps each
// workload identity to its database role. An optional Entra group gives operators break-glass access.

@description('Azure region.')
param location string

@description('Base name, e.g. sac-prod-eastus.')
param baseName string

@description('Compute SKU, e.g. Standard_B2s or Standard_D2ds_v5.')
param skuName string

@allowed(['Burstable', 'GeneralPurpose', 'MemoryOptimized'])
param skuTier string

@description('Provisioned storage in GiB.')
param storageSizeGb int

@description('ZoneRedundant in production.')
@allowed(['Disabled', 'ZoneRedundant'])
param highAvailabilityMode string

@minValue(7)
@maxValue(35)
param backupRetentionDays int

param geoRedundantBackup bool

@description('Delegated PostgreSQL subnet.')
param delegatedSubnetId string

@description('Private DNS zone id (…private.postgres.database.azure.com).')
param privateDnsZoneId string

@description('The migration identity, made Entra administrator: { principalId, name }.')
param migrationAdmin object

@description('Optional operator group made Entra administrator: { objectId, name }, or {} for none.')
param operatorAdminGroup object = {}

@description('Database name.')
param databaseName string = 'sac'

param logAnalyticsWorkspaceId string

param tags object = {}

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2024-08-01' = {
  name: '${baseName}-pg'
  location: location
  tags: tags
  sku: {
    name: skuName
    tier: skuTier
  }
  properties: {
    version: '16'
    authConfig: {
      activeDirectoryAuth: 'Enabled'
      passwordAuth: 'Disabled'
      tenantId: tenant().tenantId
    }
    storage: {
      storageSizeGB: storageSizeGb
      autoGrow: 'Enabled'
    }
    backup: {
      backupRetentionDays: backupRetentionDays
      geoRedundantBackup: geoRedundantBackup ? 'Enabled' : 'Disabled'
    }
    highAvailability: {
      mode: highAvailabilityMode
    }
    network: {
      delegatedSubnetResourceId: delegatedSubnetId
      privateDnsZoneArmResourceId: privateDnsZoneId
      publicNetworkAccess: 'Disabled'
    }
  }
}

resource migrationAdministrator 'Microsoft.DBforPostgreSQL/flexibleServers/administrators@2024-08-01' = {
  parent: server
  name: migrationAdmin.principalId
  properties: {
    principalType: 'ServicePrincipal'
    principalName: migrationAdmin.name
    tenantId: tenant().tenantId
  }
}

resource operatorAdministrator 'Microsoft.DBforPostgreSQL/flexibleServers/administrators@2024-08-01' = if (!empty(operatorAdminGroup)) {
  parent: server
  name: operatorAdminGroup.?objectId ?? 'none'
  properties: {
    principalType: 'Group'
    principalName: operatorAdminGroup.?name ?? 'none'
    tenantId: tenant().tenantId
  }
  dependsOn: [migrationAdministrator]
}

resource database 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2024-08-01' = {
  parent: server
  name: databaseName
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
}

// The server applies configuration changes one at a time.
var parameters = {
  'azure.extensions': 'pg_trgm,btree_gin'
  statement_timeout: '30000'
  lock_timeout: '5000'
  idle_in_transaction_session_timeout: '60000'
}

@batchSize(1)
resource configuration 'Microsoft.DBforPostgreSQL/flexibleServers/configurations@2024-08-01' = [for p in items(parameters): {
  parent: server
  name: p.key
  properties: {
    value: p.value
    source: 'user-override'
  }
  dependsOn: [operatorAdministrator]
}]

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: 'law'
  scope: server
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logs: [
      { category: 'PostgreSQLLogs', enabled: true }
    ]
    metrics: [
      { category: 'AllMetrics', enabled: true }
    ]
  }
}

output serverId string = server.id
output fqdn string = server.properties.fullyQualifiedDomainName
output databaseName string = database.name
