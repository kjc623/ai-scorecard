// application-gateway.bicep — Azure Application Gateway WAF_v2, the public device ingress
// (docs/05-platform-delivery.md §2, §2.1, §3.3, ADR 0020 decision 1).
//
// The device FQDN resolves here; Front Door remains the analyst edge. This module owns the whole
// "Application Gateway" resource family: the gateway, its public IP and its WAF policy.
//
// NOT VERIFIED: this module has never been compiled or deployed. There is no az CLI, no Bicep
// compiler and no subscription on the machine it was written on, so resource property names, the
// exact client-certificate server-variable spelling and the Key Vault certificate reference are
// design claims, not observations. The static checker proves the properties are *declared*; only a
// deployment proves they are *effective* (see azure/README.md).
//
// The device path it implements is the same contract localdev/edge/main.go proves end to end, and
// this module is kept behaviourally aligned with it (ADR 0020 decision 5):
//
//   - the listener terminates TLS and requests a client certificate WITHOUT requiring one: the
//     sslProfiles[].clientAuthConfiguration below is { verifyClientAuthMode: 'Passthrough',
//     verifyClientCertIssuerDN: false, verifyClientRevocation: 'None' }, the "passthrough" mode that
//     asks for a cert and accepts a request that omits one (edge: tls.RequestClientCert). The edge is
//     a filter; the origin is the authority (ADR 0019's intent, kept by ADR 0020 decision 2).
//   - a presented client certificate is forwarded to the origin as PEM in X-Client-Cert via a rewrite
//     from the {var_client_certificate} server variable (edge: peerCertPEM + url.QueryEscape).
//   - X-Forwarded-Proto is forced to https and X-Forwarded-Host to the host the device called, so the
//     origin's DPoP htu reconstruction matches the URL the device signed (edge: newProxy's Rewrite).
//   - the backend pool is the internal Container Apps environment static IP, and the Host header is
//     the per-app override that decides which app answers (edge: the route table below).
//
// Route table (mirrors localdev/edge/main.go's `routes`): /v1/events is ingest-api's; every other
// device-facing prefix (/v1/enrol, /v1/token, /v1/policy, /v1/health, /v1/content/grant) is
// control-api's. The path map below makes control-api the default and sends /v1/events to ingest-api.
//
// ASSUMPTION (verify on a real subscription): the {var_client_certificate} server variable returns
// the client certificate chain as PEM, URL-encoded for a single-line header. This is the form the
// Azure documentation describes and the one localdev/edge reproduces with url.QueryEscape. The
// origin does not depend on it: ingestion/ingest-api/internal/auth.parseCertificateChain accepts raw
// PEM or percent-encoded PEM via url.QueryUnescape, so whichever exact encoding the gateway produces
// is handled by the origin. The encoding is documented here as an assumption, not a verified fact.

// ---------------------------------------------------------------------------------------------
// Parameters

@description('Azure region for the gateway and its WAF policy. Environment-parameterised (§3.2).')
param location string

@description('Base name for the environment; the gateway name is derived from it, never typed per resource.')
param baseName string

@description('Resource id of the dedicated gateway subnet from network.bicep. Application Gateway requires its own subnet, which no other resource shares.')
param gatewaySubnetId string

@description('The public hostname devices resolve to this gateway, e.g. device.sac.example.com. It is the SNI/host of the HTTPS listener.')
param deviceFqdn string

@description('Key Vault secret id of the device FQDN TLS certificate (a certificate stored as a secret). The gateway reads it with its managed identity (§5.3: TLS certificates live in Key Vault).')
param sslCertificateKeyVaultSecretId string

@description('Resource id of the user-assigned managed identity the gateway runs as, for reading its TLS certificate from Key Vault. §5.1: the grant is the security boundary.')
param gatewayIdentityId string

@description('The Container Apps environment internal static IP (container-apps-env.bicep staticIp). The only backend address: the environment LB routes by Host header.')
param backendStaticIp string

@description('Per-app backend Host headers, { ingest, control }. Each is a container app FQDN; the environment LB uses the Host header to select the app.')
param backendHostNames object

@description('WAF policy mode. Detection logs; Prevention blocks. §3.2 makes this environment-parameterised and reuses the composition’s wafMode.')
@allowed(['Detection', 'Prevention'])
param wafMode string = 'Prevention'

@description('Gateway SKU name. WAF_v2 is the device edge (managed rule set in Prevention); Standard_v2 is the same gateway without WAF, kept as an option.')
@allowed(['WAF_v2', 'Standard_v2'])
param skuName string = 'WAF_v2'

@description('Gateway SKU tier; must match skuName.')
@allowed(['WAF_v2', 'Standard_v2'])
param tier string = 'WAF_v2'

@description('Managed rule set version for the WAF policy. Pinned so a rule-set update is a reviewed change rather than a silent edge behaviour change.')
param managedRuleSetVersion string = '3.2'

