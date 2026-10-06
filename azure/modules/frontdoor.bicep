// frontdoor.bicep — the browser edge: Front Door Premium with its WAF, on the analyst custom domain.
//
// Every origin is a Private Link origin into the internal Container Apps environment (Premium is the
// tier that supports it), so no app has a public address. The dashboard answers everything except the
// identity paths, which control-api serves to browsers and to customers' identity providers.

param baseName string

@description('Custom domain browsers use, e.g. app.sac.example.com.')
param analystFqdn string

@description('The Container Apps environment, reached through its private link service.')
param environmentId string

@description('Region of the Container Apps environment.')
param environmentLocation string

@description('Origin hostnames: { dashboard, control } — the apps\' FQDNs.')
param originHosts object

param tags object = {}

var profileName = '${baseName}-afd'

resource waf 'Microsoft.Network/frontDoorWebApplicationFirewallPolicies@2024-02-01' = {
  name: '${replace(baseName, '-', '')}afdwaf'
  location: 'global'
  tags: tags
  sku: { name: 'Premium_AzureFrontDoor' }
  properties: {
    policySettings: {
      enabledState: 'Enabled'
      mode: 'Prevention'
      requestBodyCheck: 'Enabled'
    }
    customRules: {
      rules: [
        {
          name: 'RateLimitPerClient'
          priority: 100
          ruleType: 'RateLimitRule'
          rateLimitDurationInMinutes: 1
          rateLimitThreshold: 2000
          action: 'Block'
          matchConditions: [
            {
              matchVariable: 'RemoteAddr'
              operator: 'IPMatch'
              matchValue: ['0.0.0.0/0', '::/0']
            }
          ]
        }
      ]
    }
    managedRules: {
      managedRuleSets: [
        {
          ruleSetType: 'Microsoft_DefaultRuleSet'
          ruleSetVersion: '2.1'
          ruleSetAction: 'Block'
        }
        {
          ruleSetType: 'Microsoft_BotManagerRuleSet'
          ruleSetVersion: '1.1'
        }
      ]
    }
  }
}

resource profile 'Microsoft.Cdn/profiles@2024-02-01' = {
  name: profileName
  location: 'global'
  tags: tags
  sku: { name: 'Premium_AzureFrontDoor' }
}

resource endpoint 'Microsoft.Cdn/profiles/afdEndpoints@2024-02-01' = {
  parent: profile
  name: baseName
  location: 'global'
  tags: tags
  properties: { enabledState: 'Enabled' }
}

resource domain 'Microsoft.Cdn/profiles/customDomains@2024-02-01' = {
  parent: profile
  name: replace(analystFqdn, '.', '-')
  properties: {
    hostName: analystFqdn
    tlsSettings: {
      certificateType: 'ManagedCertificate'
      minimumTlsVersion: 'TLS12'
    }
  }
}

resource originGroups 'Microsoft.Cdn/profiles/originGroups@2024-02-01' = [for app in ['dashboard', 'control']: {
  parent: profile
  name: app
  properties: {
    loadBalancingSettings: {
      sampleSize: 4
      successfulSamplesRequired: 3
    }
    healthProbeSettings: {
      probePath: '/healthz'
      probeRequestType: 'GET'
      probeProtocol: 'Https'
      probeIntervalInSeconds: 30
    }
  }
}]

resource origins 'Microsoft.Cdn/profiles/originGroups/origins@2024-02-01' = [for (app, i) in ['dashboard', 'control']: {
  parent: originGroups[i]
  name: app
  properties: {
    hostName: originHosts[app]
    originHostHeader: originHosts[app]
    httpsPort: 443
    priority: 1
    weight: 1000
    enforceCertificateNameCheck: true
    sharedPrivateLinkResource: {
      privateLink: { id: environmentId }
      groupId: 'managedEnvironments'
      privateLinkLocation: environmentLocation
      requestMessage: 'Front Door origin for ${baseName}'
    }
  }
}]

var routes = [
  { name: 'identity', group: 1, patterns: ['/scim/v2/*', '/onboard/*', '/.well-known/*'] }
  { name: 'dashboard', group: 0, patterns: ['/*'] }
]

resource routeResources 'Microsoft.Cdn/profiles/afdEndpoints/routes@2024-02-01' = [for r in routes: {
  parent: endpoint
  name: r.name
  properties: {
    originGroup: { id: originGroups[r.group].id }
    customDomains: [{ id: domain.id }]
    supportedProtocols: ['Http', 'Https']
    patternsToMatch: r.patterns
    forwardingProtocol: 'HttpsOnly'
    httpsRedirect: 'Enabled'
    linkToDefaultDomain: 'Disabled'
  }
  dependsOn: [origins]
}]

resource securityPolicy 'Microsoft.Cdn/profiles/securityPolicies@2024-02-01' = {
  parent: profile
  name: 'waf'
  properties: {
    parameters: {
      type: 'WebApplicationFirewall'
      wafPolicy: { id: waf.id }
      associations: [
        {
          domains: [{ id: domain.id }]
          patternsToMatch: ['/*']
        }
      ]
    }
  }
}

output endpointHostName string = endpoint.properties.hostName
output domainValidationToken string = domain.properties.validationProperties.validationToken
output profileId string = profile.id
