// monitoring.bicep — alerts on signals the platform actually produces: PostgreSQL, gateway, Front Door
// and Container Apps metrics, and error lines in the services' structured logs.

param location string

param baseName string

param logAnalyticsWorkspaceId string

param postgresServerId string

@description('Container app resource ids to watch for restarts. Empty on the bootstrap deployment.')
param containerAppIds array = []

@description('Application Gateway resource id, or empty when it is not deployed.')
param applicationGatewayId string = ''

@description('Front Door profile resource id, or empty when it is not deployed.')
param frontDoorProfileId string = ''

@description('Email addresses that receive every alert.')
param alertEmails array

param tags object = {}

resource actionGroup 'Microsoft.Insights/actionGroups@2023-01-01' = {
  name: '${baseName}-alerts'
  location: 'global'
  tags: tags
  properties: {
    groupShortName: 'sac-alerts'
    enabled: true
    emailReceivers: [for (address, i) in alertEmails: {
      name: 'email-${i}'
      emailAddress: address
      useCommonAlertSchema: true
    }]
  }
}

var metricAlerts = concat([
  {
    name: 'postgres-down'
    description: 'PostgreSQL is not answering. Ingest, the dashboard and content retrieval are down; devices keep spooling.'
    severity: 0
    scope: postgresServerId
    metric: 'is_db_alive'
    operator: 'LessThan'
    threshold: 1
    aggregation: 'Minimum'
    window: 'PT5M'
  }
  {
    name: 'postgres-cpu'
    description: 'PostgreSQL CPU has been above 90% for 15 minutes.'
    severity: 2
    scope: postgresServerId
    metric: 'cpu_percent'
    operator: 'GreaterThan'
    threshold: 90
    aggregation: 'Average'
    window: 'PT15M'
  }
  {
    name: 'postgres-storage'
    description: 'PostgreSQL storage is above 85%; auto-grow is on, but the bill grows with it.'
    severity: 2
    scope: postgresServerId
    metric: 'storage_percent'
    operator: 'GreaterThan'
    threshold: 85
    aggregation: 'Maximum'
    window: 'PT15M'
  }
], empty(applicationGatewayId) ? [] : [
  {
    name: 'device-edge-unhealthy-backend'
    description: 'Application Gateway sees an unhealthy backend: devices cannot deliver events or enrol.'
    severity: 1
    scope: applicationGatewayId
    metric: 'UnhealthyHostCount'
    operator: 'GreaterThan'
    threshold: 0
    aggregation: 'Maximum'
    window: 'PT5M'
  }
], empty(frontDoorProfileId) ? [] : [
  {
    name: 'browser-edge-origin-health'
    description: 'Front Door reports its origins below 90% healthy: the dashboard or sign-in is failing.'
    severity: 1
    scope: frontDoorProfileId
    metric: 'OriginHealthPercentage'
    operator: 'LessThan'
    threshold: 90
    aggregation: 'Average'
    window: 'PT5M'
  }
], map(containerAppIds, appId => {
  name: 'restarts-${last(split(appId, '/'))}'
  description: 'The container app restarted more than three times in 15 minutes.'
  severity: 2
  scope: appId
  metric: 'RestartCount'
  operator: 'GreaterThan'
  threshold: 3
  aggregation: 'Total'
  window: 'PT15M'
}))

resource metric 'Microsoft.Insights/metricAlerts@2018-03-01' = [for a in metricAlerts: {
  name: '${baseName}-${a.name}'
  location: 'global'
  tags: tags
  properties: {
    description: a.description
    severity: a.severity
    enabled: true
    scopes: [a.scope]
    evaluationFrequency: 'PT1M'
    windowSize: a.window
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          criterionType: 'StaticThresholdCriterion'
          name: a.metric
          metricName: a.metric
          operator: a.operator
          threshold: a.threshold
          timeAggregation: a.aggregation
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}]

// Every service and job logs JSON lines with a "level" field; a burst of errors is the earliest
// signal of a failing dependency or a failing job run.
resource errorLogs 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = {
  name: '${baseName}-error-logs'
  location: location
  tags: tags
  properties: {
    displayName: '${baseName}: error log burst'
    description: 'More than 20 ERROR log lines from the services and jobs in 15 minutes.'
    severity: 2
    enabled: true
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    scopes: [logAnalyticsWorkspaceId]
    criteria: {
      allOf: [
        {
          query: 'ContainerAppConsoleLogs | where Log has \'"level":"ERROR"\''
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 20
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: { actionGroups: [actionGroup.id] }
  }
}
