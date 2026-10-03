# `azure/pipelines/` — deploy, watch, and enforce

Three pipelines with three different risk profiles. They are GitHub Actions workflow definitions, so
what they do can be read without a subscription. **Nothing in this repository triggers them**: GitHub
runs workflows only from `.github/workflows/`, this repository has no `.github/` directory, and these
files are stored here beside the Bicep they act on. The triggers below are the ones each file
declares, which take effect once the file is installed as a workflow.

| Pipeline | Declared trigger | What it does |
|---|---|---|
| `infra.yml` | Manual, or a push to `main` that touches `azure/` | Deploys the Bicep for one environment, authenticating with a workload-federated identity |
| `drift.yml` | A nightly schedule, or manual | A `what-if` against each environment's parameter file. The workflow reports one of two results: drift (a change touching network access, identity, encryption or ingress properties), which fails the run, or no drift. Sorting the remaining differences into expected and noise is left to a human reading the uploaded report |
| `policy-scan.yml` | A pull request, or a push to `main`, that touches `azure/` | Runs the static rules from `../tools/` and fails when a property that must never regress does |

## The properties these exist to protect

`policy-scan.yml` is not a lint. It enforces the architectural properties that a later edit would
otherwise quietly undo — `content-vault` instantiated with external ingress, a PostgreSQL firewall
rule, a PaaS resource with public network access, a storage account with shared-key access. Each of
those is a change nobody would make on purpose and everybody could make by accident, and the check
is written to run **on the pull request that would break it** rather than on the next deployment.
Until the workflow is installed, the same rules run locally with
`node --test azure/tools/index.mjs`.

## No credential lives in a pipeline

Deployment authenticates with a workload-federated identity scoped to a repository, a branch pattern
and an environment, so a pull request from a fork cannot obtain it. There is no stored Azure
credential to rotate and none to leak. The deployment identity holds resource-group deployment rights
only: if a deploy fails asking for Key Vault data-plane or database access, the identity is wrong,
not the template.

## The half that is not here

`drift.yml` defines the scheduled `what-if` half of §3.5. The **resource-graph** half — the queries that
catch a change made in the portal after the fact — needs an Azure connection and is a documented gap
in [`../README.md`](../README.md).
