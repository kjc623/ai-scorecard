# azure/ — the Azure delivery of docs/05-platform-delivery.md

This directory is the deployment as code: one Bicep composition (`main.bicep`) shared by every
environment, modules that own one resource family each, parameter files that carry everything an
environment may differ on, three pipelines, and a static checker that validates the whole thing
**without an Azure subscription**.

## Deployment is NOT VERIFIED

Nothing in this directory has ever been deployed. There is no Azure subscription, so:

- the Bicep **compiles** (`az bicep build --file azure/main.bicep`, Bicep CLI 0.48.1, 2026-10-05: no
  errors, 32 warnings). Compiling fixed six modules that had never compiled. Reconciling the warnings
  found three real defects rather than type-definition noise: `verifyClientAuthMode` on the
  Application Gateway is not a property of the client-auth configuration before API `2025-03-01`, so
  on the old version the passthrough listener would have compiled but silently not applied (the
  module now pins `2025-03-01`); `requestBodyInspectLimitInKB` is an **Application Gateway** WAF
  setting and does not exist on Front Door's policy (removed); and `Microsoft.Consumption/budgets`
  has no top-level `tags` property (removed). The warnings that remain are the linter's
  (`use-parent-property`, `prefer-unquoted-property-names`, four unused parameters, three hardcoded
  private-DNS suffixes) and five `BCP318` notices about conditionally deployed modules that a
  reviewer can see are guarded by the same condition. A `validate` against a real subscription is
  still what confirms the rest;
- no resource has been created, so no `what-if` output, no idempotency check and no `existing`-resource
  behaviour has been observed;
- the prices in `cost-model.md` have **not** been checked against the Azure pricing calculator;
- the private DNS, Private Link approval, managed-identity role assignment and Container Apps
  revision behaviours have not been observed — they are stated as the document requires, and only a
  deployment can confirm them.

**What is verified here** is what can be verified without Azure: every parameter is typed and
described, no region or SKU is hard-coded outside a parameter file, no secret is in the repository,
the private-only properties are declared as such, content-vault is internal-only with no Front Door
route, the database has no public path and no firewall rules, and every row of the document's §2
inventory is implemented. Those checks run in `node --test azure/tools/index.mjs` and they fail when a
property regresses.

### The exact commands a human would run

Preconditions, all of which must hold before the commands mean anything:

1. `az` CLI **2.60+** with the Bicep CLI installed (`az bicep install`), and a login under the
   **workload-federated deployment identity** — never a stored credential (§5.2). In CI this is an OIDC
   token for the repository, branch and environment; locally it is a human operator's Entra ID
   principal with a PIM-eligible role.
2. The subscription has the resource providers registered: `Microsoft.App`, `Microsoft.DBforPostgreSQL`,
   `Microsoft.Storage`, `Microsoft.KeyVault`, `Microsoft.Network`, `Microsoft.Cdn`,
   `Microsoft.OperationalInsights`, `Microsoft.Insights`, `Microsoft.ContainerRegistry`,
   `Microsoft.Web`, `Microsoft.ManagedIdentity`, `Microsoft.Consumption`.
3. The deployment identity holds **resource-group deployment rights only** (§5.4). It must not hold Key
   Vault data-plane or database access; if the deployment fails asking for those, the identity is wrong,
   not the template.
4. The PostgreSQL break-glass login name comes from the pipeline (a variable), and its credential is
   supplied at deploy time from Key Vault. `passwordAuth` is disabled on the server, so the name alone
   proves nothing.
5. The container images named by `imageTag` already exist in the registry — the platform pulls over the
   registry's private endpoint, and a missing image is a failed revision, not a failed deployment.

Before the first deployment, create the four identity secrets control-api and the dashboard read
from the environment's Key Vault (`<baseName>-kv`), plus the device-trust secret the origin reads.
Values are generated, never committed, and the directory key and the policy key must be kept: losing
the first makes every sealed column unreadable, and the second's public half is the trust anchor
shipped in the agent MSI.

