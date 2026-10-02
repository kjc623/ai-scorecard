// static-web-app.bicep — the dashboard (docs/05-platform-delivery.md §2).
//
// Standard is required for a custom domain with a managed certificate and for Entra ID
// authentication on the app; the dashboard is static (master §4.1). The app is a pure front end:
// it holds no data and no key, and every byte it displays comes from query-api.

@description('Azure region for the Static Web App. Environment-parameterised (§3.2); Static Web Apps are available in a subset of regions, which is why this is a parameter and not a constant.')
param location string

@description('Base name for the environment.')
param baseName string

@description('Static Web App SKU. Standard is required for a custom domain with a managed certificate and for Entra ID authentication on the app; the value is a parameter for the cost model.')
@allowed(['Free', 'Standard'])
param skuName string = 'Standard'

@description('Custom hostname for the dashboard. Empty in dev, where the default hostname is enough.')
param customDomain string = ''

@description('Entra ID client id used by the built-in authentication provider. This is a public client identifier, not a credential; the dashboard holds no secret.')
param entraClientId string = ''

@description('Tags applied to the app.')
param tags object = {}

resource swa 'Microsoft.Web/staticSites@2023-12-01' = {
  name: '${baseName}-swa'
  location: location
  tags: tags
  sku: {
    name: skuName
    tier: skuName
  }
  properties: {
    allowConfigFileUpdates: false // the build configuration is in the repository and reviewed, not editable in the portal
    provider: 'Custom'
    enterpriseGradeCdnStatus: 'Disabled'
    stagingEnvironmentPolicy: 'Enabled'
    publicNetworkAccess: 'Enabled' // the static front end is public by definition; the data it shows is not
    authConfig: entraClientId == '' ? null : {
      identityProviders: {
        azureActiveDirectory: {
          enabled: true
          registration: {
            clientId: entraClientId
            openIdIssuer: 'https://login.microsoftonline.com/${subscription().tenantId}/v2.0'
          }
        }
      }
    }
  }
}

resource customDomainResource 'Microsoft.Web/staticSites/customDomains@2023-12-01' = if (customDomain != '') {
  name: '${swa.name}/${customDomain}'
  properties: {
    validationMethod: 'cname-delegation'
  }
}

@description('Resource id of the Static Web App.')
output staticSiteId string = swa.id

@description('The default hostname. The custom domain, when set, is CNAME’d to it.')
output defaultHostname string = swa.properties.defaultHostname
