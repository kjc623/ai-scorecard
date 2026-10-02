// container-apps-env.bicep — the VNet-injected Container Apps environment (docs/05-platform-
// delivery.md §2, §2.1, §3.3).
//
// No container app holds a public IP: the environment joins the delegated subnet and its load
// balancer is **internal**. That is what makes content-vault's internal-only ingress meaningful —
// with an external environment LB, "internal ingress" would still be reachable through the
// environment's own public address.

@description('Azure region for the environment. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment.')
param baseName string

@description('Resource id of the delegated Container Apps subnet from network.bicep. Passed in by the composition; no module reaches into another with `existing` (§3.3).')
param infrastructureSubnetId string

@description('Resource id of the Log Analytics workspace. Every app writes here; there is no second logging path (§10.4).')
param logAnalyticsWorkspaceId string

@description('Zone redundancy for the environment. Enabled in production (§2 inventory), disabled in dev to keep the ladder honest about cost.')
param zoneRedundant bool = true

@description('The environment’s internal load balancer must always be internal. The parameter exists so the composition states it, and the checker asserts the value is true: a public environment LB would defeat content-vault’s internal ingress.')
param internalLoadBalancer bool = true

@description('Tags applied to every resource.')
param tags object = {}

resource environment 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: '${baseName}-cae'
  location: location
  tags: tags
  properties: {
    vnetConfiguration: {
      infrastructureSubnetId: infrastructureSubnetId
      internal: internalLoadBalancer
    }
    zoneRedundant: zoneRedundant
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: reference(logAnalyticsWorkspaceId, '2020-08-01').customerId
        sharedKey: null // workspace-based authentication: no shared key exists to leak (§5.1)
      }
    }
    peerAuthentication: {
      mtls: {
        enabled: false
      }
    }
  }
}

@description('Resource id of the environment, for every container app and job module call.')
output environmentId string = environment.id

@description('The environment’s static IP. Internal, and the §3.5 resource-graph assertion confirms it is not public.')
output staticIp string = environment.properties.staticIp

@description('The internal load balancer property, echoed so the composition can assert it rather than trust the default.')
output internalIngress bool = internalLoadBalancer
