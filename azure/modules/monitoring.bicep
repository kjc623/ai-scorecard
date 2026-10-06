// monitoring.bicep — alerts on signals the platform actually produces: PostgreSQL, gateway, Front Door
// and Container Apps metrics, and error lines in the services' structured logs.

param location string

param baseName string

param logAnalyticsWorkspaceId string

param postgresServerId string

@description('Container app resource ids to watch for restarts. Empty on the bootstrap deployment.')
param containerAppIds array = []

@description('Container App job resource ids to watch for failed and stale executions. Empty on the bootstrap deployment.')
param jobIds array = []

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

// A job execution increments the Executions metric with a "state" dimension, so a non-zero count of
// "Failed" states means a run failed. A job that is never scheduled emits no metric, which is why
// the staleness checks below read the system logs instead.
resource jobFailed 'Microsoft.Insights/metricAlerts@2018-03-01' = [for jobId in jobIds: {
  name: '${baseName}-job-failed-${last(split(jobId, '/'))}'
  location: 'global'
  tags: tags
  properties: {
    description: 'A job execution failed: its container exited unsuccessfully.'
    severity: 1
    enabled: true
    scopes: [jobId]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          criterionType: 'StaticThresholdCriterion'
          name: 'Executions'
          metricName: 'Executions'
          metricNamespace: 'Microsoft.App/jobs'
          operator: 'GreaterThan'
          threshold: 0
          timeAggregation: 'Total'
          // A freshly deployed job has no execution history yet, so its metric has no data to
          // validate against until the first run.
          skipMetricValidation: true
          dimensions: [
            {
              name: 'state'
              operator: 'Include'
              values: ['Failed']
            }
          ]
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

// A scheduled job that stops produces no error line, so the error-burst rule above does not cover
// it. These count the job's non-error system log lines: a successful run logs at least one Info
// line, and a stopped job logs none, which reads as a count below one.
var jobStaleAlerts = [
  {
    name: 'aggregate-stale'
    job: 'aggregate'
    window: 'PT20M'
    evaluation: 'PT5M'
    description: 'No successful aggregate execution in 20 minutes; every dashboard figure goes stale.'
    severity: 1
  }
  {
    name: 'expire-stale'
    job: 'expire'
    window: 'PT26H'
    evaluation: 'PT1H'
    description: 'No successful expire execution in 26 hours; data outlives its promised retention.'
    severity: 1
  }
]

resource jobStale 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = [for a in jobStaleAlerts: if (!empty(jobIds)) {
  name: '${baseName}-${a.name}'
  location: location
  tags: tags
  properties: {
    displayName: '${baseName}: ${a.name}'
    description: a.description
    severity: a.severity
    enabled: true
    evaluationFrequency: a.evaluation
    windowSize: a.window
    scopes: [logAnalyticsWorkspaceId]
    criteria: {
      allOf: [
        {
          query: 'ContainerAppSystemLogs | where JobName == \'${a.job}\' | where Type == \'Info\''
          timeAggregation: 'Count'
          operator: 'LessThan'
          threshold: 1
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: { actionGroups: [actionGroup.id] }
  }
}]
