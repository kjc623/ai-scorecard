# azure/RUNBOOK.md — preflight and the go-live sequence

`README.md` says what is deployed and how to check it **without** Azure. This file is the operational
companion: the order a human sets an environment up in, the decisions that must be answered before the
first deployment, and the checks that say each phase is done. The exact `az` commands live in
`README.md` ("The exact commands a human would run"); this file does not repeat them, it says when to
run them and what must be true first.

**Each step names the repository location that implements it**, so a step is never a portal action with
no file behind it. A step with no path is a subscription/account action that has no repository artefact.

**Nothing here has been run.** There is no Azure subscription, so every step below is a plan, not an
observation. The static suite (`node --test azure/tools/index.mjs`, **50 checks**) proves the properties
are *declared*; only a deployment proves they are *effective*.

---

## The edge decision: Front Door **Standard** (non-Premium)

Front Door Premium exists in this design for exactly one reason: the **Private Link origin** that keeps
the Container Apps environment off the public internet (`docs/05-platform-delivery.md` §2.1; ADR 0020).
**Standard cannot use a Private Link origin**, so choosing Standard changes the analyst edge from
*Front Door → Private Link → private environment* to *Front Door → **public** container app FQDN*.

Every consequence below is a **pre-prod-only deviation that must never be promoted**. `docs/lab/LAB-COST.md`
§7.6 names the mechanism ("open mode") and §8 lists it among the shortcuts that become defects if shipped.

| # | Consequence | Repository location to change |
|---|---|---|
| 1 | The environment must expose **public** endpoints (`internalLoadBalancer: false`) | `azure/modules/container-apps-env.bicep` |
| 2 | Front Door drops `sharedPrivateLinkResource`; origins become the apps' public FQDNs | `azure/modules/frontdoor.bicep` (origins, and `skuName`) |
| 3 | WAF policy SKU must match: `Standard_AzureFrontDoor` | `azure/modules/waf.bicep` |
| 4 | The "approve the Private Link connection" step **disappears** | this file, Phase 5 (was step 3) |
| 5 | **A checked invariant now fails.** The suite asserts the environment uses an internal load balancer, which Standard's public origin contradicts | `azure/tools/check-infra.mjs`, `azure/tools/check-infra.test.mjs` |
| 6 | Cost: the Front Door base falls from ~$330 to ~$35 per profile per month | `azure/cost-model.md` (`FD-SHARED-OR-PER-TENANT`) |

