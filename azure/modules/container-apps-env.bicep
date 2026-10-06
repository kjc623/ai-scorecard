// container-apps-env.bicep — the VNet-integrated Container Apps environment. Its load balancer is
// internal, so no app has a public address: devices arrive through Application Gateway and browsers
// through Front Door's Private Link origin.

@description('Azure region.')
param location string

@description('Base name, e.g. sac-prod-eastus.')
param baseName string

@description('Delegated Container Apps subnet.')
param infrastructureSubnetId string

param logAnalyticsWorkspaceId string

param zoneRedundant bool

param tags object = {}

resource environment 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: '${baseName}-cae'
  location: location
  tags: tags
  properties: {
    vnetConfiguration: {
      infrastructureSubnetId: infrastructureSubnetId
      internal: true
    }
    workloadProfiles: [
      {
        name: 'Consumption'
        workloadProfileType: 'Consumption'
      }
    ]
    zoneRedundant: zoneRedundant
    // Logs go to Azure Monitor and from there, through the diagnostic setting below, to the workspace.
    appLogsConfiguration: {
      destination: 'azure-monitor'
    }
  }
}

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: 'law'
  scope: environment
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logAnalyticsDestinationType: 'Dedicated'
    logs: [
      { category: 'ContainerAppConsoleLogs', enabled: true }
      { category: 'ContainerAppSystemLogs', enabled: true }
    ]
    metrics: [
      { category: 'AllMetrics', enabled: true }
    ]
  }
}

output environmentId string = environment.id
output staticIp string = environment.properties.staticIp
output defaultDomain string = environment.properties.defaultDomain
