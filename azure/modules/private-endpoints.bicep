// private-endpoints.bicep — one private endpoint per PaaS resource, plus the DNS zone group that
// binds it to the zone network.bicep created (docs/05-platform-delivery.md §2, §2.1, §3.3).
//
// A private endpoint with no zone group is a resource that still resolves to a public address, so
// the zone group is not optional here: it is the difference between "has a private endpoint" and
// "is reachable only privately".

@description('Azure region for the endpoints. Environment-parameterised; never a literal (§3.2).')
param location string

@description('Base name for the environment; endpoint names derive from it.')
param baseName string

@description('Resource id of the private-endpoint subnet from network.bicep. Passed in by the composition: no module reaches into another module with `existing` (§3.3).')
param subnetId string

@description('Private DNS zone resource ids, keyed by zone name, from network.bicep.')
param privateDnsZoneIds object

@description('The PaaS targets to expose privately. Each entry names the resource id, the ARM sub-resource, and the DNS zone it must resolve through. §2 requires one endpoint per PostgreSQL server, both storage accounts, Key Vault, Managed HSM, ACR and Log Analytics.')
param targets array

@description('Tags applied to every endpoint.')
param tags object = {}

resource endpoints 'Microsoft.Network/privateEndpoints@2023-09-01' = [for target in targets: {
  name: '${baseName}-pe-${target.name}'
  location: location
  tags: tags
  properties: {
    subnet: {
      id: subnetId
    }
    privateLinkServiceConnections: [
      {
        name: '${baseName}-pe-${target.name}-connection'
        properties: {
          privateLinkServiceId: target.resourceId
          groupIds: [
            target.groupId
          ]
        }
      }
    ]
  }
}]

resource zoneGroups 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2023-09-01' = [for (target, i) in targets: {
  name: '${baseName}-pe-${target.name}/default'
  properties: {
    privateDnsZoneConfigs: [
      {
        name: target.dnsZoneName
        properties: {
          privateDnsZoneId: privateDnsZoneIds[target.dnsZoneName]
        }
      }
    ]
  }
  dependsOn: [
    endpoints[i]
  ]
}]

@description('The private endpoint resource ids, keyed by target name, for diagnostics and for the resource-graph assertions in §3.5.')
output endpointIds object = reduce(targets, {}, (acc, target) => union(acc, { '${target.name}': resourceId('Microsoft.Network/privateEndpoints', '${baseName}-pe-${target.name}') }))

@description('How many endpoints were created. The §3.5 resource-graph assertion compares this against the PaaS resources that exist.')
output endpointCount int = length(targets)
