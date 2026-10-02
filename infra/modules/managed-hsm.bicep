// managed-hsm.bicep — the conditional pool for customer-held keys (docs/05-platform-delivery.md
// §2, §5.3, §11.6).
//
// This module is instantiated only for a tenant whose contract requires vendor-blind key custody:
// §11.6 prices the pool at ~4× the rest of a tenant, and §2 makes it per-contract rather than
// per-region-by-default. The quorum is the point — a key policy the vendor alone cannot satisfy is
// what makes "the vendor cannot read content at all" a statement rather than a promise.

@description('Azure region for the pool. Environment-parameterised (§3.2); one pool per contract region.')
param location string

@description('Base name for the environment; the pool name derives from it.')
param baseName string

@description('Pool SKU. The value is a parameter because the SKU is a commercial decision per contract, not a module constant.')
param hsmSku string

@description('Administrator object ids. §5.3: 2-of-3 quorum, so exactly three administrators are expected and the vendor holds at most one of them.')
@minLength(3)
@maxLength(3)
param administratorObjectIds array

@description('Whether to instantiate the pool at all. §2: per-contract only. A flag rather than a separate composition keeps the conditional visible in one place.')
param deployManagedHsm bool = false

@description('Tags applied to every resource.')
param tags object = {}

resource pool 'Microsoft.KeyVault/managedHSMs@2023-07-01' = if (deployManagedHsm) {
  name: '${baseName}-hsm'
  location: location
  tags: tags
  sku: {
    name: hsmSku
    family: 'B'
  }
  properties: {
    tenantId: subscription().tenantId
    initialAdminObjectIds: administratorObjectIds
    enablePurgeProtection: true
    softDeleteRetentionInDays: 90
    publicNetworkAccess: 'Disabled'
    createMode: 'default'
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'AzureServices'
    }
  }
}

@description('Whether the pool was deployed. The composition and the cost model both read this rather than assuming.')
output deployed bool = deployManagedHsm

@description('Resource id of the pool, empty when not deployed.')
output hsmId string = deployManagedHsm ? pool.id : ''

@description('The quorum policy summary, for the contract review that precedes a customer-held-key tenant.')
output quorum string = '${length(administratorObjectIds)}-of-${length(administratorObjectIds)}'
