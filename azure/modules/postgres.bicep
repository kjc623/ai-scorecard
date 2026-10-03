// postgres.bicep — Azure Database for PostgreSQL Flexible Server 16 (docs/05-platform-delivery.md
// §2, §2.1, §12.2).
//
// The property that matters is structural, not cosmetic: the server has publicNetworkAccess
// Disabled, it is reachable only through a private endpoint in the delegated subnet, and there are
// **no firewall rules at all**, so "reachable from the internet" is not a configuration state it can
// be put into (§2.1). The checker refuses a firewall-rule resource anywhere in the tree.
//
// Server parameters encode the rest of §3: extensions needed by ingest.search_text, forced row-level
// security for application roles, and statement/lock timeouts so a migration cannot stall ingest
// (§3.4). Values come from the parameter file; none is compiled in.

@description('Azure region for the server. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment.')
param baseName string

@description('PostgreSQL major version. 16 per §2 inventory.')
@allowed(['15', '16'])
param majorVersion string = '16'

@description('Compute SKU name, e.g. D2ds_v5 in production, B_Standard_B2s in dev. §3.1: the ladder differs on SKU, and the module must not decide it.')
param skuName string

@description('Compute tier, e.g. GeneralPurpose or Burstable. Paired with skuName; the composition sets both from the environment parameter file.')
@allowed(['Burstable', 'GeneralPurpose', 'MemoryOptimized'])
param skuTier string

@description('Provisioned storage in GiB. 32 dev, 64 staging, 128 production (§3.1).')
param storageSizeGb int = 128

@description('High-availability mode: ZoneRedundant in production (the availability floor for 99.9% ingest), Disabled in dev and staging (§3.1).')
@allowed(['Disabled', 'ZoneRedundant', 'SameZone'])
param highAvailabilityMode string = 'ZoneRedundant'

@description('Standby availability zone when high availability is zone-redundant. Empty lets Azure choose; production pins it so a failover target is known.')
param standbyAvailabilityZone string = '2'

@description('PITR retention in days. 7 dev, 14 staging, 35 production (§3.1); §12.2 requires 35 in production.')
@minValue(7)
@maxValue(35)
param backupRetentionDays int = 35

@description('Geo-redundant backup. Required in production by §12.2; the dev setting is local redundancy.')
param geoRedundantBackup bool = true

@description('Administrator login name. Authentication is Entra-token based, so this account is for break-glass only and holds a credential supplied at deploy time from Key Vault, never from the repository (§5.1).')
param administratorLogin string

@description('Resource id of the delegated subnet the server joins, from network.bicep.')
param delegatedSubnetId string

@description('Private DNS zone resource id for privatelink.postgres.database.azure.com, from network.bicep. Azure requires it here so the server gets an address only inside the VNet.')
param privateDnsZoneId string

@description('Databases to create. §2: the schemas are ingest, ops, mart and ref; migrations own their creation (§3.4), so this creates only the database, not the tables.')
param databases array = ['sac']

@description('Server parameters applied to every database. Values are §3.4 and Q12 decisions, passed in rather than compiled in.')
param serverParameters object = {
  'row_security': 'on'
  'lock_timeout': '5000'
  'statement_timeout': '30000'
  'idle_in_transaction_session_timeout': '60000'
  'azure.extensions': 'PG_TRGM,BTREE_GIN'
  'log_checkpoints': 'on'
  'connection_throttle.enable': 'on'
}

@description('Tags applied to every resource.')
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
    version: majorVersion
    administratorLogin: administratorLogin
    // The Entra administrator is the identity services authenticate as; no database password exists
    // (§5.1). It is supplied as a principal id at deploy time.
    authConfig: {
      activeDirectoryAuth: 'Enabled'
      passwordAuth: 'Disabled'
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
      standbyAvailabilityZone: highAvailabilityMode == 'ZoneRedundant' ? standbyAvailabilityZone : null
    }
    network: {
      delegatedSubnetResourceId: delegatedSubnetId
      privateDnsZoneArmResourceId: privateDnsZoneId
      publicNetworkAccess: 'Disabled'
    }
    // No firewallRules property exists here on purpose: the server is reachable only from the VNet,
    // and a rule would be the one way to change that (§2.1).
    replicationRole: 'Primary'
  }
}

resource database 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2024-08-01' = [for db in databases: {
  name: '${server.name}/${db}'
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
}]

resource parameters 'Microsoft.DBforPostgreSQL/flexibleServers/configurations@2024-08-01' = [for item in items(serverParameters): {
  name: '${server.name}/${item.key}'
  properties: {
    value: item.value
    source: 'user-override'
  }
}]

@description('The server FQDN. It resolves only inside the VNet, through the private DNS zone.')
output serverFqdn string = server.properties.fullyQualifiedDomainName

@description('Resource id of the server.')
output serverId string = server.id

@description('The provisioned storage in GiB, for the cost model and the §10.3 storage alert.')
output storageSizeGb int = storageSizeGb

@description('Whether zone-redundant HA is on. The §3.5 resource-graph assertion checks the live value; the alert on failover events is in monitoring.bicep.')
output zoneRedundantHa bool = highAvailabilityMode == 'ZoneRedundant'
