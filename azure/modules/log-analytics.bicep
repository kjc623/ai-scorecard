// log-analytics.bicep — the environment's one log workspace. Services log structured JSON to stdout;
// the Container Apps environment ships it here, and every alert queries it.

@description('Azure region.')
param location string

@description('Base name, e.g. sac-prod-eastus.')
param baseName string

@description('Interactive retention in days.')
@minValue(30)
@maxValue(730)
param retentionInDays int

@description('Daily ingestion cap in GiB, so a logging defect becomes an alert rather than a bill.')
param dailyQuotaGb int = 20

param tags object = {}

// Ingestion arrives through diagnostic settings and queries are authorised by Entra RBAC, so the
// workspace keeps its public endpoints; resource-context access limits each reader to what their
// role on the resource allows.
resource workspace 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: '${baseName}-law'
  location: location
  tags: tags
  properties: {
    sku: { name: 'PerGB2018' }
    retentionInDays: retentionInDays
    features: {
      enableLogAccessUsingOnlyResourcePermissions: true
    }
    workspaceCapping: {
      dailyQuotaGb: dailyQuotaGb
    }
  }
}

output workspaceId string = workspace.id