```powershell
$kv = 'sac-prod-eastus-kv'
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out session.key   # product tokens (ES256)
openssl genpkey -algorithm ED25519 -out policy.key                                 # signed policy bundles
az keyvault secret set --vault-name $kv --name sac-session-signing-key --file session.key
az keyvault secret set --vault-name $kv --name sac-policy-signing-key --file policy.key
az keyvault secret set --vault-name $kv --name sac-internal-token --value (openssl rand -base64 32)
az keyvault secret set --vault-name $kv --name sac-directory-key --value (openssl rand -base64 32)
Remove-Item session.key, policy.key

# The product device certificate authority (ADR 0020 decision 3). control-api SIGNS x509 device leaves
# with it (SAC_CA_CERT_PEM / SAC_CA_KEY_PEM) and ingest-api RE-VERIFIES the forwarded leaf against the
# certificate (SAC_TLS_CLIENT_CA_PEM), so both sides must receive the same CA. Generate the pair once
# and keep it: rotating the CA invalidates every enrolled credential until devices re-enrol.
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out device-ca.key
openssl req -x509 -new -key device-ca.key -days 3650 -subj "/CN=Shadow AI Capture Device CA" -out device-ca.pem
az keyvault secret set --vault-name $kv --name sac-device-ca-cert --file device-ca.pem
az keyvault secret set --vault-name $kv --name sac-device-ca-key  --file device-ca.key
Remove-Item device-ca.key, device-ca.pem
```

After it, add control-api's managed identity as a federated credential on the vendor's multi-tenant
Entra app, from the `entraFederatedCredential` output (the command is in the comment above that
output in `main.bicep`). Until it exists, control-api cannot authenticate as the app, so neither an Entra sign-in nor the Intune check works; OIDC customers are unaffected.

```powershell
# 1. The resource group and the environment-level checks the template cannot do itself.
az group create --name rg-sac-prod-eastus --location eastus
az deployment group validate `
  --resource-group rg-sac-prod-eastus `
  --template-file azure/main.bicep `
  --parameters azure/params/prod.eastus.bicepparam `
  --parameters postgresAdministratorLogin=$env:SAC_PG_ADMIN_LOGIN

# 2. Always look at the diff before applying it.
az deployment group what-if `
  --resource-group rg-sac-prod-eastus `
  --template-file azure/main.bicep `
  --parameters azure/params/prod.eastus.bicepparam `
  --parameters postgresAdministratorLogin=$env:SAC_PG_ADMIN_LOGIN

# 3. Apply. The pipeline does this from `azure/pipelines/infra.yml`; a human does it only during a
#    declared incident.
az deployment group create `
  --resource-group rg-sac-prod-eastus `
  --template-file azure/main.bicep `
  --parameters azure/params/prod.eastus.bicepparam `
  --parameters postgresAdministratorLogin=$env:SAC_PG_ADMIN_LOGIN

# 4. Approve the Front Door private-link connection to the Container Apps environment. It is created
#    pending approval on purpose: an origin that appears without a human is an origin nobody approved.
az network private-endpoint-connection list --id <container-apps-environment-id>
```

After a deployment, the properties that only exist at runtime are asserted by the hourly resource-graph
queries in §3.5 (no PaaS resource with public network access, no storage account with shared-key access,
content-vault ingress internal, every container app with a user-assigned identity, every vault with
purge protection). Those queries are not in this directory yet: `azure/pipelines/drift.yml` defines the
`what-if` half, and the resource-graph half is a documented gap (see "Gaps" below).

## How to check the infrastructure without Azure

```powershell
node --test azure/tools/index.mjs        # the suite: 47 checks
node --test azure/tools/check-infra.test.mjs   # the same suite, named directly
node azure/tools/check-infra.mjs         # the same checks as a report, exit 1 on a finding
```

The invocations above were run and pass on Node 22.23.1 (Windows). `node --test azure/tools/` — the
directory form — resolves the directory as a module on this Node build and fails with
`MODULE_NOT_FOUND` before running anything, which is why `index.mjs` exists and is named explicitly
here; `tools/verify-all.mjs` discovers `check-infra.test.mjs` by name and reports `azure/tools` PASS.
A glob also works: `node --test 'azure/tools/*.test.mjs'`.

The checker has no dependencies and never makes a network call. It reads `azure/**/*.bicep`,
`azure/params/*.bicepparam`, `azure/inventory.json`, `azure/cost-model.md` and
`docs/05-platform-delivery.md`.

## What is where

