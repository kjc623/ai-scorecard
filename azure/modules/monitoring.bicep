// monitoring.bicep — the §10.3 alert set, action groups and SLO definitions
// (docs/05-platform-delivery.md §10.2, §10.3).
//
// Every alert here names what it means for a customer; that is the test the document applies, and an
// alert whose meaning cannot be stated that way is a dashboard or a defect. The set is data: one
// array of { name, severity, query, threshold, windowMinutes, meaning }, evaluated on the shared
// workspace, so adding an alert is a reviewed parameter change rather than a new resource type.

@description('Azure region for the alert resources. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment.')
param baseName string

@description('Resource id of the Log Analytics workspace the alert queries run against (§10.4: one workspace, one query surface).')
param logAnalyticsWorkspaceId string

@description('Resource id of the Application Insights component, for the availability and failure-rate signals that arrive as App Metrics.')
param appInsightsId string

@description('Email recipients per severity: { sev1: [], sev2: [], sev3: [] }. §3.2 makes routing environment-parameterised, and dev routes nowhere.')
param emailReceivers object = {
  sev1: []
  sev2: []
  sev3: []
}

@description('Webhook or ITSM endpoints per severity, for paging systems. Empty in dev.')
param webhookReceivers object = {
  sev1: []
  sev2: []
  sev3: []
}

