// container-app-job.bicep — one scheduled Container Apps Job (docs/05-platform-delivery.md §2, §3.4).
//
// The aggregator and the reconciler are the two jobs in §2: both are set-based SQL over a window,
// both run one replica (concurrency would contend on the same buckets), and both carry their own
// managed identity because "can recompute a bucket" and "can ingest an event" are different grants
// (§5.1). The migration job is a third instantiation with a different identity class (§5.4).

@description('Azure region for the job. Environment-parameterised (§3.2).')
param location string

@description('Job name, e.g. aggregator or reconciler.')
param jobName string

@description('Resource id of the Container Apps environment, from container-apps-env.bicep.')
param environmentId string

@description('Resource id of the user-assigned managed identity the job runs as. The job’s grant is what it may write; the module does not grant anything (§5.1).')
param userAssignedIdentityId string

@description('Fully-qualified image for the job.')
param image string

@description('Registry login server; the job pulls over the registry’s private endpoint.')
param registryLoginServer string

@description('Cron expression for the schedule. The aggregator runs every 5 minutes over a lookback window; the reconciler runs daily (C27, C34).')
param cronExpression string = '*/5 * * * *'

@description('Replica timeout in seconds. A job that exceeds it is killed and retried rather than holding a lock indefinitely.')
@minValue(60)
@maxValue(86400)
param replicaTimeout int = 900

@description('Replica retry limit. A job that keeps failing is a page, not an infinite loop.')
@minValue(0)
@maxValue(10)
param replicaRetryLimit int = 2

@description('Parallelism. 1 for both jobs: the work is set-based SQL over the same buckets, and parallelism would contend rather than help (§2 inventory).')
@minValue(1)
@maxValue(1)
param parallelism int = 1

@description('Completion count per schedule. 1, paired with parallelism 1, so a run is one replica doing one unit of work.')
@minValue(1)
@maxValue(1)
param replicaCompletionCount int = 1

@description('CPU cores for the job replica, e.g. 1.0.')
param cpu string = '1.0'

@description('Memory for the job replica, e.g. 2Gi.')
param memory string = '2Gi'

@description('Non-secret environment variables. Credentials arrive as Key Vault references, never as values (§5.4).')
param env array = []

@description('Key Vault-backed environment variables: [{ name, keyVaultUrl, identity }]. name is the environment variable; the Container Apps secret behind it is the same name lower-cased with hyphens, because a secret name may not hold an underscore or a capital.')
param keyVaultEnv array = []

@description('Command args passed to the image entrypoint, so one image can host either job with an explicit argument rather than a second build.')
param args array = []

@description('Tags applied to the job.')
param tags object = {}

// One secret per Key Vault reference, named the way Container Apps requires (lower-case, hyphens),
// and the environment variable that reads it. Variables rather than inline for-expressions, because a
// for-expression is not allowed inside concat().
var secretRefs = [for kv in keyVaultEnv: {
  name: toLower(replace(kv.name, '_', '-'))
  keyVaultUrl: kv.keyVaultUrl
  identity: kv.identity
}]
var secretEnv = [for kv in keyVaultEnv: {
  name: kv.name
  secretRef: toLower(replace(kv.name, '_', '-'))
}]

resource job 'Microsoft.App/jobs@2024-03-01' = {
  name: jobName
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${userAssignedIdentityId}': {}
    }
  }
  properties: {
    environmentId: environmentId
    configuration: {
      triggerType: 'Schedule'
      scheduleTriggerConfig: {
        cronExpression: cronExpression
        parallelism: parallelism
        replicaCompletionCount: replicaCompletionCount
      }
      replicaTimeout: replicaTimeout
      replicaRetryLimit: replicaRetryLimit
      registries: [
        {
          server: registryLoginServer
          identity: userAssignedIdentityId
        }
      ]
      secrets: secretRefs
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
          env: concat(env, secretEnv)
        }
      ]
    }
  }
}

@description('Resource id of the job.')
output jobId string = job.id

@description('The schedule actually applied, echoed so the composition and the runbook agree.')
output schedule string = cronExpression
