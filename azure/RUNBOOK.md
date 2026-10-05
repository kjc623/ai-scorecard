# azure/RUNBOOK.md — preflight and the go-live sequence

`README.md` says what is deployed and how to check it **without** Azure. This file is the operational
companion: the order a human sets an environment up in, the decisions that must be answered before the
first deployment, and the checks that say each phase is done. The exact `az` commands live in
`README.md` ("The exact commands a human would run"); this file does not repeat them, it says when to
run them and what must be true first.

**Nothing here has been run.** There is no Azure subscription, so every step below is a plan, not an
observation. The static suite (`node --test azure/tools/index.mjs`, 47 checks) proves the properties
are *declared*; only a deployment proves they are *effective*.

## The state on 2026-10-05

| Ready | Not ready |
|---|---|
| Bicep composition, 15 modules, 4 parameter files | No subscription; nothing has ever been created |
| 47 static checks (inventory, invariants, cost model) | No `validate`/`what-if` output has been seen |
| Edge, dashboard, exports switches for a cheap lab | Pipelines are inert: no `.github/` directory |
| Migration ordering fixed in `infra.yml`? **No** — see Phase 2 | No image build/push pipeline; two images have no source |
| Key Vault role separation now enforced (2026-10-05) | Four Key Vault secrets and the Entra app do not exist |
| Application Gateway API version now supports passthrough | No `SAC_PG_ADMIN_LOGIN` variable or federated identity |

## Phase 0 — decisions that must precede everything

These are product/security decisions, not engineering ones. Changing them later is expensive; changing
them after a production deployment is not possible without a migration.

1. **Residency region.** `az`/params assume `eastus`. One production environment per data-residency
   region; a second region is a second parameter file (`prod.<region>.bicepparam`). Q1 is still open.
2. **Shared or per-tenant edge.** The Front Door and Application Gateway fixed bases (~$330 and ~$321
   per month) are charged per tenant in `docs/05` §11.2 but are built once per region by `main.bicep`.
   The two readings differ by roughly a factor of 1.7 on the headline per-tenant figure
   (`COST-FINDING.md`, `cost-model.md` findings `FD-SHARED-OR-PER-TENANT` / `AGW-SHARED-OR-PER-TENANT`).
   Decide before quoting a price.
3. **First milestone.** Recommended: a throwaway resource group, deploy the `lab.bicepparam` shape
   (edge/dashboard/exports off) or `dev`, confirm `validate`/`what-if`/deploy and then delete it. This
   is the only way to close the `BCP318` notices and the Private Link / managed-identity / Container
   Apps behaviours the static checker cannot reach. It costs a few dollars, not the `$12,000/month`
   the production budget allows.
4. **DNS names.** `device.sac.example.com` and `app.sac.example.com` are placeholders. Choose the real
   names; the device FQDN needs a TLS certificate in Key Vault, the analyst FQDN needs a Front Door
   custom domain and its validation record.

## Phase 1 — subscription prerequisites

- [ ] Subscription, billing, and a region with the SKUs available (`D2ds_v5` vCores, `WAF_v2`,
      Front Door Premium are not available by default in every subscription/region).
- [ ] Register the 12 resource providers listed in `README.md` precondition 2.
- [ ] Cost approval for the target environment's budget (`params/*.bicepparam` set dev `$250`,
      staging `$900`, prod `$12,000`). Managed HSM is per-contract at ~$3,358/month.
- [ ] A resource group per environment, named as the pipelines expect:
      `rg-sac-dev-eastus`, `rg-sac-staging-eastus`, `rg-sac-prod-eastus`.

## Phase 2 — repository blockers to fix before the first deploy

1. **Two images have no source.** The composition names eight images
   (`ingest-api`, `control-api`, `content-vault`, `query-api`, `dashboard`, `aggregator`, `reconciler`,
   `migrations`); six have Dockerfiles. **`reconciler` and `migrations` have no command or Dockerfile
   anywhere in the repository** — those jobs will fail to start even after everything else is correct.
   Either build them or trim those two jobs from the first deployment.
2. **No image build/push pipeline.** Nothing builds or pushes to the per-environment ACRs
   (`sacdeveastusacr.azurecr.io`, …). `imageTag` must already exist in the registry or every revision
   is a failed revision. `localdev/build.mjs` builds *lab* images only, and refuses off its host.
3. **Pipelines are not installed.** `azure/pipelines/*.yml` are inert until copied to
   `.github/workflows/`; GitHub will not run a workflow from any other path. Wired or not is a repo
   decision (one source of truth vs. the current "stored beside the Bicep, installed by hand").
4. **Migration before traffic.** `infra.yml` runs `az deployment group create` first and starts the
   migration job afterwards; `docs/05` §3.4 requires the migration to run *before* the new revision
   takes traffic. Doing that correctly also needs multi-revision mode — `container-app.bicep` is
   `activeRevisionsMode: 'Single'`, so there is no revision to hold back and no canary (§4.4).
