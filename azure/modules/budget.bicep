// budget.bicep — budgets and the cost-anomaly alert (docs/05-platform-delivery.md §2, §10.3, §11.6).
//
// The attachment tier is the only unbounded cost in the product (brief §3.1), and §11.6 names it as
// the single line with no natural bound. A budget is what turns that statement into a signal: a
// cost-anomaly alert is cheaper than a monthly surprise.

@description('Base name for the environment.')
param baseName string

@description('The budget amount in the subscription’s billing currency. §3.2 makes it per-environment: dev and staging are small, production scales with tenant count.')
@minValue(1)
param amount int

@description('Budget period. Monthly is the shape every figure in §11 is quoted in.')
@allowed(['Monthly', 'Quarterly', 'Annually'])
param timeGrain string = 'Monthly'

@description('Notification thresholds as percentages of the budget, e.g. [50, 80, 100]. Each becomes an actual-spend alert and a forecast alert. Whole percentages because Bicep has no fractional literal.')
param thresholds array = [50, 80, 100]

@description('Email recipients for budget notifications. Empty in dev, where a budget is a record rather than an alert.')
param contactEmails array = []

@description('Resource filter: the environment’s resource group id. Budgets scope to a resource group so one environment cannot hide behind another’s headroom.')
param resourceGroupFilter string

@description('Whether to alert when the forecast exceeds the budget, not only when actual spend does. On: by the time actual spend crosses, the month is already lost.')
param alertOnForecast bool = true

@description('First day of the budget period, yyyy-MM-01. Defaults to the current month at deployment: utcNow is allowed only as a parameter default, and a redeploy re-states the same budget.')
param startDate string = utcNow('yyyy-MM-01')

// No `tags` here: Microsoft.Consumption/budgets has no top-level tags property (the ARM schema
// exposes only eTag, name, properties and scope). Tags for cost allocation live on the resources the
// budget watches; the budget is scoped to the environment's resource group instead.
resource budget 'Microsoft.Consumption/budgets@2023-11-01' = {
  name: '${baseName}-budget'
  properties: {
    category: 'Cost'
    amount: amount
    timeGrain: timeGrain
    timePeriod: {
      startDate: startDate
      endDate: dateTimeAdd(startDate, 'P10Y')
    }
    filter: {
      dimensions: {
        name: 'ResourceGroupName'
        operator: 'In'
        values: [
          last(split(resourceGroupFilter, '/'))
        ]
      }
    }
    notifications: union(
      reduce(thresholds, {}, (acc, t) => union(acc, {
        'actual-${t}': {
          enabled: true
          operator: 'GreaterThan'
          threshold: t
          contactEmails: contactEmails
          contactRoles: []
          thresholdType: 'Actual'
        }
      })),
      alertOnForecast ? reduce(thresholds, {}, (acc, t) => union(acc, {
        'forecast-${t}': {
          enabled: true
          operator: 'GreaterThan'
          threshold: t
          contactEmails: contactEmails
          contactRoles: []
          thresholdType: 'Forecasted'
        }
      })) : {}
    )
  }
}

@description('The budget amount, for the runbook and the cost model.')
output amount int = amount
