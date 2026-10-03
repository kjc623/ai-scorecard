# `azure/modules/` — one resource family per module

Seventeen modules, matching the layout `docs/05-platform-delivery.md` §3.3 prescribes. `../main.bicep`
composes them; each module owns one resource family and takes everything an environment may vary as a
parameter.

| Module | Resource family |
|---|---|
| `network.bicep` | The virtual network, subnets and private DNS zones |
| `private-endpoints.bicep` | Private endpoints for every data service |
| `postgres.bicep` | Azure Database for PostgreSQL Flexible Server, private-only, no firewall rules |
| `storage-ciphertext.bicep` | The blob account holding content ciphertext |
| `storage-exports.bicep` | The blob account holding scheduled exports |
| `keyvault.bicep` | Key Vault for wrapped keys and deployment material |
| `managed-hsm.bicep` | Managed HSM, for the tenants that require it |
| `container-apps-env.bicep` | The Container Apps environment the services run in |
| `container-app.bicep` | One service: ingress, probes, identity, environment, scale |
| `container-app-job.bicep` | One scheduled or manual job, on the same environment as the apps |
| `registry.bicep` | The container registry the images are pulled from |
| `frontdoor.bicep` | Front Door Premium: the device-facing edge |
| `waf.bicep` | The WAF policy Front Door attaches |
| `log-analytics.bicep` | The workspace everything reports into |
| `monitoring.bicep` | Alerts, action groups and the SLO surfaces |
| `static-web-app.bicep` | Hosting for the dashboard |
| `budget.bicep` | The cost budget and its notifications |

## Two things the composition states in the clear

`../main.bicep` carries two architectural assertions as ordinary code, because a property that only
exists in prose is a property that regresses:

- **`content-vault` is instantiated with internal ingress only and no Front Door route.** It is the
  only component that can unwrap content, and it must not be reachable from the internet. A CI policy
  check fails the build if its ingress ever becomes external.
- **The PostgreSQL server has no public path and no firewall rules.** Its private endpoint is the only
  way in.

## Reading order

Start with `container-app.bicep` — it is the module every service uses, and it carries the comments
explaining the probe scheme, the identity binding and why the app's configuration arrives as
environment variables rather than a mounted file. Then `postgres.bicep` and `network.bicep`, which
everything else depends on.

## What these modules cannot tell you

Bicep is compiled by Azure, not here: there is no `az` CLI and no Bicep compiler on the machine this
was written on, so **none of these files has been compiled**. A syntax error or a wrong resource
property is a real possibility, and the checker next door proves only that the properties are
*declared*, never that they are *effective*. `../README.md` states this at length.