**Point 5 is not cosmetic.** `azure/tools/check-infra.test.mjs` ("the Container Apps environment uses
an internal load balancer") and `azure/tools/check-infra.mjs` were written to make the private-origin
property un-regressable. A Standard-Front-Door pre-prod *intentionally* violates it. The parameter file
for that environment must carry the deviation explicitly and the checker must be taught to allow it for
that file — **not** have the failing test quietly deleted. Until the templates and the checker carry the
deviation, `node --test azure/tools/index.mjs` is expected to fail for this environment.

If a customer ever requires the private-origin property, move back to Premium; nothing else changes.

---

## Repository map

| Path | What lives there |
|---|---|
| `azure/main.bicep` | The one composition; all wiring and the `deployEdge` / `deployDashboard` / `deployExports` switches |
| `azure/modules/` | One resource family per module (`frontdoor.bicep`, `waf.bicep`, `application-gateway.bicep`, `container-apps-env.bicep`, …) |
| `azure/params/` | The per-environment parameter files (`dev`, `staging`, `prod.eastus`, `lab`) |
| `azure/README.md` | The exact `az` commands and the static-vs-deployed distinction |
| `azure/cost-model.md`, `azure/COST-FINDING.md` | The §11 arithmetic and its two unresolved findings |
| `azure/pipelines/` | `infra.yml`, `drift.yml`, `policy-scan.yml` — inert until installed under `.github/workflows/` |
| `azure/tools/` | The zero-dependency static checker and its suite |
| `control/control-api/` | Enrolment, deployment keys, the Intune check, policy delivery, the device-certificate signer |
| `ingestion/ingest-api/` | The device write path; device authentication |
| `vault/content-vault/` | Ciphertext store; the only unwrap identity |
| `query/query-api/`, `query/dashboard/` | The read API and the browser app (the dashboard server is the BFF) |
| `database/schema.sql` | The whole schema; `invariants.test.sql` is its assertion suite |
| `docs/05-platform-delivery.md` | The delivery design (environments, pipelines, identity, secrets) |
| `docs/lab/LAB-COST.md` | The cheapest faithful Azure lab and the §8 do-not-promote list |
| `installer/` | The MSI/PKG/Linux agent packages and the tenant-package mechanism |
| `localdev/` | The local labs, including the device-auth lab and the config-vocabulary checker |
| `tools/accept.mjs` | The repository acceptance gate |

---

## The state on 2026-10-05

| Ready | Not ready |
|---|---|
| Bicep composition, 15 modules, 4 parameter files | No subscription; nothing has ever been created |
| 50 static checks (inventory, invariants, cost model) | No `validate`/`what-if` output has been seen |
| Edge, dashboard, exports switches for a cheap lab | Pipelines are inert: no `.github/` directory |
| Key Vault role separation enforced; Application Gateway API 2025-03-01 | Four+ Key Vault secrets and the Entra app do not exist |
| Shape A product-issued x509 device auth wired end to end | No image build/push pipeline; `reconciler`/`migrations` images absent |
| **Front Door Standard chosen** (see above; deviates from private origin) | `SAC_STORE=sql` unset; production images build without the SQL driver |

---

## Phase 0 — decisions that must precede everything

1. **Residency region.** `az`/params assume `eastus`. One production environment per data-residency
   region; a second region is a second parameter file. *Repo: `azure/params/prod.<region>.bicepparam`,
   `docs/05` §3.1.*
2. **Edge tier — DECIDED: Front Door Standard.** Accepts the public-origin deviation above, and the
   loss of the "no container app holds a public IP" property, for ~$295/month less. *Repo:
   `azure/modules/frontdoor.bicep`, `azure/modules/waf.bicep`, `azure/modules/container-apps-env.bicep`,
   `docs/05` §2.1, `docs/lab/LAB-COST.md` §7.6/§8.*
3. **Shared or per-tenant edge.** The two base fees are built once per region by `main.bicep` but are
   charged per tenant in §11.2; the readings differ by ~1.7×. *Repo: `azure/COST-FINDING.md`,
   `azure/cost-model.md` `FD-SHARED-OR-PER-TENANT` / `AGW-SHARED-OR-PER-TENANT`.*
4. **First milestone.** Recommended: a throwaway resource group, deploy the `lab` or `dev` shape,
   confirm `validate`/`what-if`/deploy, delete. *Repo: `azure/params/lab.bicepparam`,
   `azure/params/dev.bicepparam`.*
5. **DNS names.** `device.sac.example.com` / `app.sac.example.com` are placeholders. The device FQDN
   needs a Key Vault certificate; the analyst FQDN is a Front Door custom domain (Standard can use a
   Front Door managed certificate) and must match the Entra redirect URIs. *Repo: `azure/params/*`,
   `azure/modules/application-gateway.bicep`.*

## Phase 1 — subscription prerequisites

- [ ] Subscription, billing, and a region with the SKUs (`D2ds_v5` vCores, `WAF_v2`; **Front Door
      Standard** is available far more broadly than Premium). *Repo: `azure/README.md` precondition 2.*
- [ ] Register the resource providers listed in `README.md` precondition 2. *Repo: `azure/README.md`.*
- [ ] Cost approval. Pre-prod with both edges: ~$700–1,100/month; Standard Front Door trims ~$295 of
      that. *Repo: `azure/cost-model.md`, `azure/params/*.bicepparam` (`monthlyBudgetAmount`).*
- [ ] A resource group per environment as the pipelines expect: `rg-sac-<env>-eastus`. *Repo:
      `azure/pipelines/infra.yml` (the `RG=` value).*

## Phase 2 — repository blockers to fix before the first deploy

1. **Services run in-memory.** `azure/main.bicep` never passes `SAC_STORE`, and every image defaults
   `SAC_STORE=memory`; the production Dockerfiles also build **without** `-tags sac_sql_driver`, so
   `-store sql` refuses (no driver linked). Without both, nothing persists. *Repo: `azure/main.bicep`
   (app `env`), `control/control-api/Dockerfile`, `ingestion/ingest-api/Dockerfile`,
   `vault/content-vault/Dockerfile`.*
2. **Two images have no source.** The composition names eight; six have Dockerfiles. `reconciler` and
   `migrations` have none, so the migration job cannot apply the schema. *Repo: `azure/main.bicep`
   (job modules), `aggregation/aggregator/Dockerfile` and siblings.*
3. **No image build/push pipeline.** `imageTag` must already exist in the ACR. *Repo:
   `localdev/build.mjs` (lab images only), `azure/pipelines/` (no build pipeline).*
4. **Pipelines are not installed** — inert until copied under `.github/workflows/`. *Repo:
   `azure/pipelines/README.md`.*
5. **Migration before traffic.** `infra.yml` runs the migration *after* the deploy; §3.4 requires
   *before*, which also needs multi-revision mode. *Repo: `azure/pipelines/infra.yml`,
   `azure/modules/container-app.bicep` (`activeRevisionsMode`).*
6. **Device auth is product-issued x509 (Shape A).** control-api signs with the CA in
   `sac-device-ca-cert`/`sac-device-ca-key`; ingest-api verifies the same certificate. *Repo:
   `azure/main.bicep` (`ingestApp`/`controlApp`), `control/control-api/cmd/control-api/main.go`
   (`loadSigner`), `control/control-api/internal/signer/`.*

## Phase 3 — deployment identities (no stored credential)

- [ ] One **workload-federated deployment identity per environment**, scoped to repo/branch/environment.
      *Repo: `azure/pipelines/infra.yml` (`azure/login@v2` OIDC block).*
- [ ] A **separate ACR-push identity**. *Repo: `azure/pipelines/policy-scan.yml` (its note).*
- [ ] Record `AZURE_DEPLOY_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`,
      `SAC_PG_ADMIN_LOGIN` as GitHub `vars`. *Repo: `azure/README.md` §"exact commands".*

## Phase 4 — secrets and the vendor identity

- [ ] Create the Key Vault secrets in `<baseName>-kv`: session signing key (EC P-256), policy signing
      key (Ed25519), internal token, directory key. **Keep the policy key's public half** (the MSI pins
      it); never lose the directory key. *Repo: `azure/README.md` (commands), `azure/main.bicep`
      (`keyVaultEnv`/`keyVaultFiles` for `controlApp`).*
