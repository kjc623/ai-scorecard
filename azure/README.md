# azure/ — the deployment

One Bicep composition (`main.bicep`) deploys one environment. `RUNBOOK.md` is the go-live sequence;
this file describes what is deployed and how to deploy and check it.

## What one environment contains

| Resource | Purpose |
|---|---|
| VNet (`modules/network.bicep`) | Subnets for the Container Apps environment, PostgreSQL, private endpoints and Application Gateway. An NSG on the environment subnet admits only the gateway subnet and the environment itself |
| Application Gateway WAF_v2 (`application-gateway.bicep`) | The device edge on `deviceFqdn`: mutual TLS, client certificate forwarded in `X-Client-Cert`, only the device API routed (WAF allow-list; anything else goes to an empty pool) |
| Front Door Premium + WAF (`frontdoor.bicep`) | The browser edge on `analystFqdn` (managed certificate): `/*` → dashboard, `/scim/v2/*`, `/onboard/*`, `/.well-known/*` → control-api, through a Private Link origin |
| Container Apps environment (`container-apps-env.bicep`) | Internal load balancer, workload profiles, logs to Log Analytics |
| Apps (`container-app.bicep`) | `ingest-api`, `control-api`, `dashboard` (external ingress, VNet only); `query-api`, `content-vault` (internal ingress) |
| Jobs (`container-app-job.bicep`) | `aggregate` (every 5 minutes), `expire` (daily), `migrate` and `tenant-admin` (started on demand) |
| PostgreSQL Flexible Server 16 (`postgres.bicep`) | VNet-integrated, Entra authentication only; the `migrate` identity is its Entra administrator |
| Key Vault (`keyvault.bicep`) | Every secret, read per secret by the identities listed in `main.bicep`; private endpoint |
| Container Registry (`registry.bicep`) | Images, pulled by managed identity; no admin user |
| Log Analytics, alerts, budget | `log-analytics.bicep`, `monitoring.bicep`, `budget.bicep` |

Every app and job runs as its own user-assigned managed identity, which is also its PostgreSQL
login (`ingest-api`, `control-api`, `content-vault`, `query-api`, `jobs`); the `migrate` job creates
those logins and grants each its database role.

## Environments

`params/preprod.bicepparam` and `params/prod.bicepparam`. Production adds zone redundancy
(PostgreSQL HA, gateway, environment), geo-redundant backups and a geo-replicated registry.

Values that belong to the environment's external setup are read from environment variables when the
parameter file is compiled. The deploy workflow sets them from the GitHub environment's variables:

| Variable | Meaning |
|---|---|
| `SAC_DEVICE_FQDN` | Hostname devices connect to |
| `SAC_ANALYST_FQDN` | Hostname browsers use |
| `SAC_ENTRA_APP_CLIENT_ID` | The vendor multi-tenant Entra application |
| `SAC_ALERT_EMAILS` | Comma-separated alert recipients |
| `SAC_KEYVAULT_ADMIN_IPS` | Optional: operator addresses allowed to reach Key Vault to manage secrets |
| `SAC_KEYVAULT_ADMIN_PRINCIPALS` | Optional: Entra object ids granted Key Vault secret and certificate officer roles |
| `SAC_IMAGE_TAG` | Set by the workflow to the commit it built |
| `SAC_BOOTSTRAP` | `true` for an environment's first deployment only |

## Secrets

Created once per environment by `scripts/create-secrets.sh <vault>`; never replaced by it.

| Secret | Format | Read by |
|---|---|---|
| `sac-session-signing-key` | EC P-256 private key, PEM | control-api |
| `sac-policy-signing-key` | Ed25519 private key, PEM | control-api |
| `sac-device-ca-cert`, `sac-device-ca-key` | Device CA certificate and key, PEM | control-api (both), ingest-api (certificate) |
| `sac-directory-key` | 32 random bytes, base64 | control-api |
| `sac-internal-token` | 32 random bytes, base64 | control-api, dashboard |
| `sac-content-keys` | `v1:<base64 32 bytes>[,v0:…]`, current key first | content-vault |
| `sac-cursor-key` | 32 random bytes, base64 | query-api |
| `sac-device-tls` | Certificate for `deviceFqdn` (imported) | Application Gateway |

Rotating content keys: prepend a new version (`v2:…,v1:…`). New content is encrypted with the first
key; older content stays readable until the expire job removes it.

## Deploying

The pipeline does this (`.github/workflows/deploy.yml`). By hand, with the variables above exported:

```sh
az deployment group what-if --resource-group rg-sac-preprod-eastus \
  --template-file azure/main.bicep --parameters azure/params/preprod.bicepparam
az deployment group create  --resource-group rg-sac-preprod-eastus \
  --template-file azure/main.bicep --parameters azure/params/preprod.bicepparam
```

## Checking

```sh
az bicep build --file azure/main.bicep --stdout > /dev/null   # compiles, lint-clean
node azure/tools/check-infra.mjs                               # security properties hold
node --test azure/tools/check-infra.test.mjs                   # each rule trips on a regression
```
