// container-app.bicep — one Container App (docs/05-platform-delivery.md §2, §3.3, §5.1).
//
// One module parameterised by identity, scale and ingress, because the four apps differ in exactly
// those and duplicating the module four times is how their identities drift (§3.3). Two properties
// are enforced here rather than left to the composition:
//
//   - every app runs as a user-assigned managed identity; there is no other authentication path,
//     and no container app environment variable carries a credential (§5.1);
//   - `ingress` is an explicit input with no default, so a composition cannot create a public app by
//     forgetting a property. content-vault's internal-only ingress is asserted at the composition
//     level and by the CI policy check (D7).

@description('Azure region for the app. Environment-parameterised (§3.2).')
param location string

@description('Container app name, e.g. ingest-api. The route-to-app mapping is fixed by the document, not by this module.')
param appName string

@description('Resource id of the Container Apps environment, from container-apps-env.bicep.')
param environmentId string

@description('Resource id of the user-assigned managed identity the app runs as. §5.1: every service runs as one, and the grant is the security boundary.')
param userAssignedIdentityId string

@description('Ingress: internal (reachable only inside the environment) or external (reachable through the environment’s private-link origin). No default on purpose — a missing value must fail, not silently mean external.')
@allowed(['internal', 'external'])
param ingress string

@description('Container port the app listens on. The service validates its own environment at boot (§3.2) and this is the port it expects.')
param targetPort int = 8080

@description('CPU cores per replica. 0.5 for the API services, 1 for content-vault, which streams ciphertext (§2 inventory).')
param cpu string = '0.5'

@description('Memory per replica, e.g. 1Gi or 2Gi. content-vault gets the larger value because it streams ciphertext.')
param memory string = '1Gi'

@description('Minimum replicas. 2 is the availability floor for 99.9% ingest; content-vault uses 1 because it has no user-facing endpoint (§2 inventory).')
@minValue(0)
@maxValue(30)
param minReplicas int = 2

@description('Maximum replicas. The ceiling covers a fleet flush, which is bursty by nature (§2 inventory).')
@minValue(1)
@maxValue(50)
param maxReplicas int = 20

@description('Fully-qualified image, e.g. sacprodacr.azurecr.io/ingest-api:1.2.3. Deployments pin a digest in the pipeline; the module takes whatever the pipeline resolved.')
param image string

@description('Registry login server, so the app pulls over the registry’s private endpoint with its managed identity rather than a registry password.')
param registryLoginServer string

@description('Non-secret environment variables. Credentials are never placed here: a secret in a plaintext environment variable reaches Log Analytics through revision logs (§5.4).')
param env array = []

@description('Key Vault-backed environment variables: [{ name, keyVaultUrl, identity }]. This is the only credential-shaped input the module accepts, and it is a reference, not a value.')
param keyVaultEnv array = []

@description('HTTP concurrency target for the scale rule. A flush arrives as a burst of batches, so concurrency rather than CPU is the trigger.')
param concurrencyTarget int = 40

@description('Liveness probe path. Every service exposes it; §10.2’s SLOs are computed from the same surface.')
param livenessPath string = '/healthz'

@description('Readiness probe path. Distinct from liveness on purpose: a service holding a broken database connection is ready to be taken out of rotation, not killed.')
param readinessPath string = '/readyz'

@description('Scheme both probes use. HTTP while the container serves plaintext (the default, and what every app does today). Set HTTPS for an app that terminates TLS on targetPort itself: an HTTP probe against a TLS listener can never succeed, so the app would fail its own liveness check. §2.1 requires mutual TLS on the device channel, and F5 in the lab work is where this became visible.')
@allowed(['HTTP', 'HTTPS'])
param probeScheme string = 'HTTP'

@description('Tags applied to the app.')
param tags object = {}

resource app 'Microsoft.App/containerApps@2024-03-01' = {
  name: appName
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${userAssignedIdentityId}': {}
    }
  }
  properties: {
    managedEnvironmentId: environmentId
    configuration: {
      activeRevisionsMode: 'Single' // revision rollback is the pipeline’s job (§4.4); Single keeps the traffic story auditable
      ingress: {
        external: ingress == 'external'
        targetPort: targetPort
        transport: 'http2'
        allowInsecure: false
        traffic: [
          {
            latestRevision: true
            weight: 100
          }
        ]
        // No ipSecurityRestrictions: the only ingress path is Front Door over Private Link, and an
        // IP allowlist here would be a second, weaker gate that hides a broken private-link origin.
      }
      registries: [
        {
          server: registryLoginServer
          identity: userAssignedIdentityId
        }
      ]
      secrets: [for kv in keyVaultEnv: {
        name: kv.name
        keyVaultUrl: kv.keyVaultUrl
        identity: kv.identity
      }]
    }
    template: {
      containers: [
        {
          name: appName
          image: image
          resources: {
            cpu: json(cpu)
            memory: memory
          }
          env: concat(env, [for (kv, i) in keyVaultEnv: {
            name: kv.name
            secretRef: kv.name
          }])
          livenessProbe: {
            httpGet: {
              path: livenessPath
              port: targetPort
              scheme: probeScheme
            }
            initialDelaySeconds: 5
            periodSeconds: 10
            failureThreshold: 3
          }
          readinessProbe: {
            httpGet: {
              path: readinessPath
              port: targetPort
              scheme: probeScheme
            }
            initialDelaySeconds: 3
            periodSeconds: 5
            failureThreshold: 3
          }
        }
      ]
      scale: {
        minReplicas: minReplicas
        maxReplicas: maxReplicas
        rules: [
          {
            name: 'http-concurrency'
            http: {
              metadata: {
                concurrentRequests: string(concurrencyTarget)
              }
            }
          }
        ]
      }
    }
  }
}

@description('Resource id of the app, for Front Door origin wiring and role assignments.')
output appId string = app.id

@description('The app’s fully-qualified internal name. Front Door reaches it as a private-link origin, never over the public internet.')
output fqdn string = app.properties.configuration.ingress.fqdn

@description('The ingress mode actually applied, echoed so the composition can assert internal-only for content-vault.')
output ingressMode string = ingress
