// container-app-job.bicep — one Container Apps job, scheduled or started manually, running as its
// user-assigned managed identity (exported as AZURE_CLIENT_ID).

param location string

param jobName string

param environmentId string

@description('{ id, clientId } of the user-assigned identity the job runs as.')
param identity object

param image string

param registryLoginServer string

@description('Arguments passed to the image entrypoint.')
param args array = []

@description('Cron expression for a scheduled job; empty for a job started manually.')
param cronExpression string = ''

@description('Seconds a replica may run before it is stopped and retried.')
param replicaTimeout int = 1800

param cpu string = '0.5'

param memory string = '1Gi'

@description('Non-secret environment variables: [{ name, value }].')
param env array = []

@description('Secrets as environment variables: [{ name, secret }] where secret is the Key Vault secret name.')
param secretEnv array = []

param vaultUri string = ''

param tags object = {}

var scheduled = !empty(cronExpression)

resource job 'Microsoft.App/jobs@2024-03-01' = {
  name: jobName
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
      triggerType: scheduled ? 'Schedule' : 'Manual'
      scheduleTriggerConfig: scheduled ? {
        cronExpression: cronExpression
        parallelism: 1
        replicaCompletionCount: 1
      } : null
      manualTriggerConfig: scheduled ? null : {
        parallelism: 1
        replicaCompletionCount: 1
      }
      replicaTimeout: replicaTimeout
      replicaRetryLimit: 1
      registries: [
        {
          server: registryLoginServer
          identity: identity.id
        }
      ]
      secrets: [for s in secretEnv: {
        name: s.secret
        keyVaultUrl: '${vaultUri}secrets/${s.secret}'
        identity: identity.id
      }]
    }
    template: {
      containers: [
        {
          name: jobName
          image: image
          args: args
          resources: {
            cpu: json(cpu)
            memory: memory
          }
          env: concat(
            [{ name: 'AZURE_CLIENT_ID', value: identity.clientId }],
            env,
            map(secretEnv, s => { name: s.name, secretRef: s.secret })
          )
        }
      ]
    }
  }
}

output id string = job.id
