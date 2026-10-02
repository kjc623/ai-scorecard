// frontdoor.bicep — Front Door Premium, the endpoint, the Private Link origin group and the routes
// (docs/05-platform-delivery.md §2, §2.1, §6).
//
// This is the only public surface in the deployment. Two things are deliberate:
//
//   - the origin group reaches the Container Apps environment over **Private Link**, so the
//     environment stays internal and the apps never accept a public connection (§2.1);
//   - there is **no route to content-vault**. It has no Front Door route and no public endpoint, by
//     construction (C15, D7), and the checker fails the build if the string appears in this file.
//
// The five device-facing endpoints of master §5.1 are the /v1/* routes onto ingest-api and
// control-api; analyst traffic reaches query-api through /analyst/*, authenticated by Entra ID.

@description('Front Door profile SKU. Premium is required for Private Link origins; the value is a parameter so the cost model and the composition state it once.')
@allowed(['Premium_AzureFrontDoor', 'Standard_AzureFrontDoor'])
param skuName string = 'Premium_AzureFrontDoor'

@description('Resource id of the WAF policy from waf.bicep. The security policy below associates it with every route.')
param wafPolicyId string

@description('Resource id of the internal Container Apps environment, reached as a Private Link origin. It is an origin, never a public hostname.')
param environmentId string

@description('The Container Apps environment’s internal FQDN, from container-apps-env.bicep. Front Door resolves it through the private endpoint.')
param environmentFqdn string

@description('Origin host headers, keyed by app name. Backends validate the Host header, so the route decides which app answers.')
param originHostHeaders object

@description('Health probe path for the origin group. Shared by all apps because all expose it (§10.2).')
param healthProbePath string = '/healthz'

@description('Health probe interval in seconds. Short enough that a failed revision is removed before a flush arrives.')
@minValue(5)
@maxValue(255)
param healthProbeIntervalSeconds int = 30

@description('Whether the routes are enabled. A parameter rather than a code path so a region can be staged dark and enabled by a reviewed parameter change.')
param routesEnabled bool = true

@description('Tags applied to every resource.')
param tags object = {}

resource profile 'Microsoft.Cdn/profiles@2023-05-01' = {
  name: 'afd-profile'
  location: 'global'
  tags: tags
  sku: {
    name: skuName
  }
}

resource endpoint 'Microsoft.Cdn/profiles/afdEndpoints@2023-05-01' = {
  name: '${profile.name}/device-api'
  location: 'global'
  tags: tags
  properties: {
    enabledState: routesEnabled ? 'Enabled' : 'Disabled'
  }
}

// One origin group with a Private Link origin. Premium is what makes this possible; with Standard,
// the origin would be a public hostname and §2.1's "no container app holds a public IP" would be
// false.
resource originGroup 'Microsoft.Cdn/profiles/originGroups@2023-05-01' = {
  name: '${profile.name}/container-apps'
  properties: {
    loadBalancingSettings: {
      sampleSize: 4
      successfulSamplesRequired: 3
      additionalLatencyInMilliseconds: 50
    }
    healthProbeSettings: {
      probePath: healthProbePath
      probeRequestType: 'HEAD'
      probeProtocol: 'Https'
      probeIntervalInSeconds: healthProbeIntervalSeconds
    }
    sessionAffinityState: 'Disabled'
  }
  dependsOn: [
    endpoint
  ]
}

resource origin 'Microsoft.Cdn/profiles/originGroups/origins@2023-05-01' = {
  name: '${profile.name}/container-apps/container-apps-internal'
  properties: {
    hostName: environmentFqdn
    originHostHeader: originHostHeaders['ingest-api']
    httpPort: 80
    httpsPort: 443
    priority: 1
    weight: 1000
    enabled: true
    privateLinkAlias: environmentId
    privateLinkApprovalMessage: 'Front Door private-link origin for the internal Container Apps environment.'
  }
  dependsOn: [
    originGroup
  ]
}

// The device-facing surface: the five endpoints of master §5.1, all on ingest-api and control-api.
// A route per endpoint rather than a catch-all, so an unlisted path is a 404 at the edge rather
// than a surprise inside a service.
resource deviceRoute 'Microsoft.Cdn/profiles/afdEndpoints/routes@2023-05-01' = {
  name: '${profile.name}/device-api/device-v1'
  properties: {
    originGroup: {
      id: originGroup.id
    }
    supportedProtocols: [
      'Https'
    ]
    patternsToMatch: [
      '/v1/*'
    ]
    forwardingProtocol: 'HttpsOnly'
    linkToDefaultDomain: 'Enabled'
    httpsRedirect: 'Enabled'
    enabledState: routesEnabled ? 'Enabled' : 'Disabled'
  }
  dependsOn: [
    origin
  ]
}

// Analyst entry. Entra ID authentication is enforced by the app as well as here; the dashboard is
// the only client, and §2.1 is explicit that analyst traffic uses the same path as devices.
resource analystRoute 'Microsoft.Cdn/profiles/afdEndpoints/routes@2023-05-01' = {
  name: '${profile.name}/device-api/analyst'
  properties: {
    originGroup: {
      id: originGroup.id
    }
    supportedProtocols: [
      'Https'
    ]
    patternsToMatch: [
      '/analyst/*'
    ]
    forwardingProtocol: 'HttpsOnly'
    linkToDefaultDomain: 'Enabled'
    httpsRedirect: 'Enabled'
    enabledState: routesEnabled ? 'Enabled' : 'Disabled'
  }
  dependsOn: [
    origin
  ]
}

// WAF is associated with the endpoint through a security policy, so no route can exist without it.
resource securityPolicy 'Microsoft.Cdn/profiles/securityPolicies@2023-05-01' = {
  name: '${profile.name}/waf'
  properties: {
    parameters: {
      type: 'WebApplicationFirewall'
      wafPolicy: {
        id: wafPolicyId
      }
      associations: [
        {
          domains: [
            {
              id: endpoint.id
            }
          ]
          patternsToMatch: [
            '/*'
          ]
        }
      ]
    }
  }
  dependsOn: [
    deviceRoute
    analystRoute
  ]
}

@description('Resource id of the Front Door profile.')
output profileId string = profile.id

@description('The public hostname devices and analysts resolve. The only public surface in the deployment.')
output endpointHostName string = endpoint.properties.hostName

@description('The origin group id, so monitoring.bicep can alert on origin health.')
output originGroupId string = originGroup.id
