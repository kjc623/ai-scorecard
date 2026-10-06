// container-app.bicep — one Container App running as its user-assigned managed identity.
//
// The identity's client id is always exported as AZURE_CLIENT_ID, which is how the process asks the
// platform for tokens (PostgreSQL, Key Vault references, the Entra app). Secrets arrive as Key Vault
// references, never as values.

param location string

param appName string

param environmentId string

@description('{ id, clientId } of the user-assigned identity the app runs as.')
param identity object

@description('internal: reachable only inside the environment. external: reachable from the VNet (the gateway) and through the environment\'s Private Link (Front Door). No default, so a public-facing app is never created by omission.')
@allowed(['internal', 'external'])
param ingress string

param image string

param registryLoginServer string

param cpu string = '0.5'

param memory string = '1Gi'

@minValue(0)
param minReplicas int

@minValue(1)
param maxReplicas int

@description('Non-secret environment variables: [{ name, value }].')
param env array = []

@description('Secrets as environment variables: [{ name, secret }] where secret is the Key Vault secret name.')
param secretEnv array = []

@description('Secrets as files: Key Vault secret names, each mounted at /mnt/secrets/<name>.')
param secretFiles array = []

@description('Key Vault URI, e.g. https://sac-prod-eastus-kv.vault.azure.net/.')
param vaultUri string

param tags object = {}

var secretNames = union(map(secretEnv, s => s.secret), secretFiles)

resource app 'Microsoft.App/containerApps@2024-03-01' = {
  name: appName
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  properties: {
    environmentId: environmentId
    workloadProfileName: 'Consumption'
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: ingress == 'external'
        targetPort: 8080
        allowInsecure: false
      }
      registries: [
        {
          server: registryLoginServer
          identity: identity.id
        }
      ]
      secrets: [for name in secretNames: {
        name: name
        keyVaultUrl: '${vaultUri}secrets/${name}'
        identity: identity.id
      }]
    }
    template: {
      volumes: empty(secretFiles) ? [] : [
        {
          name: 'secrets'
          storageType: 'Secret'
          secrets: map(secretFiles, name => { secretRef: name, path: name })
        }
      ]
      containers: [
        {
          name: appName
          image: image
          resources: {
            cpu: json(cpu)
            memory: memory
          }
          env: concat(
            [{ name: 'AZURE_CLIENT_ID', value: identity.clientId }],
            env,
            map(secretEnv, s => { name: s.name, secretRef: s.secret })
          )
          volumeMounts: empty(secretFiles) ? [] : [
            {
              volumeName: 'secrets'
              mountPath: '/mnt/secrets'
            }
          ]
          probes: [
            {
              type: 'Liveness'
              httpGet: { path: '/healthz', port: 8080 }
              periodSeconds: 10
              failureThreshold: 3
            }
            {
              type: 'Readiness'
              httpGet: { path: '/readyz', port: 8080 }
              periodSeconds: 5
              failureThreshold: 3
            }
          ]
        }
      ]
      scale: {
        minReplicas: minReplicas
        maxReplicas: maxReplicas
        rules: [
          {
            name: 'http'
            http: { metadata: { concurrentRequests: '40' } }
          }
        ]
      }
    }
  }
}

output id string = app.id
output fqdn string = app.properties.configuration.ingress.fqdn
output ingress string = ingress