@description('Public IP SKU. Application Gateway requires a Standard public IP; it is a parameter so the value is reviewable rather than a hard-coded choice.')
@allowed(['Standard'])
param publicIpSkuName string = 'Standard'

@description('Tags applied to every resource. Cost allocation and drift reporting scope by tag.')
param tags object = {}

var gatewayName = '${baseName}-agw'

// ---------------------------------------------------------------------------------------------
// Resources

resource pip 'Microsoft.Network/publicIPAddresses@2023-09-01' = {
  name: '${baseName}-agw-pip'
  location: location
  tags: tags
  sku: {
    name: publicIpSkuName
  }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv4'
  }
  zones: []
}

// WAF policy: managed rule set in the composition's mode, attached to the gateway below. The
// device edge accepts traffic from machines the vendor does not control, so WAF must block at the
// edge, not in the app (docs/05 §2).
resource wafPolicy 'Microsoft.Network/ApplicationGatewayWebApplicationFirewallPolicies@2023-09-01' = {
  name: '${baseName}-agw-waf'
  location: location
  tags: tags
  properties: {
    policySettings: {
      state: 'Enabled'
      mode: wafMode
      requestBodyCheck: true
      requestBodyInspectLimitInKB: 128 // prompt bodies are inspected for shape, not retained
      maxRequestBodySizeInKb: 128
    }
    managedRules: {
      managedRuleSets: [
        {
          ruleSetType: 'OWASP'
          ruleSetVersion: managedRuleSetVersion
        }
      ]
    }
  }
}