| Path | What it is |
|---|---|
| `main.bicep` | The one composition every environment deploys: modules, identity bindings, the two architectural assertions stated in the clear, and the `deployEdge` / `deployDashboard` / `deployExports` switches (all `true` by default) that let a lab omit the two edges (Front Door + Application Gateway), the Static Web App, and the export account |
| `modules/` | One resource family per module, matching §3.3's layout exactly |
| `modules/application-gateway.bicep` | The public device edge: `WAF_v2`, dedicated subnet, public IP, HTTPS listener for the device FQDN, SSL profile in client-auth **passthrough** (a certificate is requested, never required), a rewrite that forwards the presented certificate as `X-Client-Cert` plus `X-Forwarded-Proto`/`Host`, a backend pool to the internal Container Apps static IP with per-app Host headers, and health probes (ADR 0020 decision 1) |
| `params/` | `dev`, `staging`, `prod.eastus`: the only place an environment differs (§3.2). `lab` is the architecture-fidelity lab: the same composition with the three `deploy*` switches off |
| `pipelines/` | `infra.yml` (deploy), `drift.yml` (scheduled `what-if`), `policy-scan.yml` (the properties that must never regress). GitHub Actions workflow definitions; nothing in this repository triggers them — see `pipelines/README.md` |
| `inventory.json` | Every §2 inventory row mapped to the module that implements it; §2.2's deliberately-absent list with the resource types that must never appear |
| `cost-model.md` | §11's arithmetic recomputed from its own unit prices, with the disagreements stated as findings |
| `tools/` | The zero-dependency checker and its suite |

## The device edge and its local stand-in

ADR 0020 decision 1 splits the two audiences across two edges: **Application Gateway** is the public
device ingress (the device FQDN), and **Front Door** is the analyst ingress. `modules/application-gateway.bicep`
is kept behaviourally aligned with `localdev/edge/main.go`, the Docker stand-in that has been proven
end to end in the lab:

| Behaviour | localdev/edge | application-gateway.bicep |
|---|---|---|
| Terminate TLS, request a client cert without requiring one | `tls.RequestClientCert` | `sslProfiles[].clientAuthConfiguration.verifyClientAuthMode: 'Passthrough'` |
| Forward the presented cert as PEM | `peerCertPEM` + `url.QueryEscape` → `X-Client-Cert` | rewrite `X-Client-Cert = {var_client_certificate}` |
| `X-Forwarded-Proto` / `X-Forwarded-Host` for the DPoP `htu` | `pr.Out.Header.Set(...)` | rewrite `X-Forwarded-Proto = https`, `X-Forwarded-Host = {var_host}` |
| Route table | `/v1/events → ingest`, others → control | path map: `/v1/events/* → ingest`, default → control |

The client-certificate header encoding is an **assumption to verify on a real subscription**: the
gateway produces URL-encoded PEM, but `ingestion/ingest-api/internal/auth.parseCertificateChain`
accepts raw PEM or percent-encoded PEM (`url.QueryUnescape`), so the origin does not depend on the
exact form.

## The inventory diff against docs/05 §2

All **18** rows of §2 are implemented, and `inventory.json` records which module implements each. The
checker diffs in both directions, so the state is not a claim in prose:

- a §2 row with no module is reported as `inventory-missing` (**currently none**);
- a module with no §2 row is reported as `inventory-extra` (**currently none**);
- a §3.3 module missing from disk is `layout-missing` (**currently none**);
- a module outside §3.3's layout is `layout-extra` (**currently none**);
- a §2.2 deliberately-absent resource type appearing anywhere is `deliberately-absent`
  (**currently none** — no Service Bus, Event Hubs, AI Search, Synapse, Databricks, Fabric, AKS,
  virtual machines or reservations).

## Gaps, stated rather than implied

1. **The hourly resource-graph assertions of §3.5 are not implemented.** `drift.yml` defines the nightly
   `what-if`; the resource-graph queries (the ones that catch a portal change after the fact) need an
   Azure connection and are a documented gap.
2. **No environment has been deployed**, so nothing in this directory has been observed against a real
   API version. The checker proves the properties are *declared*; only a deployment proves they are
   *effective*.
3. **The cost model is list-price arithmetic, not a bill.** Two findings in `cost-model.md` change the
   figure materially (Front Door's base is regional in this architecture, not per-tenant; the Key Vault
   key charge has no unit price in §11.1).
