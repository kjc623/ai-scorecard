# `azure/pipelines/` — deploy, watch, and enforce

Three pipelines with three different risk profiles. They are plain YAML definitions rather than a
vendor-specific pipeline format, so what they do can be read without a subscription.

| Pipeline | Trigger | What it does |
|---|---|---|
| `infra.yml` | Manual or on merge to the main branch | Deploys the Bicep for one environment, authenticating with a workload-federated identity |
| `drift.yml` | Nightly, scheduled | A `what-if` against the same parameter file as the last deployment, classifying every difference as expected, noise, or drift. Drift pages on-call |
| `policy-scan.yml` | Every pull request that touches `azure/` | Runs the static rules from `../tools/` and fails the build when a property that must never regress does |

## The properties these exist to protect

`policy-scan.yml` is not a lint. It enforces the architectural properties that a later edit would
otherwise quietly undo — `content-vault` instantiated with external ingress, a PostgreSQL firewall
rule, a PaaS resource with public network access, a storage account with shared-key access. Each of
those is a change nobody would make on purpose and everybody could make by accident, and the check
runs **on the pull request that would break it** rather than on the next deployment.

## No credential lives in a pipeline

Deployment authenticates with a workload-federated identity scoped to a repository, a branch pattern
and an environment, so a pull request from a fork cannot obtain it. There is no stored Azure
credential to rotate and none to leak. The deployment identity holds resource-group deployment rights
only: if a deploy fails asking for Key Vault data-plane or database access, the identity is wrong,
not the template.

## The half that is not here

`drift.yml` runs the scheduled `what-if` half of §3.5. The **resource-graph** half — the queries that
catch a change made in the portal after the fact — needs an Azure connection and is a documented gap
in [`../README.md`](../README.md).