resource gateway 'Microsoft.Network/applicationGateways@2023-09-01' = {
  name: gatewayName
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${gatewayIdentityId}': {}
    }
  }
  properties: {
    sku: {
      name: skuName
      tier: tier
    }
    gatewayIPConfigurations: [
      {
        name: 'appGatewayIpConfig'
        properties: {
          subnet: {
            id: gatewaySubnetId
          }
        }
      }
    ]
    frontendIPConfigurations: [
      {
        name: 'appGatewayFrontendIp'
        properties: {
          publicIPAddress: {
            id: pip.id
          }
        }
      }
    ]
    frontendPorts: [
      {
        name: 'port_443'
        properties: {
          port: 443
        }
      }
    ]
    // The device TLS certificate, referenced from Key Vault so no private key material exists in
    // the repository. The gateway reads it as its managed identity.
    sslCertificates: [
      {
        name: 'device-tls-cert'
        properties: {
          keyVaultSecretId: sslCertificateKeyVaultSecretId
        }
      }
    ]
    // Passthrough client authentication: request a certificate, accept a request that omits one,
    // and never verify it here. The origin is the authority (ADR 0020 decision 2).
    sslProfiles: [
      {
        name: 'device-passthrough'
        properties: {
          clientAuthConfiguration: {
            verifyClientAuthMode: 'Passthrough'
            verifyClientCertIssuerDN: false
            verifyClientRevocation: 'None'
          }
        }
      }
    ]
    backendAddressPools: [
      {
        name: 'container-apps'
        properties: {
          backendAddresses: [
            {
              ipAddress: backendStaticIp
            }
          ]
        }
      }
    ]
    // Per-app Host header: the environment LB selects the app from the Host header, so the two
    // backend HTTP settings carry the two app FQDNs. Protocol is Https to the environment's internal
    // LB; the environment certificate must be trusted on a real deployment (see the note below).
    backendHttpSettingsCollection: [
      {
        name: 'ingest-api-http'
        properties: {
          port: 443
          protocol: 'Https'
          cookieBasedAffinity: 'Disabled'
          pickHostNameFromBackendAddress: false
          hostName: backendHostNames.ingest
          requestTimeout: 30
          probe: {
            id: resourceId('Microsoft.Network/applicationGateways/probes', gatewayName, 'ingest-api-probe')
          }
        }
      }
      {
        name: 'control-api-http'
        properties: {
          port: 443
          protocol: 'Https'
          cookieBasedAffinity: 'Disabled'
          pickHostNameFromBackendAddress: false
          hostName: backendHostNames.control
          requestTimeout: 30
          probe: {
            id: resourceId('Microsoft.Network/applicationGateways/probes', gatewayName, 'control-api-probe')
          }
        }
      }
    ]
    probes: [
      {
        name: 'ingest-api-probe'
        properties: {
          protocol: 'Https'
          path: '/healthz'
          interval: 30
          timeout: 30
          unhealthyThreshold: 3
          pickHostNameFromBackendHttpSettings: true
        }
      }
      {
        name: 'control-api-probe'
        properties: {
          protocol: 'Https'
          path: '/healthz'
          interval: 30
          timeout: 30
          unhealthyThreshold: 3
          pickHostNameFromBackendHttpSettings: true
        }
      }
    ]
    httpListeners: [
      {
        name: 'device-https'
        properties: {
          frontendIPConfiguration: {
            id: resourceId('Microsoft.Network/applicationGateways/frontendIPConfigurations', gatewayName, 'appGatewayFrontendIp')
          }
          frontendPort: {
            id: resourceId('Microsoft.Network/applicationGateways/frontendPorts', gatewayName, 'port_443')
          }
          protocol: 'Https'
          sslCertificate: {
            id: resourceId('Microsoft.Network/applicationGateways/sslCertificates', gatewayName, 'device-tls-cert')
          }
          sslProfile: {
            id: resourceId('Microsoft.Network/applicationGateways/sslProfiles', gatewayName, 'device-passthrough')
          }
          hostName: deviceFqdn
          requireServerNameIndication: true
        }
      }
    ]
    // The whole edge contract in one rewrite. {var_client_certificate} is the presented chain as
    // PEM, URL-encoded (Application Gateway encodes it for a single-line header). The origin
    // (ingest-api internal/auth parseCertificateChain) accepts raw PEM or percent-encoded PEM via
    // url.QueryUnescape, so this module does not depend on the exact encoding form — see the
    // ASSUMPTION below. X-Forwarded-Proto/Host match what localdev/edge sets.
    rewriteRuleSets: [
      {
        name: 'device-rewrite'
        properties: {
          rewriteRules: [
            {
              name: 'forward-client-cert-and-forwarded'
              ruleSequence: 100
              conditions: []
              actionSet: {
                requestHeaderConfigurations: [
                  {
                    headerName: 'X-Client-Cert'
                    headerValue: '{var_client_certificate}'
                  }
                  {
                    headerName: 'X-Forwarded-Proto'
                    headerValue: 'https'
                  }
                  {
                    headerName: 'X-Forwarded-Host'
                    headerValue: '{var_host}'
                  }
                ]
                responseHeaderConfigurations: []
              }
            }
          ]
        }
      }
    ]
    // Path-based routing: control-api is the default (enrol/token/policy/health/grant); the single
    // ingest prefix /v1/events goes to ingest-api. Mirrors localdev/edge/main.go's route table.
    urlPathMaps: [
      {
        name: 'device-pathmap'
        properties: {
          defaultBackendAddressPool: {
            id: resourceId('Microsoft.Network/applicationGateways/backendAddressPools', gatewayName, 'container-apps')
          }
          defaultBackendHttpSettings: {
            id: resourceId('Microsoft.Network/applicationGateways/backendHttpSettingsCollection', gatewayName, 'control-api-http')
          }
          pathRules: [
            // The device's signed-policy fetch (docs/02 §5.2) is control-api's, which the default
            // already says. It is named anyway because a device now depends on it after every
            // enrolment: a later change to the default must not be able to drop it silently.
            {
              name: 'policy-to-control'
              properties: {
                paths: [
                  '/v1/policy'
                  '/v1/policy/*'
                ]
                backendAddressPool: {
                  id: resourceId('Microsoft.Network/applicationGateways/backendAddressPools', gatewayName, 'container-apps')
                }
                backendHttpSettings: {
                  id: resourceId('Microsoft.Network/applicationGateways/backendHttpSettingsCollection', gatewayName, 'control-api-http')
                }
              }
            }
            {
              name: 'events-to-ingest'
              properties: {
                paths: [
                  '/v1/events'
                  '/v1/events/*'
                ]
                backendAddressPool: {
                  id: resourceId('Microsoft.Network/applicationGateways/backendAddressPools', gatewayName, 'container-apps')
                }
                backendHttpSettings: {
                  id: resourceId('Microsoft.Network/applicationGateways/backendHttpSettingsCollection', gatewayName, 'ingest-api-http')
                }
              }
            }
          ]
        }
      }
    ]
    requestRoutingRules: [
      {
        name: 'device-rule'
        properties: {
          ruleType: 'PathBasedRouting'
          priority: 100
          httpListener: {
            id: resourceId('Microsoft.Network/applicationGateways/httpListeners', gatewayName, 'device-https')
          }
          urlPathMap: {
            id: resourceId('Microsoft.Network/applicationGateways/urlPathMaps', gatewayName, 'device-pathmap')
          }
          rewriteRuleSet: {
            id: resourceId('Microsoft.Network/applicationGateways/rewriteRuleSets', gatewayName, 'device-rewrite')
          }
        }
      }
    ]
    firewallPolicy: {
      id: wafPolicy.id
    }
    enableHttp2: false
  }
  dependsOn: [
    pip
    wafPolicy
  ]
}

// ---------------------------------------------------------------------------------------------
// Outputs

@description('Resource id of the gateway, for drift/monitoring wiring.')
output gatewayId string = gateway.id

@description('The public IP address of the device edge — the only public device endpoint (docs/05 §3.5).')
output deviceEndpointIp string = pip.properties.ipAddress

@description('The gateway SKU name applied, echoed so the composition can assert WAF_v2.')
output skuNameApplied string = skuName