- [ ] Create the **product device CA** `sac-device-ca-cert` + `sac-device-ca-key`. *Repo:
      `azure/main.bicep` (`controlApp` `SAC_CA_CERT_PEM`/`SAC_CA_KEY_PEM`; `ingestApp`
      `SAC_TLS_CLIENT_CA_PEM`), `control/control-api/internal/signer/localca.go`.*
- [ ] Register the vendor multi-tenant Entra app (roles, redirect URIs, Graph
      `DeviceManagementManagedDevices.Read.All`). Set `entraAppClientId`. *Repo:
      `azure/main.bicep` (`SAC_ENTRA_CLIENT_ID`), `control/control-api/internal/entraapp/`,
      `control/control-api/internal/intune/`.*
- [ ] Add the federated credential from the `entraFederatedCredential` output. *Repo:
      `azure/main.bicep` (the `entraFederatedCredential` output comment has the command).*

## Phase 5 — DNS, certificates, and the Standard-origin wiring

- [ ] Device FQDN → a Key Vault certificate; pass `deviceTlsCertKeyVaultSecretId`. *Repo:
      `azure/modules/application-gateway.bicep`, `azure/main.bicep` (`deviceFqdn`,
      `deviceTlsCertKeyVaultSecretId`).*
- [ ] Analyst FQDN → a Front Door **Standard** custom domain (managed certificate is available). Set
      `publicUrl`. *Repo: `azure/modules/frontdoor.bicep`, `azure/main.bicep` (`publicUrl`).*
- [ ] **Standard-only:** make the analyst origins public — the environment's
      `internalLoadBalancer: false` and the Front Door origins pointing at the apps' public FQDNs, with
      the WAF policy on `Standard_AzureFrontDoor`. *(Was "approve the Private Link connection"; under
      Standard there is none.)* *Repo: `azure/modules/container-apps-env.bicep`,
      `azure/modules/frontdoor.bicep`, `azure/modules/waf.bicep`.*

## Phase 6 — first deployment

