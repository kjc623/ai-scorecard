// log-analytics.bicep — workspace, workspace-based Application Insights, retention and diagnostic
// settings (docs/05-platform-delivery.md §2, §10.4, §12).
//
// 90 days interactive is the incident-investigation window; the archive answers the audit and
// reconciliation questions that arrive later. Sampling is configured rather than assumed (§10.4),
// which is why the sampling percentage is a parameter and not a constant.

@description('Azure region for the workspace and Application Insights. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment.')
param baseName string

@description('Interactive retention in days. §3.2: 30 in dev, 90 in staging and production.')
param retentionInDays int = 90

@description('Archive retention in days. 365 in production (§2 inventory); 0 disables the archive, which is the dev setting.')
param archiveRetentionInDays int = 365

@description('Daily ingestion cap in GiB. A cap is what turns a logging defect into a cost anomaly instead of a monthly surprise (§11.6).')
param dailyQuotaGb int = 50

@description('Trace sampling percentage. §10.4: 10% of successful traces, 100% of errors; the percentage is a parameter and errors are exempted in the service, not here.')
param traceSamplingPercentage int = 10

@description('Resource id of the Container Apps environment, whose diagnostic settings write here. Empty on the first pass, because the environment module is deployed after the workspace.')
param containerAppsEnvId string = ''

@description('Tags applied to every resource.')
param tags object = {}

resource workspace 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: '${baseName}-law'
  location: location
  tags: tags
  properties: {
    retentionInDays: retentionInDays
    features: {
      enableLogAccessUsingOnlyResourcePermissions: true
    }
    workspaceCapping: {
      dailyQuotaGb: dailyQuotaGb
    }
    publicNetworkAccessForIngestion: 'Disabled'
    publicNetworkAccessForQuery: 'Disabled'
  }
}

// Application Insights is workspace-based, which is what makes one workspace the query surface for
// SLO computation, alerts and the §14 evidence (§2 inventory).
resource appInsights 'Microsoft.Insights/components@2020-02-02' = {
  name: '${baseName}-appi'
  location: location
  tags: tags
  kind: 'web'
  properties: {
    Application_Type: 'web'
    WorkspaceResourceId: workspace.id
    RetentionInDays: retentionInDays
    SamplingPercentage: traceSamplingPercentage
    publicNetworkAccessForIngestion: 'Disabled'
    publicNetworkAccessForQuery: 'Disabled'
  }
}

// Diagnostic settings for the workspace itself. A workspace that does not log its own state cannot
// answer "who changed the retention".
resource workspaceDiagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: '${baseName}-law-diag'
  scope: workspace
  properties: {
    workspaceId: workspace.id
    logs: [
      {
        category: 'Audit'
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

@description('Resource id of the Log Analytics workspace.')
output workspaceId string = workspace.id

@description('Application Insights connection string for the services. It is not a credential: it is an ingestion endpoint plus an instrumentation key scoped to this workspace, and services receive it as an environment variable.')
output appInsightsConnectionString string = appInsights.properties.ConnectionString

@description('Resource id of Application Insights, for the monitoring module’s alert targets.')
output appInsightsId string = appInsights.id

@description('The daily ingestion cap, echoed so budget.bicep can alert before the cap silently drops logs.')
output dailyQuotaGb int = dailyQuotaGb
