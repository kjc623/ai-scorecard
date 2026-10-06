// application-gateway.bicep — the device edge.
//
// Devices connect to deviceFqdn with mutual TLS. The listener requests a client certificate without
// validating it (passthrough) and forwards the presented leaf to the origin in X-Client-Cert; the
// origins verify it against the device CA. Only the device API is reachable: a WAF custom rule
// blocks every other path, and the path map's default sends anything unmatched to an empty pool.

param location string

param baseName string

param gatewaySubnetId string

@description('Public hostname devices connect to, e.g. device.sac.example.com.')
param deviceFqdn string

@description('Unversioned Key Vault secret id of the device TLS certificate, so renewals are picked up.')
param certificateKeyVaultUri string

@description('{ id } of the identity that reads the certificate from Key Vault.')
param identity object

@description('The Container Apps environment\'s internal static IP.')
param backendIp string

@description('Backend hostnames: { ingest, control } — the apps\' FQDNs, sent as Host and SNI.')
param backendHosts object

param zoneRedundant bool

param minCapacity int

param tags object = {}

var name = '${baseName}-agw'
var zones = zoneRedundant ? ['1', '2', '3'] : []
var ingestPaths = ['/v1/events']
var controlPaths = ['/v1/enrol', '/v1/policy', '/v1/health', '/v1/content/grant', '/v1/extension/*']
var contentPaths = ['/v1/content']
var allowedPrefixes = ['/v1/events', '/v1/enrol', '/v1/policy', '/v1/health', '/v1/content', '/v1/extension/']

var managedRules = {
  managedRuleSets: [
    {
      ruleSetType: 'OWASP'
      ruleSetVersion: '3.2'
    }
  ]
}

var allowListRule = {
  name: 'DeviceApiOnly'
  priority: 10
  ruleType: 'MatchRule'
  action: 'Block'
  matchConditions: [
    {
      matchVariables: [{ variableName: 'RequestUri' }]
      operator: 'BeginsWith'
      negationConditon: true
      matchValues: allowedPrefixes
      transforms: ['Lowercase']
    }
  ]
}

resource waf 'Microsoft.Network/ApplicationGatewayWebApplicationFirewallPolicies@2024-05-01' = {
  name: '${baseName}-agw-waf'
  location: location
  tags: tags
  properties: {
    policySettings: {
      state: 'Enabled'
      mode: 'Prevention'
      requestBodyCheck: true
      requestBodyEnforcement: false
      requestBodyInspectLimitInKB: 128
      maxRequestBodySizeInKb: 2000
    }
    customRules: [allowListRule]
    managedRules: managedRules
  }
}

// Uploaded prompt content is arbitrary user text and code, which the managed rules would read as
// attacks; its body is not inspected. Headers, URI and the allow-list still apply.
resource contentWaf 'Microsoft.Network/ApplicationGatewayWebApplicationFirewallPolicies@2024-05-01' = {
  name: '${baseName}-agw-waf-content'
  location: location
  tags: tags
  properties: {
    policySettings: {
      state: 'Enabled'
      mode: 'Prevention'
      requestBodyCheck: false
    }
    customRules: [allowListRule]
    managedRules: managedRules
  }
}

resource pip 'Microsoft.Network/publicIPAddresses@2024-05-01' = {
  name: '${name}-pip'
  location: location
  tags: tags
  sku: { name: 'Standard' }
  zones: zones
  properties: {
    publicIPAllocationMethod: 'Static'
  }
}

var id = resourceId('Microsoft.Network/applicationGateways', name)