@description('The §10.3 alert set. Each entry: { name, severity, meaning, query, threshold, windowMinutes, evaluationFrequencyMinutes, metricMeasureColumn, resourceColumn }. The queries are supplied per environment because they name the tables the services write.')
param alerts array = [
  {
    name: 'ingest-availability-burn'
    severity: 1
    meaning: 'Nothing is arriving. Devices are spooling and will flush; if the spool fills, events are lost and counted.'
    query: 'requests | where timestamp > ago(1h) | where name == \'POST /v1/events\' | summarize total = count(), failed = countif(success == false) | extend burn = failed * 100.0 / max(total, 1) | project burn'
    threshold: 1
    windowMinutes: 60
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'ingest-5xx-rate'
    severity: 1
    meaning: 'Same as the availability burn, earlier.'
    query: 'requests | where timestamp > ago(5m) | where name startswith \'POST /v1/events\' | summarize rate = countif(resultCode startswith \'5\') * 100.0 / max(count(), 1) | project rate'
    threshold: 1
    windowMinutes: 5
    evaluationFrequencyMinutes: 1
  }
  {
    name: 'postgres-unavailable-or-failover'
    severity: 1
    meaning: 'Ingest and the dashboard are down; content retrieval is down. Collection continues on-device.'
    query: 'AzureMetrics | where ResourceProvider == \'MICROSOFT.DBFORPOSTGRESQL\' | where MetricName in (\'connections_failed\',\'ha_state\') | summarize failures = sum(Total) by bin(TimeGenerated, 5m) | where failures > 0 | project failures'
    threshold: 0
    windowMinutes: 15
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'keyvault-unwrap-failure-rate'
    severity: 1
    meaning: 'Content retrieval is failing for every affected tenant; metadata and dashboards are unaffected.'
    query: 'AzureDiagnostics | where ResourceType == \'VAULTS\' | where OperationName has \'unwrapkey\' | summarize rate = countif(ResultSignature != \'Success\') * 100.0 / max(count(), 1) | project rate'
    threshold: 1
    windowMinutes: 10
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'customer-key-disabled-or-destroyed'
    severity: 1
    meaning: 'That tenant\'s stored content is now unretrievable, permanently if the key was destroyed. Metadata is unaffected (§12.5).'
    query: 'AzureDiagnostics | where ResourceType == \'VAULTS\' | where OperationName in (\'VaultDelete\',\'KeyDelete\',\'KeyPurge\',\'KeyDisable\') | where TimeGenerated > ago(5m) | project TimeGenerated, OperationName, id_s'
    threshold: 0
    windowMinutes: 5
    evaluationFrequencyMinutes: 1
  }
  {
    name: 'aggregate-freshness-sev2'
    severity: 2
    meaning: 'The dashboard under-reports by up to that window. Stated in the UI, not just in the alert.'
    query: 'customMetrics | where name == \'aggregate_oldest_unrecomputed_bucket_minutes\' | summarize oldest = max(value) | where oldest > 15 | project oldest'
    threshold: 15
    windowMinutes: 30
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'aggregate-freshness-sev1'
    severity: 1
    meaning: 'The dashboard is materially wrong; the event-visible SLO is breaching.'
    query: 'customMetrics | where name == \'aggregate_oldest_unrecomputed_bucket_minutes\' | summarize oldest = max(value) | where oldest > 60 | project oldest'
    threshold: 60
    windowMinutes: 30
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'coverage-collapse-one-provider'
    severity: 2
    meaning: 'A usage mode has stopped being collected. If it is the egress proxy, the customer’s network path may also be affected.'
    query: 'customMetrics | where name == \'provider_reporting_share\' | summarize share = min(value) by provider = tostring(customDimensions.provider) | where share < 0.8 | project provider, share'
    threshold: json('0.8')
    windowMinutes: 30
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'coverage-collapse-one-tenant'
    severity: 2
    meaning: 'Likely a broken policy deploy or a mass uninstall at that customer.'
    query: 'customMetrics | where name == \'tenant_provider_share\' | summarize share = avg(value) by tenant = tostring(customDimensions.tenant_id) | where share < 0.5 | project tenant, share'
    threshold: json('0.5')
    windowMinutes: 30
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'spool-saturation-fleet'
    severity: 2
    meaning: 'Events are about to be dropped and counted. Something upstream has been unavailable longer than the spool can cover.'
    query: 'customMetrics | where name == \'spool_utilisation\' | summarize saturated = countif(value > 0.8) * 100.0 / count() | where saturated > 5 | project saturated'
    threshold: 5
    windowMinutes: 30
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'devices-not-reporting'
    severity: 2
    meaning: 'The customer is paying for coverage they are not getting; possibly a failed rollout.'
    query: 'customMetrics | where name == \'device_silent_share\' | summarize silent = max(value) | where silent > 0.1 | project silent'
    threshold: json('0.1')
    windowMinutes: 1440
    evaluationFrequencyMinutes: 60
  }
  {
    name: 'degraded-classifier-share'
    severity: 2
    meaning: 'Labels are rules-only, so the numbers understate sensitive-data traffic. Explicitly not "no sensitive data found".'
    query: 'customMetrics | where name == \'degraded_classifier_share\' | summarize share = max(value) | where share > 0.05 | project share'
    threshold: json('0.05')
    windowMinutes: 60
    evaluationFrequencyMinutes: 10
  }
  {
    name: 'dedup-reconciliation-drift'
    severity: 2
    meaning: 'Counts a customer could challenge. Investigate before the customer does.'
    query: 'customMetrics | where name == \'dedup_reconciliation_drift_share\' | summarize drift = max(value) | where drift > 0.005 | project drift'
    threshold: json('0.005')
    windowMinutes: 60
    evaluationFrequencyMinutes: 15
  }
  {
    name: 'certificate-expiry'
    severity: 1
    meaning: 'On the expiry date the vendor cannot ship a fix to the endpoint (§7.4).'
    query: 'customMetrics | where name == \'signing_certificate_days_remaining\' | summarize days = min(value) | where days < 14 | project days'
    threshold: 14
    windowMinutes: 60
    evaluationFrequencyMinutes: 60
  }
  {
    name: 'client-version-below-minimum'
    severity: 3
    meaning: 'That device’s data may be incomplete after the compatibility window closes.'
    query: 'customMetrics | where name == \'devices_below_minimum_version\' | summarize devices = max(value) | where devices > 0 | project devices'
    threshold: 0
    windowMinutes: 1440
    evaluationFrequencyMinutes: 60
  }
  {
    name: 'waf-false-positive'
    severity: 2
    meaning: 'A customer’s devices are being blocked from the product’s own edge.'
    query: 'AzureDiagnostics | where ResourceType == \'FRONTDOORWEBAPPLICATIONFIREWALLPOLICIES\' | where action_s == \'Block\' | summarize blocked = count() by bin(TimeGenerated, 5m) | where blocked > 100 | project blocked'
    threshold: 100
    windowMinutes: 15
    evaluationFrequencyMinutes: 5
  }
  {
    name: 'cost-anomaly'
    severity: 3
    meaning: 'The attachment tier is the only unbounded line (brief §3.1); a spike is usually a retention or budget misconfiguration (§11.6).'
    query: 'Usage | where TimeGenerated > ago(1d) | summarize cost = sum(Quantity * Price) by bin(TimeGenerated, 1d) | where cost > 1.5 * 100 | project cost'
    threshold: 0
    windowMinutes: 1440
    evaluationFrequencyMinutes: 60
  }
  {
    name: 'revoked-credential-storm'
    severity: 2
    meaning: 'Either an incident response at a customer or a defect; both need a human.'
    query: 'customMetrics | where name == \'device_revocations_per_hour\' | summarize revocations = max(value) | where revocations > 50 | project revocations'
    threshold: 50
    windowMinutes: 60
    evaluationFrequencyMinutes: 10
  }
]

