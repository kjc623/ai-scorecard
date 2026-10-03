// network.bicep — VNet, subnets, NSGs and private DNS zones (docs/05-platform-delivery.md §2, §2.1).
//
// One VNet per regional environment. VNet injection is what makes content-vault's internal-only
// ingress meaningful (§2 inventory), and private DNS zones linked to this VNet are what stop
// resolution falling back to a public answer — a private endpoint without a linked zone is a
// private endpoint that resolves publicly.

@description('Azure region for every resource in this environment. Never hard-coded: §3.2 makes region environment-parameterised, one production environment per residency region.')
param location string

@description('Environment name (dev | staging | prod). Used for naming and tags only; no resource behaviour depends on it.')
@allowed(['dev', 'staging', 'prod'])
param environment string

@description('Base name for the environment, e.g. sac-prod-eastus. Names are derived from it, never typed per resource.')
param baseName string

@description('VNet address space. One VNet per environment; the plan is deliberately /16 so a second residency region is a separate VNet rather than a second address range.')
param vnetAddressPrefix string = '10.40.0.0/16'

@description('Container Apps infrastructure subnet prefix. Must be /23 or larger: the Container Apps runtime reserves addresses per replica.')
param containerAppsSubnetPrefix string = '10.40.0.0/23'

@description('Private endpoint subnet prefix. Every PaaS resource in §2 gets one endpoint in this subnet, so it is sized for growth.')
param privateEndpointSubnetPrefix string = '10.40.8.0/24'

@description('DNS resolver subnet prefix, used by the private DNS resolution path (Q12).')
param dnsResolverSubnetPrefix string = '10.40.9.0/24'

@description('Private DNS zone names to create and link to this VNet, one per PaaS family in §2.')
param privateDnsZones array = [
  'privatelink.postgres.database.azure.com'
  'privatelink.blob.core.windows.net'
  'privatelink.vaultcore.azure.net'
  'privatelink.managedhsm.azure.net'
  'privatelink.azurecr.io'
  'privatelink.monitor.azure.com'
  'privatelink.oms.opinsights.azure.com'
  'privatelink.ods.opinsights.azure.com'
]

@description('Tags applied to every resource. Cost allocation depends on them: the budgets in budget.bicep scope by tag.')
param tags object = {}

resource vnet 'Microsoft.Network/virtualNetworks@2023-09-01' = {
  name: '${baseName}-vnet'
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: [
        vnetAddressPrefix
      ]
    }
    subnets: [
      {
        name: 'container-apps'
        properties: {
          addressPrefix: containerAppsSubnetPrefix
          delegations: [
            {
              name: 'Microsoft.App/environments'
              properties: {
                serviceName: 'Microsoft.App/environments'
              }
            }
          ]
        }
      }
      {
        name: 'private-endpoints'
        properties: {
          addressPrefix: privateEndpointSubnetPrefix
          privateEndpointNetworkPolicies: 'Disabled'
        }
      }
      {
        name: 'dns-resolver'
        properties: {
          addressPrefix: dnsResolverSubnetPrefix
        }
      }
    ]
  }
}

// One NSG on the private-endpoint subnet. The rules are deliberately deny-by-default: PaaS traffic
// arrives over private endpoints, so the only inbound rule is from the Container Apps subnet.
resource nsgPrivateEndpoints 'Microsoft.Network/networkSecurityGroups@2023-09-01' = {
  name: '${baseName}-nsg-pe'
  location: location
  tags: tags
  properties: {
    securityRules: [
      {
        name: 'allow-container-apps-inbound'
        properties: {
          priority: 100
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourceAddressPrefix: containerAppsSubnetPrefix
          destinationAddressPrefix: privateEndpointSubnetPrefix
          destinationPortRange: '443'
        }
      }
      {
        name: 'deny-internet-inbound'
        properties: {
          priority: 4000
          direction: 'Inbound'
          access: 'Deny'
          protocol: '*'
          sourceAddressPrefix: 'Internet'
          destinationAddressPrefix: '*'
          destinationPortRange: '*'
        }
      }
    ]
  }
}

resource zones 'Microsoft.Network/privateDnsZones@2020-06-01' = [for zone in privateDnsZones: {
  name: zone
  location: 'global'
  tags: tags
}]

resource zoneLinks 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2020-06-01' = [for (zone, i) in privateDnsZones: {
  name: '${zone}/${baseName}-link'
  location: 'global'
  tags: tags
  properties: {
    registrationEnabled: false
    virtualNetwork: {
      id: vnet.id
    }
  }
  dependsOn: [
    zones[i]
  ]
}]

@description('Resource id of the VNet, for modules that need to join a subnet or link a zone.')
output vnetId string = vnet.id

@description('Resource id of the Container Apps infrastructure subnet.')
output containerAppsSubnetId string = '${vnet.id}/subnets/container-apps'

@description('Resource id of the private-endpoint subnet.')
output privateEndpointSubnetId string = '${vnet.id}/subnets/private-endpoints'

@description('Resource id of the private-endpoint NSG.')
output privateEndpointNsgId string = nsgPrivateEndpoints.id

@description('The private DNS zones by name, so private-endpoints.bicep links exactly the zones it needs and no others.')
output privateDnsZoneIds object = reduce(privateDnsZones, {}, (acc, zone) => union(acc, { '${zone}': resourceId('Microsoft.Network/privateDnsZones', zone) }))