resource gateway 'Microsoft.Network/applicationGateways@2025-03-01' = {
  name: name
  location: location
  tags: tags
  zones: zones
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  properties: {
    sku: { name: 'WAF_v2', tier: 'WAF_v2' }
    autoscaleConfiguration: {
      minCapacity: minCapacity
      maxCapacity: 10
    }
    firewallPolicy: { id: waf.id }
    sslPolicy: {
      policyType: 'Predefined'
      policyName: 'AppGwSslPolicy20220101S'
    }
    gatewayIPConfigurations: [
      { name: 'gateway', properties: { subnet: { id: gatewaySubnetId } } }
    ]
    frontendIPConfigurations: [
      { name: 'public', properties: { publicIPAddress: { id: pip.id } } }
    ]
    frontendPorts: [
      { name: 'https', properties: { port: 443 } }
    ]
    sslCertificates: [
      { name: 'device', properties: { keyVaultSecretId: certificateKeyVaultUri } }
    ]
    sslProfiles: [
      {
        name: 'device-client-cert'
        properties: {
          clientAuthConfiguration: {
            verifyClientAuthMode: 'Passthrough'
          }
        }
      }
    ]
    httpListeners: [
      {
        name: 'device'
        properties: {
          frontendIPConfiguration: { id: '${id}/frontendIPConfigurations/public' }
          frontendPort: { id: '${id}/frontendPorts/https' }
          protocol: 'Https'
          sslCertificate: { id: '${id}/sslCertificates/device' }
          sslProfile: { id: '${id}/sslProfiles/device-client-cert' }
          hostName: deviceFqdn
          requireServerNameIndication: true
        }
      }
    ]
    backendAddressPools: [
      { name: 'apps', properties: { backendAddresses: [{ ipAddress: backendIp }] } }
      { name: 'unrouted', properties: { backendAddresses: [] } }
    ]
    probes: [for app in ['ingest', 'control']: {
      name: app
      properties: {
        protocol: 'Https'
        path: '/healthz'
        interval: 30
        timeout: 30
        unhealthyThreshold: 3
        pickHostNameFromBackendHttpSettings: true
        match: { statusCodes: ['200-399'] }
      }
    }]
    backendHttpSettingsCollection: [for app in ['ingest', 'control']: {
      name: app
      properties: {
        port: 443
        protocol: 'Https'
        cookieBasedAffinity: 'Disabled'
        hostName: backendHosts[app]
        pickHostNameFromBackendAddress: false
        requestTimeout: 60
        probe: { id: '${id}/probes/${app}' }
      }
    }]
    rewriteRuleSets: [
      {
        name: 'device'
        properties: {
          rewriteRules: [
            {
              name: 'forward-client-certificate'
              ruleSequence: 100
              actionSet: {
                requestHeaderConfigurations: [
                  { headerName: 'X-Client-Cert', headerValue: '{var_client_certificate}' }
                  { headerName: 'X-Forwarded-Proto', headerValue: 'https' }
                  { headerName: 'X-Forwarded-Host', headerValue: '{var_host}' }
                ]
              }
            }
          ]
        }
      }
    ]
    urlPathMaps: [
      {
        name: 'device'
        properties: {
          defaultBackendAddressPool: { id: '${id}/backendAddressPools/unrouted' }
          defaultBackendHttpSettings: { id: '${id}/backendHttpSettingsCollection/control' }
          defaultRewriteRuleSet: { id: '${id}/rewriteRuleSets/device' }
          pathRules: [
            {
              name: 'ingest'
              properties: {
                paths: ingestPaths
                backendAddressPool: { id: '${id}/backendAddressPools/apps' }
                backendHttpSettings: { id: '${id}/backendHttpSettingsCollection/ingest' }
                rewriteRuleSet: { id: '${id}/rewriteRuleSets/device' }
              }
            }
            {
              name: 'control'
              properties: {
                paths: controlPaths
                backendAddressPool: { id: '${id}/backendAddressPools/apps' }
                backendHttpSettings: { id: '${id}/backendHttpSettingsCollection/control' }
                rewriteRuleSet: { id: '${id}/rewriteRuleSets/device' }
              }
            }
            {
              name: 'content'
              properties: {
                paths: contentPaths
                backendAddressPool: { id: '${id}/backendAddressPools/apps' }
                backendHttpSettings: { id: '${id}/backendHttpSettingsCollection/control' }
                rewriteRuleSet: { id: '${id}/rewriteRuleSets/device' }
                firewallPolicy: { id: contentWaf.id }
              }
            }
          ]
        }
      }
    ]
    requestRoutingRules: [
      {
        name: 'device'
        properties: {
          ruleType: 'PathBasedRouting'
          priority: 100
          httpListener: { id: '${id}/httpListeners/device' }
          urlPathMap: { id: '${id}/urlPathMaps/device' }
        }
      }
    ]
  }
}

output publicIp string = pip.properties.ipAddress
