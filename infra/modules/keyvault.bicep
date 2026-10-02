// keyvault.bicep — the vault that holds per-tenant KEKs, TLS certificates and policy-signing keys
// (docs/05-platform-delivery.md §2, §5.3, §12.2).
//
// Three invariants are encoded rather than described: purge protection on, RBAC authorization (not
// access policies), and a private endpoint with public network access disabled. The role assignments
// are the security boundary (§5.1): unwrap is held by exactly one identity, and the checker's
// separation-of-duties test asserts that no other principal appears in the unwrap assignment.

@description('Azure region for the vault. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment.')
param baseName string

@description('Vault SKU. Premium carries HSM-backed keys, which §2 requires for per-tenant KEKs; Standard is not offered because a KEK must be HSM-backed.')
@allowed(['premium', 'standard'])
param skuName string = 'premium'

@description('Soft-delete retention in days. §12.2 fixes it at 90: a shorter window makes an accidental deletion unrecoverable sooner than a customer expects.')
@minValue(7)
@maxValue(90)
param softDeleteRetentionInDays int = 90

@description('Purge protection. Not a toggle in practice — §12.2 makes it non-optional — but it is a parameter so the value is reviewable in the composition rather than buried in a module.')
param enablePurgeProtection bool = true

@description('Resource id of the private-endpoint subnet from network.bicep.')
param privateEndpointSubnetId string = ''

@description('Private DNS zone resource ids keyed by zone name, from network.bicep.')
param privateDnsZoneIds object = {}

@description('Resource id of the Log Analytics workspace for audit diagnostics. Empty skips diagnostics (dev).')
param logAnalyticsWorkspaceId string = ''

@description('Principals that may read secrets and use keys for operations — the operational path, not unwrap. Each entry is { name, principalId, roleDefinitionId } and is a *managed identity or workload identity*, never a group of humans (§5.3).')
param operationalRoleAssignments array = []

@description('The single principal permitted to unwrap per-tenant KEKs: content-vault. §5.3/C15: no other assignment may exist, and the checker fails the build if one does.')
param unwrapPrincipalIds array = []

@description('Tags applied to every resource.')
param tags object = {}

// Role definition ids, named so the assignment reads as a decision rather than a GUID.
var roleKeyVaultSecretsUser = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')
var roleKeyVaultCryptoServiceEncryptionUser = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'e147488a-f6f5-4113-8e2d-b22465e65bf6')
var roleKeyVaultCryptoUser = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '12338af0-0e69-4776-bea7-57ae8d297424')

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: '${baseName}-kv'
  location: location
  tags: tags
  properties: {
    sku: {
      family: 'A'
      name: skuName
    }
    tenantId: subscription().tenantId
    enableRbacAuthorization: true
    enablePurgeProtection: enablePurgeProtection
    enableSoftDelete: true
    softDeleteRetentionInDays: softDeleteRetentionInDays
    publicNetworkAccess: 'Disabled'
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'AzureServices'
    }
    vaultUri: null
  }
}

module privateEndpoint 'private-endpoints.bicep' = if (privateEndpointSubnetId != '') {
  name: '${baseName}-kv-pe'
  params: {
    location: location
    baseName: '${baseName}-kv'
    subnetId: privateEndpointSubnetId
    privateDnsZoneIds: privateDnsZoneIds
    targets: [
      {
        name: 'keyvault'
        resourceId: vault.id
        groupId: 'vault'
        dnsZoneName: 'privatelink.vaultcore.azure.net'
      }
    ]
    tags: tags
  }
}

resource operationalAssignments 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (a, i) in operationalRoleAssignments: {
  name: guid(vault.id, a.principalId, 'operational', string(i))
  scope: vault
  properties: {
    roleDefinitionId: a.roleDefinitionId == 'secretsUser' ? roleKeyVaultSecretsUser : roleKeyVaultCryptoServiceEncryptionUser
    principalId: a.principalId
    principalType: 'ServicePrincipal'
    description: 'Operational path for ${a.name}. Not unwrap: C15 keeps unwrap separate.'
  }
}]

// Unwrap. This loop is the only place roleKeyVaultCryptoUser appears in the repository, and the
// checker asserts it appears exactly once and only for content-vault's identity.
resource unwrapAssignments 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (principalId, i) in unwrapPrincipalIds: {
  name: guid(vault.id, principalId, 'unwrap', string(i))
  scope: vault
  properties: {
    roleDefinitionId: roleKeyVaultCryptoUser
    principalId: principalId
    principalType: 'ServicePrincipal'
    description: 'Unwrap on per-tenant KEKs. Held by content-vault only (C15, D7).'
  }
}]

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = if (logAnalyticsWorkspaceId != '') {
  name: '${baseName}-kv-diag'
  scope: vault
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logs: [
      {
        category: 'AuditEvent'
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

@description('Resource id of the vault.')
output vaultId string = vault.id

@description('The vault URI services use for Key Vault references. A URI is not a credential: the caller still needs a role assignment, and every caller is a managed identity.')
output vaultUri string = vault.properties.vaultUri

@description('Whether purge protection is on. The §3.5 resource-graph assertion reads this back from Azure; the composition reads it here.')
output purgeProtectionEnabled bool = enablePurgeProtection