@description('Tags applied to every resource.')
param tags object = {}

resource actionGroups 'Microsoft.Insights/actionGroups@2023-01-01' = [for severity in [1, 2, 3]: {
  name: '${baseName}-ag-sev${severity}'
  location: 'global'
  tags: tags
  properties: {
    groupShortName: 'sac${severity}'
    enabled: true
    emailReceivers: [for (address, i) in emailReceivers['sev${severity}'] ?? []: {
      name: 'email-${i}'
      emailAddress: address
      useCommonAlertSchema: true
    }]
    webhookReceivers: [for (url, i) in webhookReceivers['sev${severity}'] ?? []: {
      name: 'webhook-${i}'
      serviceUri: url
      useCommonAlertSchema: true
    }]
  }
}]

// Every alert is a scheduled query rule against the shared workspace. The queries are parameters so
// the set can be reviewed as data, and `severity` selects the action group — which is what makes
// "one action group per severity" true rather than aspirational.
resource scheduledQueryAlerts 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = [for (alert, i) in alerts: {
  name: '${baseName}-alert-${alert.name}'
  location: location
  tags: tags
  properties: {
    displayName: alert.name
    description: alert.meaning
    severity: alert.severity
    enabled: true
    evaluationFrequency: 'PT${alert.evaluationFrequencyMinutes}M'
    windowSize: 'PT${alert.windowMinutes}M'
    scopes: [
      logAnalyticsWorkspaceId
    ]
    targetResourceTypes: [
      'Microsoft.OperationalInsights/workspaces'
    ]
    criteria: {
      allOf: [
        {
          query: alert.query
          timeAggregation: 'Maximum'
          operator: 'GreaterThan'
          threshold: alert.threshold
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      // A scheduled query rule names its action groups by resource id; the object form is the
      // metric-alert shape and is not accepted here.
      actionGroups: [
        actionGroups[alert.severity - 1].id
      ]
      customProperties: {
        meaning: alert.meaning
      }
    }
  }
}]

@description('Resource ids of the action groups, keyed by severity name.')
output actionGroupIds object = {
  sev1: actionGroups[0].id
  sev2: actionGroups[1].id
  sev3: actionGroups[2].id
}

@description('How many §10.3 alerts are deployed. §10.3’s table has 18 rows; the composition asserts the count so a dropped alert is visible.')
output alertCount int = length(alerts)
