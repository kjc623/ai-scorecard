// waf.bicep — the WAF policy attached to Front Door (docs/05-platform-delivery.md §2, §10.3).
//
// WAF must block at the edge, not in the app, because ingest-api accepts traffic from machines the
// vendor does not control (§2 inventory). The mode is a parameter (Detection in dev, Prevention in
// staging and production) and the module carries the §10.3 WAF-false-positive signal as an
// explicit custom rule exclusion path rather than as documentation.

@description('WAF policy mode. Detection logs; Prevention blocks. §3.2 makes this environment-parameterised, and the checker asserts Prevention is the production value.')
@allowed(['Detection', 'Prevention'])
param wafMode string = 'Prevention'

@description('Managed rule set version. Pinned so a rule-set update is a reviewed change rather than a silent behaviour change at the edge.')
param managedRuleSetVersion string = '2.1'

@description('Managed rule set type: Microsoft_DefaultRuleSet for OWASP-style coverage, Microsoft_BotManagerRuleSet for bot filtering. Both are enabled.')
param managedRuleSetType string = 'Microsoft_DefaultRuleSet'

@description('Requests per minute per client IP before the rate-limit rule applies. Sized for a fleet flush rather than for a browser: 5,000 devices behind a customer NAT must not be throttled into a coverage gap.')
@minValue(100)
@maxValue(1000000)
param rateLimitPerMinute int = 20000

@description('Custom rule exclusions: [{ matchVariable, operator, selector, ruleSet, ruleGroup }]. Every entry is a false-positive fix and must name the alert it closes (§10.3).')
param exclusions array = []

@description('Whether to enable bot protection. On in production; the customer’s devices are not browsers and must not be classified as bots, which is why the exclusion list exists.')
param enableBotProtection bool = true

@description('Tags applied to the policy.')
param tags object = {}

resource wafPolicy 'Microsoft.Network/frontDoorWebApplicationFirewallPolicies@2024-02-01' = {
  name: 'waf-policy'
  location: 'global'
  tags: tags
  sku: {
    name: 'Premium_AzureFrontDoor'
  }
  properties: {
    policySettings: {
      enabledState: 'Enabled'
      mode: wafMode
      requestBodyCheck: 'Enabled'
      requestBodyInspectLimitInKB: 128 // prompt bodies are inspected for shape, not retained; the limit bounds edge work
      customBlockResponseStatusCode: 403
      customBlockResponseBody: null
      javascriptChallengeExpirationInMinutes: 30
    }
    managedRules: {
      managedRuleSets: concat([
        {
          ruleSetType: managedRuleSetType
          ruleSetVersion: managedRuleSetVersion
          ruleSetAction: 'Block'
          ruleGroupOverrides: []
        }
      ], enableBotProtection ? [
        {
          ruleSetType: 'Microsoft_BotManagerRuleSet'
          ruleSetVersion: '1.1'
          ruleSetAction: 'Block'
          ruleGroupOverrides: []
        }
      ] : [])
    }
    customRules: {
      rules: [
        {
          name: 'rate-limit-per-client'
          priority: 100
          ruleType: 'RateLimitRule'
          rateLimitDurationInMinutes: 1
          rateLimitThreshold: rateLimitPerMinute
          matchConditions: [
            {
              matchVariable: 'RemoteAddr'
              operator: 'IPMatch'
              negateCondition: false
              matchValue: [
                '0.0.0.0/0'
              ]
              transforms: []
            }
          ]
          action: wafMode == 'Prevention' ? 'Block' : 'Log'
        }
        {
          name: 'api-path-allow'
          priority: 200
          ruleType: 'MatchRule'
          matchConditions: [
            {
              matchVariable: 'RequestUri'
              operator: 'BeginsWith'
              negateCondition: false
              matchValue: [
                '/v1/'
              ]
              transforms: [
                'Lowercase'
              ]
            }
          ]
          action: 'Allow'
        }
        {
          name: 'diagnostics-path-allow'
          priority: 210
          ruleType: 'MatchRule'
          matchConditions: [
            {
              matchVariable: 'RequestUri'
              operator: 'BeginsWith'
              negateCondition: false
              matchValue: [
                '/healthz'
                '/readyz'
              ]
              transforms: [
                'Lowercase'
              ]
            }
          ]
          action: 'Allow'
        }
      ]
    }
  }
}

// False-positive exclusions live in their own child resource so a reviewer sees each one with the
// alert it closes, rather than as an opaque list inside the policy.
resource exclusionRules 'Microsoft.Network/frontDoorWebApplicationFirewallPolicies@2024-02-01' = [for (ex, i) in exclusions: {
  name: 'waf-policy'
  location: 'global'
  tags: tags
  sku: {
    name: 'Premium_AzureFrontDoor'
  }
  properties: {
    policySettings: {
      enabledState: 'Enabled'
      mode: wafMode
    }
    managedRules: {
      managedRuleSets: [
        {
          ruleSetType: managedRuleSetType
          ruleSetVersion: managedRuleSetVersion
          ruleGroupOverrides: [
            {
              ruleGroupName: ex.ruleGroup
              rules: [
                {
                  ruleId: ex.ruleId
                  enabledState: 'Disabled'
                  action: 'Log'
                }
              ]
            }
          ]
        }
      ]
    }
  }
  dependsOn: [
    wafPolicy
  ]
}]

@description('Resource id of the WAF policy, for the Front Door security policy association.')
output wafPolicyId string = wafPolicy.id

@description('The mode applied, echoed so the composition and §14’s go-live gate agree that production is in Prevention.')
output mode string = wafMode