1. `az deployment group validate`. *Repo: `azure/README.md`; `azure/main.bicep`.*
2. `az deployment group what-if` — always read the diff. *Repo: `azure/README.md`; `azure/pipelines/drift.yml` for the nightly classification.*
3. `az deployment group create`. *Repo: `azure/README.md`; `azure/pipelines/infra.yml`.*
4. Push images on the tag the params name, then redeploy so revisions go healthy. *Repo:
   `azure/params/*.bicepparam` (`registryLoginServer`, `imageTag`).*

Only step 3 creates resources; steps 1–2 are free and turn the compile warnings and `BCP318` notices
into either "confirmed" or "a real error". Under Standard there is **no** Private Link approval step.

## Phase 7 — schema

- [ ] `postgres.bicep` creates no Entra administrator with `passwordAuth: Disabled`; create one, then
      apply `database/schema.sql` from **inside the VNet** (Phase 2.2: there is no `migrations` image).
      *Repo: `azure/modules/postgres.bicep`, `database/schema.sql`, `database/invariants.test.sql`.*
- [ ] Run the invariant suite as the runtime roles to prove RLS. *Repo: `database/invariants.test.sql`,
      `database/tools/`.*

## Phase 8 — verification (the part no static check can do)

- [ ] Every container app reports ready; `content-vault` ingress is `internal`. *Repo:
      `azure/main.bicep` (`contentVaultIngress` output), `azure/modules/container-app.bicep`.*
- [ ] PostgreSQL accepts only the VNet path; no firewall rules. *Repo: `azure/modules/postgres.bicep`,
      `azure/tools/check-infra.mjs`.*
- [ ] `GET /v1/health` through Application Gateway; a device enrols against the device FQDN. *Repo:
      `azure/modules/application-gateway.bicep`, `ingestion/ingest-api/internal/auth/`.*
- [ ] `control-api` sign-in returns a session and a product token. *Repo:
      `control/control-api/internal/identity/`, `internal/session/`.*
- [ ] **Standard-specific:** confirm the analyst origin is public and the WAF is in front of it — this
      is the deviation, and it should be *verified as deviating*, not assumed private. *Repo:
      `azure/modules/frontdoor.bicep`, `azure/modules/waf.bicep`, `azure/tools/check-infra.mjs`.*
- [ ] Log Analytics receives telemetry; the §10.3 alerts are live. *Repo: `azure/modules/monitoring.bicep`.*
- [ ] Re-run `what-if`: the only remaining diff is expected/noise, not drift. *Repo:
      `azure/pipelines/drift.yml`.*

## Phase 9 — staging, then production

Staging is production-shaped (contract suite, migration rehearsal, 500 events/s burst, WAF
false-positive pass); production adds the human approval gate. **Neither may inherit the Standard-origin
deviation** if the private-origin property is required in production. *Repo:
`docs/05-platform-delivery.md` §3.1, §4.4, §4.5.*

---

## Preflight checklist

- [ ] Phase 0 decisions recorded (region, **Front Door Standard + deviation**, DNS, first milestone) — `azure/params/`, `docs/05` §2.1
- [ ] Phase 1: subscription, providers, quotas, budgets, resource groups — `azure/README.md`
- [ ] Phase 2: `SAC_STORE=sql` + tagged images; `reconciler`/`migrations` images; image pipeline; workflows installed — `azure/main.bicep`, `*/Dockerfile`, `azure/pipelines/`
- [ ] Phase 3: federated deploy identity + separate push identity; GitHub vars — `azure/pipelines/infra.yml`
- [ ] Phase 4: Key Vault secrets incl. device CA; Entra app + federated credential — `azure/README.md`, `azure/main.bicep`
- [ ] Phase 5: DNS; device certificate; **Standard public origins + Standard WAF SKU** — `azure/modules/{frontdoor,waf,container-apps-env}.bicep`
- [ ] **Checker taught the deviation** so the internal-LB assertion is scoped, not deleted — `azure/tools/check-infra.mjs`
- [ ] `validate` and `what-if` reviewed; then `create` — `azure/README.md`
- [ ] Phase 7 schema applied and invariants run in-VNet — `database/`
- [ ] Phase 8 verification observed, including that the Standard origin is *public on purpose* — `azure/tools/`
