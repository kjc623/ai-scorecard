// budget.bicep — a monthly cost budget on the environment's resource group, alerting on actual and
// forecast spend.

param baseName string

@minValue(1)
param amount int

param contactEmails array

@description('First day of the budget period; a redeploy restates the same budget.')
param startDate string = utcNow('yyyy-MM-01')

var thresholds = [50, 80, 100]

resource budget 'Microsoft.Consumption/budgets@2023-11-01' = {
  name: '${baseName}-budget'
  properties: {
    category: 'Cost'
    amount: amount
    timeGrain: 'Monthly'
    timePeriod: {
      startDate: startDate
      endDate: dateTimeAdd(startDate, 'P10Y')
    }
    filter: {
      dimensions: {
        name: 'ResourceGroupName'
        operator: 'In'
        values: [resourceGroup().name]
      }
    }
    notifications: union(
      reduce(thresholds, {}, (acc, t) => union(acc, {
        'actual-${t}': {
          enabled: true
          operator: 'GreaterThan'
          threshold: t
          contactEmails: contactEmails
          thresholdType: 'Actual'
        }
      })),
      reduce(thresholds, {}, (acc, t) => union(acc, {
        'forecast-${t}': {
          enabled: true
          operator: 'GreaterThan'
          threshold: t
          contactEmails: contactEmails
          thresholdType: 'Forecasted'
        }
      }))
    )
  }
}
