// frontdoor.bicep — Front Door Premium, the endpoint, the Private Link origin groups and the routes
// (docs/05-platform-delivery.md §2, §2.1, §6).
//
// Front Door is the **browser and identity-provider** edge (ADR 0020 decision 1). The device /v1/*
// routes are on Application Gateway (modules/application-gateway.bicep). This module carries:
//
//   /analyst/*                                -> query-api (the original analyst route)
//   /scim/v2/*, /onboard/*, /.well-known/*    -> control-api: a customer IdP's SCIM client, the
//                                                vendor's one-time onboarding pages, and the product
//                                                token issuer's discovery document and JWKS
//   /* (when the dashboard is deployed)       -> the dashboard server: the pages, the sign-in and the
//                                                session, and its forwarding of /v1/*, /admin/v1/*
//                                                and a minted retrieval URL to the apps behind it
//
// Two things are deliberate:
//
//   - every origin is reached over **Private Link** into the internal Container Apps environment, so
//     the apps never accept a public connection (§2.1);
//   - there is **no route to the content vault**. A browser's minted retrieval URL is answered by the
//     dashboard server, which forwards that one path to the vault inside the environment; the vault
//     keeps internal ingress and no edge reaches it (C15, D7). The checker fails the build if the
//     vault's name appears in this file outside a comment.
//
// Each route is spelled out rather than generated from a list, so a reviewer can read the public
// surface here and the checker can match it.

@description('Front Door profile SKU. Premium is required for Private Link origins; the value is a parameter so the cost model and the composition state it once.')
@allowed(['Premium_AzureFrontDoor', 'Standard_AzureFrontDoor'])
param skuName string = 'Premium_AzureFrontDoor'

@description('Resource id of the WAF policy from waf.bicep. The security policy below associates it with every route.')
param wafPolicyId string

@description('Resource id of the internal Container Apps environment, reached as a Private Link origin. It is an origin, never a public hostname.')
param environmentId string

@description('Region of the Container Apps environment, which is where its private link service lives.')
param privateLinkLocation string

@description('Origin host headers, keyed by app name: query-api and control-api always, dashboard when the dashboard server is deployed. Backends validate the Host header, so the route decides which app answers.')
param originHostHeaders object

@description('Health probe path for the origin groups. Every routed app exposes it (§10.2).')
param healthProbePath string = '/healthz'

@description('Health probe interval in seconds. Short enough that a failed revision is removed before a flush arrives.')
@minValue(5)
@maxValue(255)
param healthProbeIntervalSeconds int = 30

@description('Whether the routes are enabled. A parameter rather than a code path so a region can be staged dark and enabled by a reviewed parameter change.')
param routesEnabled bool = true

@description('Tags applied to every resource.')
param tags object = {}

var routeDashboard = contains(originHostHeaders, 'dashboard')

// The apps Front Door has an origin for. The vault is not one of them, by construction.
var routedApps = concat(['query-api', 'control-api'], routeDashboard ? ['dashboard'] : [])

resource profile 'Microsoft.Cdn/profiles@2023-05-01' = {
  name: 'afd-profile'
  location: 'global'
  tags: tags
  sku: {
    name: skuName
  }
}

resource endpoint 'Microsoft.Cdn/profiles/afdEndpoints@2023-05-01' = {
  parent: profile
  name: 'analyst'
  location: 'global'
  tags: tags
  properties: {
    enabledState: routesEnabled ? 'Enabled' : 'Disabled'
  }
}

// One origin group per app, so each route names exactly one app and a probe failure on one app
// cannot take another's traffic away.
resource originGroups 'Microsoft.Cdn/profiles/originGroups@2023-05-01' = [for app in routedApps: {
  parent: profile
  name: app
  properties: {
    loadBalancingSettings: {
      sampleSize: 4
      successfulSamplesRequired: 3
      additionalLatencyInMilliseconds: 50
    }
    healthProbeSettings: {
      probePath: healthProbePath
      probeRequestType: 'GET'
      probeProtocol: 'Https'
      probeIntervalInSeconds: healthProbeIntervalSeconds
    }
    sessionAffinityState: 'Disabled'
  }
}]

// Premium is what makes a Private Link origin possible; with Standard, the origin would be a public
// hostname and §2.1's "no container app holds a public IP" would be false.
resource origins 'Microsoft.Cdn/profiles/originGroups/origins@2023-05-01' = [for (app, i) in routedApps: {
  parent: originGroups[i]
  name: '${app}-private-link'
  properties: {
    hostName: originHostHeaders[app]
    originHostHeader: originHostHeaders[app]
    httpPort: 80
    httpsPort: 443
    priority: 1
    weight: 1000
    enabledState: 'Enabled'
    sharedPrivateLinkResource: {
      privateLink: {
        id: environmentId
      }
      groupId: 'managedEnvironments'
      privateLinkLocation: privateLinkLocation
      requestMessage: 'Front Door private-link origin for the internal Container Apps environment.'
    }
  }
}]

// Analyst entry: the original query-api route.
resource analystRoute 'Microsoft.Cdn/profiles/afdEndpoints/routes@2023-05-01' = {
  parent: endpoint
  name: 'analyst'
  properties: {
    originGroup: {
      id: originGroups[0].id
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
    origins
  ]
}

// The identity service's public surface. SCIM is called by the customer's provisioning service
// (Entra, Okta, ...), not by a browser, so it is not behind the dashboard; each path authenticates
// itself (a SCIM bearer, the invite token, nothing secret for the issuer documents).
resource identityRoute 'Microsoft.Cdn/profiles/afdEndpoints/routes@2023-05-01' = {
  parent: endpoint
  name: 'identity'
  properties: {
    originGroup: {
      id: originGroups[1].id
    }
    supportedProtocols: [
      'Https'
    ]
    patternsToMatch: [
      '/scim/v2/*'
      '/onboard/*'
      '/.well-known/*'
    ]
    forwardingProtocol: 'HttpsOnly'
    linkToDefaultDomain: 'Enabled'
    httpsRedirect: 'Enabled'
    enabledState: routesEnabled ? 'Enabled' : 'Disabled'
  }
  dependsOn: [
    origins
  ]
}

// Everything else is the dashboard server. More specific patterns above win, so this route never
// takes SCIM, onboarding or the analyst path from their owners.
resource webRoute 'Microsoft.Cdn/profiles/afdEndpoints/routes@2023-05-01' = if (routeDashboard) {
  parent: endpoint
  name: 'web'
  properties: {
    originGroup: {
      id: originGroups[length(routedApps) - 1].id
    }
    supportedProtocols: [
      'Https'
    ]
    patternsToMatch: [
      '/*'
    ]
    forwardingProtocol: 'HttpsOnly'
    linkToDefaultDomain: 'Enabled'
    httpsRedirect: 'Enabled'
    enabledState: routesEnabled ? 'Enabled' : 'Disabled'
  }
  dependsOn: [
    origins
  ]
}

// WAF is associated with the endpoint through a security policy, so no route can exist without it.
resource securityPolicy 'Microsoft.Cdn/profiles/securityPolicies@2023-05-01' = {
  parent: profile
  name: 'waf'
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
    analystRoute
    identityRoute
  ]
}

@description('Resource id of the Front Door profile.')
output profileId string = profile.id

@description('The public hostname browsers and identity providers resolve. The device FQDN resolves to Application Gateway instead (ADR 0020).')
output endpointHostName string = endpoint.properties.hostName

@description('The query-api origin group id, so monitoring.bicep can alert on the analyst origin\'s health.')
output originGroupId string = originGroups[0].id
