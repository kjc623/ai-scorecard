// network.bicep — the environment's VNet, subnets, the origin-lock NSG and private DNS.

@description('Azure region.')
param location string

@description('Base name, e.g. sac-prod-eastus.')
param baseName string

@description('VNet address space.')
param vnetAddressPrefix string = '10.40.0.0/16'

@description('Container Apps environment subnet (workload profiles; /23 leaves room for scale-out).')
param containerAppsSubnetPrefix string = '10.40.0.0/23'

@description('PostgreSQL Flexible Server subnet, delegated to the server and used by nothing else.')
param postgresSubnetPrefix string = '10.40.4.0/24'

@description('Private endpoint subnet (Key Vault).')
param privateEndpointSubnetPrefix string = '10.40.8.0/24'

@description('Application Gateway subnet, dedicated to the gateway.')
param gatewaySubnetPrefix string = '10.40.10.0/24'

param tags object = {}

// The private DNS zone for a VNet-integrated PostgreSQL server must end in
// .postgres.database.azure.com and must not be the server's own name.
var postgresDnsZoneName = '${baseName}.private.postgres.database.azure.com'
var keyVaultDnsZoneName = 'privatelink.vaultcore.azure.net'

// The origin lock: device requests carry a forwarded client certificate, which is only trustworthy if
// nothing but Application Gateway can reach the device-facing apps. Inside the environment subnet,
// apps reach each other and Front Door's Private Link traffic arrives through the environment's own
// private link service, so intra-subnet traffic is allowed; from the rest of the VNet only the
// gateway subnet is.
resource nsgContainerApps 'Microsoft.Network/networkSecurityGroups@2024-05-01' = {
  name: '${baseName}-nsg-container-apps'
  location: location
  tags: tags
  properties: {
    securityRules: [
      {
        name: 'allow-gateway'
        properties: {
          priority: 100
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourceAddressPrefix: gatewaySubnetPrefix
          sourcePortRange: '*'
          destinationAddressPrefix: containerAppsSubnetPrefix
          destinationPortRanges: ['80', '443']
        }
      }
      {
        name: 'allow-environment'
        properties: {
          priority: 110
          direction: 'Inbound'
          access: 'Allow'
          protocol: '*'
          sourceAddressPrefix: containerAppsSubnetPrefix
          sourcePortRange: '*'
          destinationAddressPrefix: containerAppsSubnetPrefix
          destinationPortRange: '*'
        }
      }
      {
        name: 'allow-load-balancer'
        properties: {
          priority: 120
          direction: 'Inbound'
          access: 'Allow'
          protocol: '*'
          sourceAddressPrefix: 'AzureLoadBalancer'
          sourcePortRange: '*'
          destinationAddressPrefix: '*'
          destinationPortRange: '*'
        }
      }
      {
        name: 'deny-vnet'
        properties: {
          priority: 4000
          direction: 'Inbound'
          access: 'Deny'
          protocol: '*'
          sourceAddressPrefix: 'VirtualNetwork'
          sourcePortRange: '*'
          destinationAddressPrefix: '*'
          destinationPortRange: '*'
        }
      }
    ]
  }
}

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' = {
  name: '${baseName}-vnet'
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: [vnetAddressPrefix]
    }
    subnets: [
      {
        name: 'container-apps'
        properties: {
          addressPrefix: containerAppsSubnetPrefix
          networkSecurityGroup: { id: nsgContainerApps.id }
          delegations: [
            {
              name: 'container-apps'
              properties: { serviceName: 'Microsoft.App/environments' }
            }
          ]
        }
      }
      {
        name: 'postgres'
        properties: {
          addressPrefix: postgresSubnetPrefix
          delegations: [
            {
              name: 'postgres'
              properties: { serviceName: 'Microsoft.DBforPostgreSQL/flexibleServers' }
            }
          ]
        }
      }
      {
        name: 'private-endpoints'
        properties: {
          addressPrefix: privateEndpointSubnetPrefix
        }
      }
      {
        name: 'application-gateway'
        properties: {
          addressPrefix: gatewaySubnetPrefix
        }
      }
    ]
  }
}

resource postgresZone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: postgresDnsZoneName
  location: 'global'
  tags: tags
}

resource keyVaultZone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: keyVaultDnsZoneName
  location: 'global'
  tags: tags
}

resource postgresZoneLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: postgresZone
  name: '${baseName}-vnet'
  location: 'global'
  tags: tags
  properties: {
    registrationEnabled: false
    virtualNetwork: { id: vnet.id }
  }
}

resource keyVaultZoneLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: keyVaultZone
  name: '${baseName}-vnet'
  location: 'global'
  tags: tags
  properties: {
    registrationEnabled: false
    virtualNetwork: { id: vnet.id }
  }
}

output containerAppsSubnetId string = '${vnet.id}/subnets/container-apps'
output postgresSubnetId string = '${vnet.id}/subnets/postgres'
output privateEndpointSubnetId string = '${vnet.id}/subnets/private-endpoints'
output gatewaySubnetId string = '${vnet.id}/subnets/application-gateway'
output postgresDnsZoneId string = postgresZone.id
output keyVaultDnsZoneId string = keyVaultZone.id