5. **The device credential is product-issued (Shape A).** `control-api` signs x509 device leaves from
   the CA in `sac-device-ca-cert` / `sac-device-ca-key`, and `ingest-api` re-verifies the forwarded
   leaf against `sac-device-ca-cert`; both are now wired, so x509 enrolment works end to end with no
   customer PKI. **They must be the same CA**, or every device is refused at the origin. The optional
   DPoP mode is not wired (it needs `SAC_DPOP_TOKEN_KEY_PEM` on control-api and the verification key
   on ingest). `control-api` signs with the CA key as a Key Vault secret in-process rather than in the
   HSM — a deviation from §5.3's "sign in the vault" note, acceptable for a pre-prod and recorded here.

## Phase 3 — deployment identities (no stored credential)

- [ ] One **workload-federated deployment identity per environment**, subject-scoped to this
      repository, branch pattern and GitHub environment (`.github` environment protection rules carry
      the human approval for prod, §4.5). Its only rights are resource-group deployment.
- [ ] A **separate ACR-push identity**. Pushing an image and deploying it are different privileges.
- [ ] Record `AZURE_DEPLOY_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`,
      `SAC_PG_ADMIN_LOGIN` as GitHub `vars` per environment (never secrets — a login name is not a
      credential, because `passwordAuth` is disabled).

## Phase 4 — secrets and the vendor identity

- [ ] Create the four Key Vault secrets in `<baseName>-kv` (commands in `README.md`): the session
      signing key (EC P-256), the policy signing key (Ed25519), the internal token, and the directory
      key. **Keep the policy key's public half** — the agent MSI pins it — and never lose the
      directory key, or every tenant's user-reference key is unreadable.
- [ ] Create the **product device CA** as `sac-device-ca-cert` (the certificate) and `sac-device-ca-key`
      (its private key). `control-api` signs device leaves with both; `ingest-api` re-verifies them with
      the certificate. Generate once and keep the pair; rotating the CA invalidates every enrolled
      credential until devices re-enrol.
- [ ] Register the vendor multi-tenant Entra app (app roles `viewer`/`analyst`/`content_reader`/`admin`,
      redirect URIs `{SAC_PUBLIC_URL}/callback` and `{SAC_PUBLIC_URL}/onboard/entra/callback`,
      delegated `openid profile email offline_access`, application
      `DeviceManagementManagedDevices.Read.All`). Set `entraAppClientId`.
- [ ] Add the federated credential from the `entraFederatedCredential` output on that app, so
      `control-api` authenticates as its managed identity with no secret. Until it exists, Entra
      sign-in and the Intune check do not work; OIDC customers are unaffected.

## Phase 5 — DNS and certificates

- [ ] Device FQDN → a Key Vault certificate; pass `deviceTlsCertKeyVaultSecretId` at deploy time.
- [ ] Analyst FQDN (Front Door custom domain) → validation TXT/CNAME; set `publicUrl`.
- [ ] Approve the Front Door Private Link connection to the Container Apps environment after the
      deployment; it is created pending approval on purpose (`README.md` step 4).

## Phase 6 — first deployment

1. `az deployment group validate` (needs the Key Vault secrets to exist; see `README.md`).
2. `az deployment group what-if` — always read the diff.
3. `az deployment group create`.
4. Push the images with the tag the params name, then redeploy (or let revisions pull) so the apps go
   healthy.

Only step 3 creates resources; steps 1–2 are free and are what turn the 32 compile warnings and the
`BCP318` notices into either "confirmed" or "a real error".

## Phase 7 — verification (the part no static check can do)

- [ ] Every container app reports ready; `content-vault` ingress is `internal`.
- [ ] PostgreSQL accepts only the VNet path; no firewall rules; private DNS resolves.
- [ ] `GET /v1/health` through Application Gateway; a device enrols against the device FQDN.
- [ ] `control-api` sign-in returns a session and a product token.
- [ ] The migration job ran and the schema matches `database/schema.sql`.
- [ ] Log Analytics receives telemetry; the §10.3 alerts are live (`alertCount` output).
- [ ] Re-run `az deployment group what-if`: the only remaining diff is expected/noise, not drift.

## Phase 8 — staging, then production

Staging is production-shaped with a full contract suite, migration rehearsal, a 500 events/s burst and
a WAF false-positive pass (`docs/05` §3.1). Production adds the human approval gate and the staged
traffic shift (§4.4) — which does not exist yet (Phase 2.4). After production: install `drift.yml` and
close the resource-graph half of §3.5, which is still a documented gap.

## Preflight checklist

- [ ] Phase 0 decisions recorded (region, edge model, DNS, first milestone)
- [ ] Phase 1: subscription, providers, quotas, budgets, resource groups
- [ ] Phase 2: `reconciler`/`migrations` images exist; image pipeline; workflows installed; migration
      ordering / revision mode decided
- [ ] Phase 3: per-environment federated deploy identity + separate push identity; GitHub vars
- [ ] Phase 4: four Key Vault secrets; Entra app + federated credential
- [ ] Phase 5: DNS records and the device certificate
- [ ] `validate` and `what-if` reviewed; then `create`
- [ ] Phase 7 verification observed and recorded
