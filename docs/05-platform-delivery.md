# 05 — Platform Delivery

How Shadow AI Capture is deployed, updated, observed and kept running. This document exists because
the product has **two deployment problems that share no mechanism**: a cloud service on Azure, and a
fleet of endpoint components on machines the vendor does not own and the customer only partially
manages. The second is harder, and §6–§9 are the reason.

Everything here traces to a numbered section of the brief (*Shadow AI Capture — Product Requirements
& Engineering Context*), to a decision in the master document [00-architecture](00-architecture.md)
(`D1`–`D8`, `C1`–`C36`, `E1`–`E25`, `Q1`–`Q13`), or carries an **ASSUMPTION** label with a one-line
justification. Azure is fixed (master §2.4 S1). Prices are estimates; see §11 for the basis.

---

## 1. Scope

### 1.1 Two deployment problems

| | Cloud tier | Endpoint fleet |
|---|---|---|
| **What is deployed** | `ingest-api`, `control-api`, `content-vault`, `query-api`, `aggregator`, `reconciler`, dashboard, database, storage, keys (master §4.1) | `capture-extension` (Chromium MV3), `capture-core` (privileged service), `classifier-host` + document parser, policy bundle, classifier content, root certificate |
| **Who owns the machine** | Vendor | Customer's employee; the customer's MDM reaches 70–85% of the managed fleet and nothing else (brief §5.5, E24) |
| **Rollback** | Redeploy the previous revision or job image | Removal is not a redeploy: a bad shim leaves a machine with no local AI, a bad proxy leaves a machine with no network |
| **Blast radius of a defect** | One tenant's ingest or dashboard | Every user on every device in the ring; the egress proxy carries the largest blast radius in the product (brief §5.5) |
| **Update authority** | Vendor, continuously | Vendor signs, **the customer's policy deploys**. The vendor cannot push, cannot verify, and cannot retract |
| **Verification** | Health probes, synthetic transactions, SLO burn | Health reporting only (C23); the vendor has no shell on the endpoint, by design |

### 1.2 The consequences this document is written around

**The endpoint cannot be assumed reachable or current.** A device may run a version from three rings ago,
or none, so every server-side contract must accept an N-2 client for a stated window (§9.6): there is no
forced upgrade. **Silence is not evidence.** A device that stopped reporting looks identical to one whose
owner took it home for a month; coverage, spool depth, drop counters and liveness are the product's
honesty layer (C22–C25, R11) and also the fleet's operating system (§10). **Going faster requires being
able to go backwards** — brief §5.5 makes rings with automatic halt, signed update manifests, content/code
separation, atomic install with rollback and a server-side kill switch hard expectations, not maturity
goals, and §9 discharges them. And **the vendor's commercial risk is concentrated in two dates and one
artefact**: the signing-certificate expiry (brief §5.5, §7), the end of the reputation bake period
(brief §5.5, §8), and the exclusion deliverable other endpoint security products need (R4, §8).

**ASSUMPTION:** the vendor operates the cloud tier itself in a multi-tenant deployment; per-tenant
single-tenant deployments are not offered in v1. *Justification:* C33's customer-held keys are
delivered by per-tenant keys inside a shared control plane, which is where `per-tenant` and
"one customer in ten will want to run their own SQL" (master §2.4 S3) both point, and a per-tenant
deployment per customer is not operable by the team of two that master §3 assumes.

---

## 2. Azure resource inventory

One **regional production environment per data-residency region**. The inventory below is one region;
the resources marked region-shared are deployed once per region, the rest are shared across all tenants
**in** that region. Residency is pinned per tenant at ingest (Q1, §3.1).

| Resource | SKU / tier | Why this tier (and which master §4.1 component it hosts) |
|---|---|---|
| **Application Gateway `WAF_v2`** + WAF policy | `WAF_v2`; WAF in Prevention, managed rule set plus custom rules; listener in **passthrough** mode | The public device ingress (ADR 0020 decision 1). GA client-certificate authentication; it terminates the device TLS connection and reaches the internal Container Apps environment over the VNet. Passthrough forwards the client certificate and the origin authenticates; strict mode is optional for certificate-only tenants. WAF must block at the edge, not in the app, because `ingest-api` accepts traffic from machines the vendor does not control. Public entry for `/v1/enrol`, `/v1/token`, `/v1/policy`, `/v1/events`, `/v1/health`, `/v1/content/grant` |
| **Front Door Premium** + WAF policy | Premium; WAF in Prevention, managed rule set plus custom rules | **Browser and identity-provider ingress only** (ADR 0020 decision 1). Premium is required for the Private Link origin that keeps Container Apps off the public internet; Standard cannot use Private Link. Routes: `/analyst/*` → `query-api`; `/scim/v2/*`, `/onboard/*`, `/.well-known/*` → `control-api` (a customer's SCIM client, the one-time onboarding pages, the product token issuer's JWKS); everything else → the `dashboard` server. No route reaches `content-vault` |
| **Container Apps environment** | Consumption-only, no workload profiles, VNet-injected, internal load balancer | Peak demand is ~50–500 events/s during a fleet flush (master §1.4) against ~0.14 events/s steady. Consumption costs nothing when idle, which is most of the day; a dedicated workload profile would bill for idle capacity. Network boundary for all five apps |
| **Container App `ingest-api`** | Min 2, max 20; 0.5 vCPU / 1 GiB; HTTP/2 ingress transport (`transport: 'http2'`, as on every app); external via Private Link origin | Two replicas is the availability floor for 99.9% (brief §8); the ceiling covers a flush storm, which is bursty by nature. Validates and writes; it never decrypts |
| **Container App `control-api`** | Min 2, max 10; 0.5 vCPU / 1 GiB | Enrolment, policy signing, health upsert, grant decisions, and the product's identity service: sign-in for every customer identity provider, sessions, product access tokens, onboarding, SCIM provisioning and the admin API ([06](06-security-and-threat-model.md) §4.1). Policy signing is CPU-cheap — it signs a bundle, not an event. Its managed identity is also the credential of the vendor's Entra application, as a federated identity credential (the composition's `entraFederatedCredential` output), so no Entra secret is stored |
| **Container App `content-vault`** | Min 1, max 4; 1 vCPU / 2 GiB; **internal ingress only** | The only component that can unwrap content keys (C15, D7), and internal ingress is the structural control: neither devices nor browsers can reach it. Larger memory because it streams ciphertext. Its identity holds Storage Blob Data Reader on the ciphertext account, which is how it reads stored ciphertext to serve a retrieval |
| **Container App `query-api`** | Min 2, max 10; 0.5 vCPU / 1 GiB | Serves the dashboard and the export path; scales on request concurrency and reads aggregates, so it is I/O-light |
| **Container Apps Job `aggregator`** | Scheduled (cron), one replica, manual start available | Recomputes each bucket from `ingest.submission` over a lookback window and **replaces** it (C27, C28). One replica is correct: the job is set-based SQL and concurrency would contend on the same buckets |
| **Container Apps Job `reconciler`** | Scheduled (cron), one replica | The second and independent expiry mechanism, the drift detector between the two (C34), and dedup/route reconciliation (R9) |
| **Container Apps Job `migration`** | Scheduled trigger used only as a placeholder (annual cron); started manually by the pipeline; one replica; its own identity | Schema migrations (§3.4). A third instantiation of the job module with a different identity class (§5.4) |
| **Azure Database for PostgreSQL Flexible Server** 16 | General Purpose `D2ds_v5`, 2 vCores / 8 GiB, **zone-redundant HA**, 128 GiB, PITR 35 days, `publicNetworkAccess = Disabled`, VNet-injected into a delegated subnet (no private endpoint) | Master §1.4: four years of a full tenant is under 18M rows and 40 GB, so one well-indexed relational database suffices and v1 does not partition (D2). Burstable is rejected in production because the aggregator's set-based scans would exhaust credits and stall ingest freshness. Zone-redundant HA is the availability floor; geo-redundant backup answers regional failure (§12). Hosts `ingest`, `ops`, `mart`, `ref` (master §5.3) |
| **Storage account — ciphertext** | StorageV2 (general-purpose v2), **RA-GRS**, versioning on, soft delete 30 days, lifecycle per §12, no public blob access, no shared-key access | Holds M3 attachment ciphertext; the vendor stores ciphertext only (C15). Shared-key access is disabled because every access path is a managed identity. Referenced by `ops.content_object` |
| **Storage account — exports** | StorageV2 (general-purpose v2), ZRS, lifecycle to cool at 30 days | The scheduled columnar export to the customer's own storage (C31, Q11). A separate account because its retention and access model is the customer's, not the product's |
| **Key Vault** | Premium (HSM-backed keys, FIPS 140-2 Level 2), RBAC authorization, purge protection, soft delete 90 days, private endpoint | Per-tenant KEKs wrapping per-object content keys, TLS certificates, policy-bundle signing keys (C15, C33), and the identity service's secrets: the session-signing and policy-signing keys (mounted into `control-api` as files), the directory key that seals identity and directory columns, and the internal token the `dashboard` server presents to `control-api`. Premium rather than Standard because KEKs must be HSM-backed |
| **Managed HSM** | One pool, 2-of-3 quorum, only where a contract requires it | Brief §3.3 requires customer-held keys for some buyers, and "the vendor cannot read content at all" is a contractual statement that wants FIPS 140-2 Level 3 and a quorum the vendor alone cannot satisfy. At ~$4.6/hour it is **per-contract, not per-region-by-default** |
| **Container App `dashboard`** | Min as the APIs, max 10 (2 in dev); 0.5 vCPU / 1 GiB | The pages and a thin server that holds the sign-in session, signs people in through `control-api`, and forwards `/v1/*` to `query-api`, `/admin/v1/*` to `control-api` and a minted retrieval URL to `content-vault`, inside the environment. It replaces the Static Web App the design first named: a static host holds no session and cannot reach an internal app, and its own Entra sign-in would be a second, unconnected sign-in path. Its identity reads one Key Vault secret (the internal token) and holds no database role and no content key |
| **Container Registry** | Premium, private endpoint, geo-replication to the paired region | Images are pulled over Private Link; geo-replication is what makes regional failover a redeploy rather than a rebuild (§12) |
| **Log Analytics workspace** + Application Insights | Pay-as-you-go, 90-day interactive retention, 12-month archive, workspace-based App Insights | 90 days interactive covers an incident's investigation window; the archive covers the audit and reconciliation questions that arrive later. Serves SLO computation, alerts and the §14 evidence |
| **VNet, subnets, private endpoints, Private DNS zones** | One VNet; delegated Container Apps subnet, private-endpoint subnet, a subnet reserved for a DNS resolver (no resolver resource is declared in it); one private endpoint per PaaS resource that takes one (both storage accounts, Key Vault, Managed HSM, ACR, Log Analytics); PostgreSQL joins the VNet by delegated-subnet injection instead of a private endpoint; zones linked to the VNet | VNet injection is what makes `content-vault`'s internal-only ingress meaningful, and linked private DNS zones are what stop resolution falling back to a public answer (Q12's connection ceiling is a separate concern) |
| **Azure Monitor alerts, action groups, budgets** | Scheduled-query (log-search) alerts; one action group per severity; one budget per environment, scoped to its resource group | The alert set in §10.3 is only real if it pages someone, and the attachment tier is the only unbounded cost (brief §3.1) — a cost-anomaly alert is cheaper than a monthly surprise |

### 2.1 Networking, stated once

Inbound, devices: device → Application Gateway `WAF_v2` (public 443) → VNet → Container Apps
environment (internal LB). Inbound, analysts: browser → Front Door Premium (public 443) → WAF →
Private Link origin → Container Apps environment (internal LB), authenticated by `control-api`'s
sign-in against the customer's identity provider. The two
audiences have two public edges, and neither reaches a container app directly: Application Gateway is
the only public device endpoint, and Front Door is the only public analyst endpoint. Egress: container
apps → subnet → private endpoint → PaaS, except PostgreSQL, which is reached at its VNet-injected
address in a delegated subnet rather than through a private endpoint. No container app holds a public
IP — it is reachable only through the internal load balancer, from the gateway subnet on the device
path and from the Private Link origin on the analyst path — and no PaaS resource accepts a public
connection; the PostgreSQL server additionally has no firewall rules at all, so "reachable from the
internet" is not a configuration state it can be put into.

### 2.2 Deliberately absent

| Not deployed | Why, and the consequence accepted |
|---|---|
| **Message broker** (Service Bus, Event Hubs, Kafka) | At ~0.14 events/s steady and 50–500 events/s at flush, `ingest-api` writes directly to PostgreSQL; a broker would add a hop, a second failure domain, a dead-letter queue nobody reads and a per-message cost, to absorb a burst a 2-vCPU Postgres already absorbs. Consequence: ingest is coupled to the database's availability, accepted because devices spool and retry (C22), so a database blip is a delay rather than a loss — and C13 makes *aggregation*, not ingest, the asynchronous part |
| **Search cluster** (Azure AI Search, Elastic) | Rejected on volume rather than on principle: prompt text is ~4.4 GB/year per tenant, so `ingest.search_text` in the existing PostgreSQL instance serves the requirement (ADR 0014). A second copy of plaintext content in another system would widen the breach surface, add a second retention path to reconcile, and cost more than the rest of the tenant combined. **New provisioning dependency:** that table needs the `pg_trgm` and `btree_gin` extensions, so both must be confirmed on the Flexible Server allow-list for each target region before provisioning (master doc Q12). Consequence: content search is capped by what one PostgreSQL instance can index, which at this volume is not a constraint |
| **Data warehouse / columnar store** (Synapse, Databricks, Fabric) | 5–9 GB/year of events (brief §3.1); a warehouse would cost more per month than the entire tenant, to query 4.4M rows a year. Consequence: heavy analytical questions are answered by the export path, not in-product |
| **Read replica** | Aggregate queries read `mart`, which is small and precomputed, and one primary is the source of truth; a replica adds a second consistency model to explain. Consequence: dashboard reads share the primary — revisit if `mart` latency degrades (§10.3 alert) |
| **Kubernetes / AKS** | Four small Go/Node services and three jobs do not justify a cluster, a node-pool rotation policy or a CNI plugin to patch. Consequence: less control over node-level tuning, accepted |
| **Kernel-mode or host-based inspection infrastructure** | Forbidden by the design (D3, C9). Consequence: E8's coverage boundary is a product feature, measured per provider (§10.2) |
| **Reserved capacity or savings plan** | Not in v1: reservations are a pricing decision taken once the fleet is real, and every figure in §11 is list price with no committed-use discount. Consequence: cost is 20–40% higher than it needs to be. Stated, not hidden |
| **A broker for policy distribution** | Policy is a signed, versioned, ETag-able document (C10) served by `control-api`; a device polling a URL is the whole mechanism. Consequence: the poll interval is the kill switch's latency (§9.5) |
| **Vendor-operated MDM** | Brief §5.6: EDR and MDM platforms permit no in-sensor injection and expose no content, so the product must ship its own collection and distribution rides the *customer's* Intune or Jamf (§6). Consequence: 70–85% management coverage is the planning figure, never 100% (brief §5.5) |

---

## 3. Environments and infrastructure as code

### 3.1 The environment ladder

| Environment | Purpose | Data | Database | Gate to promote out |
|---|---|---|---|---|
| **dev** | Feature work; breakable | Synthetic, generated | `B2s`, 32 GiB, no HA, single zone | Automated tests green; no manual gate |
| **staging** | Promotion gate and rehearsal | Synthetic, production-shaped volume and skew | Same SKU as production, no HA, 64 GiB | Full contract test suite, migration rehearsal against a production-shaped row count, load test at 500 events/s burst, WAF false-positive pass |
| **production / region** | Live tenancy for one residency region | Regulated personal data | `D2ds_v5` + zone-redundant HA | Change approval (§4.4) plus a green staging run of the same commit |

**One production environment per residency region**, not one global production. Q1 is unresolved, and
master §7 is explicit that "a region column and a fail-closed check are cheap now and expensive later".
This document therefore assumes the deployment *shape* supports per-region production from day one even
if only one region is live initially. **ASSUMPTION:** v1 launches in a single region (East US) with the
ladder built to be instantiated per region. *Justification:* the brief and master doc both leave
residency open (Q1, S5); building the ladder as a per-region instantiation is the cheap half of that
decision, and choosing the launch region is a product decision this document should not make.

**ASSUMPTION:** each environment occupies its own resource group and its own managed identities, and no
principal holds a role in more than one environment. *Justification:* the deployment identity is
separated from the runtime identity in §5.4; that separation is only meaningful if it also holds across
environments, otherwise a staging deploy can reach production keys.

### 3.2 What is environment-parameterised

The module templates are identical in every environment; only the parameter file differs. Parameterised:
region (one each for dev and staging; one per residency region in production); container app min/max
replicas (1/2 dev; 2/5 staging for `ingest-api` and 2/10 for `control-api` and `query-api`, whose ceiling
the composition sets to 10 everywhere outside dev; per §2 in production); PostgreSQL SKU, storage, HA and
PITR days (`B2s`, 32 GiB, no HA, 7 d / production SKU, 64 GiB, no HA, 14 d / `D2ds_v5`, 128 GiB,
zone-redundant, 35 d); Key Vault versus Managed HSM (vault only in dev and staging; HSM per contract in
production); ciphertext blob redundancy (LRS / ZRS / RA-GRS); WAF mode (Detection / Prevention /
Prevention — the mode is the only WAF parameter the composition passes, so the managed rule set and the
custom rules, including the per-client rate limit, are declared identically in every environment); Log
Analytics sampling and retention (10% traces, 30 d, no archive / 10% traces, 90 d plus a 12-month
archive / the same); alert routing (none / non-paging channel / on-call, §10.3); and ring assignment
(n/a / internal ring 0 / the full ladder, §9.2).

The composition also carries three deployment-scope switches — `deployEdge` (the edge: Front Door and its
WAF today, and the Application Gateway ADR 0020 adds), `deployDashboard` (the dashboard server) and
`deployExports` (the exports storage account) — each defaulting to true. `params/lab.bicepparam` sets all three false, so the lab is a parameter file over
the same composition rather than a second one; it is the one case in which the resource set, and not
only the sizing, differs by parameter file.

**Never parameterised:** table names and schemas; the event envelope contract; signing algorithm and
key purposes; tenant isolation and row-level security; the set of environment variables a service
validates at boot.

### 3.3 Bicep layout and module boundaries

Master §4.1 fixes Bicep. One composition, instantiated per environment by its parameter file; modules
that own one resource family each; and **no module reaches into another module's resources with
`existing`** — wiring happens in the composition file, so a module's inputs are visible in one place.
The one piece of wiring that is not in the composition is the private endpoint of the Key Vault, the
registry and the two storage accounts: each of those modules instantiates `private-endpoints.bicep` for
its own resource, and the composition calls that module directly only for Log Analytics and Managed HSM.

```
azure/
  main.bicep                     the composition; one file for every environment, thin
  modules/
    network.bicep                VNet, subnets (incl. one reserved for a DNS resolver), one NSG,
                                 private DNS zones and their VNet links
    private-endpoints.bicep      one private endpoint + DNS zone group per target
    log-analytics.bicep          workspace, App Insights, diagnostic settings, retention
    registry.bicep               ACR Premium, optional geo-replication, its private endpoint
    postgres.bicep               Flexible Server (VNet-injected), databases, server parameters, HA,
                                 PITR; schemas and roles are created by migrations, not here
    storage-ciphertext.bicep     StorageV2, RA-GRS, versioning, lifecycle, no shared-key access,
                                 its private endpoint
    storage-exports.bicep        StorageV2, ZRS, lifecycle, its private endpoint
    keyvault.bicep               vault, purge protection, RBAC role assignments, its private endpoint
    managed-hsm.bicep            conditional: only instantiated for HSM tenants
    container-apps-env.bicep     environment, VNet injection, internal LB, log destination
    container-app.bicep          one app: identity, secrets refs, probes, scale rules
    container-app-job.bicep      one scheduled job: cron, timeout, retry, identity
    application-gateway.bicep    WAF_v2, listener in passthrough, rewrite to X-Client-Cert, backend pool
                                 (ADR 0020; the module is not built yet)
    frontdoor.bicep              analyst profile, endpoint, origin group, Private Link origin, routes
    waf.bicep                    policy, managed rule set, custom rules, rate limits
    static-web-app.bicep         no longer instantiated: the dashboard is a container app
                                 (container-app.bicep) since sign-in moved to control-api
    monitoring.bicep             action groups and scheduled-query alert rules (the §10.3 set)
    budget.bicep                 one budget, scoped to the environment's resource group
  params/
    dev.bicepparam  staging.bicepparam  prod.<region>.bicepparam  lab.bicepparam
  pipelines/
    infra.yml  drift.yml  policy-scan.yml
  tools/
    check-infra.mjs  check-infra.test.mjs  index.mjs    the static checker the pipelines run
```

Module boundaries follow **trust and lifecycle**, not nouns:

`container-app.bicep` is one module parameterised by identity, scale and ingress, because the five apps
differ in exactly those and duplicating the module five times is how their identities drift.
`content-vault`'s internal-only ingress is a **composition-level assertion**, not a default: every
environment's composition file must pass `ingress: internal`, and a CI policy check fails the build if it
is instantiated with external ingress (D7). Key material has its own module, because the key *policies*
are the thing that gets reviewed, not the vault.

### 3.4 Schema migrations

Migrations are a separate pipeline with a separate identity (§5.4), because "can deploy code" and "can
alter the schema" are different privileges. **Ordering:** the migration runs as a Container Apps Job
*before* the new revision receives traffic — a migration that cannot run before the new code is a
migration that is wrong. **As built:** `azure/pipelines/infra.yml` has the two steps in the other
order — its `deploy` job runs `az deployment group create` first and starts the `migration` job, and
waits for it, afterwards — and the apps run in single-revision mode (§4.4), so the pipeline as written
does not hold a new revision back from traffic until the migration has finished. **Compatibility:** every migration must be compatible with the **N-1 application
version**, because revision rollback (§4.4) returns traffic to N-1 without reverting the schema; that is
the mechanical reason migrations are additive-first (expand, backfill, contract) rather than a style
preference. **Never reverted:** there are no down-migrations — a defective migration is fixed forward or
repaired by restoring into a side database and copying rows back, and §12.4's restore drill is its
rehearsal. **Timeouts:** `lock_timeout` and `statement_timeout` are set on the migration session so a DDL
lock cannot stall `ingest-api`'s writes; ingest must not depend on aggregation latency (C13) and must not
depend on migration latency either. **Evidence:** the job writes version, duration and row counts to the
audit trail, and the pipeline fails on a non-zero exit code — a migration that "ran" is not a migration
that succeeded.

### 3.5 Drift detection

Two mechanisms, because one of them is always asleep. **Scheduled `what-if`**, nightly per environment
against the same parameter file as the last deployment: each diff is classified *expected* (a value Bicep
reads back, a service-assigned property), *noise* (case, ordering) or **drift** — anything touching
network, identity, encryption or ingress. Drift pages on-call, and the report lands in the platform
channel either way. **Continuous resource-graph assertions**, hourly, queried from Azure Resource Graph
against resource state rather than against the deployment that was supposed to produce it: no PaaS
resource with `publicNetworkAccess` enabled; no storage account with shared-key access enabled; **the
only public device endpoint is Application Gateway; the container environment stays private** (no
container app has external ingress, and Front Door's origin is the Private Link endpoint);
`content-vault` ingress internal; every container app has a user-assigned identity; every Key Vault has
purge protection. These are the invariants whose failure is a security event rather than a configuration
event.

**As built:** only the first mechanism exists. `azure/pipelines/drift.yml` runs the nightly `what-if`
for dev, staging and production; on a drift-class diff it fails the job with an error annotation and
uploads the report as a build artefact kept for 90 days. It does not itself page anyone or post to a
channel — paging depends on whatever is attached to a failed run. The hourly resource-graph assertions
are not implemented; `azure/README.md` ("Gaps") and `azure/pipelines/README.md` record that. The static
checker in `azure/tools/` asserts the same properties against the templates on every pull request that
touches `azure/`,
which covers what is declared and not what is deployed.

Portal changes are permitted only during a declared incident, and must be reverted by pull request
within 24 hours. A portal change that is not reverted becomes drift on the next nightly run and pages
someone, which is the intended outcome.

---

## 4. CI/CD

### 4.1 Four pipelines, four artefacts, four risk profiles

The endpoint can be bricked; the server cannot. The pipelines are separate because they have different
gates, different blast radii, and different rollback semantics.

| Pipeline | Artefact | Trigger | Gates | Rollback |
|---|---|---|---|---|
| **Server tier** | Container images for the four apps and three jobs (aggregator, reconciler, migration); Bicep for infrastructure | Merge to `main`; manual promotion | §4.2 | Revision rollback (traffic) or image pin; infrastructure by prior parameter-file commit |
| **Classifier artefacts** | Rules bundle, model artefact, evaluation report — signed content, **not** code | Manual, on a candidate tag | §4.3 | Promote the previous release; no code ships (§9.5, C20) |
| **Browser extension** | Signed MV3 package for Chrome and Edge | Manual, on a candidate tag | §4.2 plus store review; store is the delivery channel | Version pin and `max_version` policy; the previous version cannot be un-published (§6.3) |
| **Desktop agent** | MSI (Windows), signed PKG (macOS), and a signed Linux package for `capture-core` + `classifier-host` + parser | Manual, on a candidate tag | §4.2 plus ring promotion (§9.2) | Ring-level supersedence to the previous MSI/PKG/package; atomic install rollback on the device (§9.4) |

### 4.2 The contract-generation step, shared by everything

`contracts/event-envelope.schema.json` is the sole source of field names (master §5.2). TypeScript and
Go types are **generated** from it, never hand-written, and the generation is a build step with a
staleness check:

1. **Generate.** JSON Schema 2020-12 → TypeScript (for `capture-extension`, `query-api`, `dashboard`)
   and Go (for `capture-core`, `ingest-api`, `control-api`, `content-vault`, jobs).
2. **Staleness gate.** CI regenerates into a temp tree and diffs against the committed generated files.
   A non-empty diff **fails the build**, with the instruction to re-run generation. This is the check
   that stops a field being added in one language and forgotten in another — the failure mode that
   produces a collector emitting a field the server silently drops.
3. **Fixture validation gate.** A fixture corpus (`contracts/fixtures/` — not in the repository yet:
   `contracts/` holds the schema, `generated/` and `tools/` only) holds at least one valid
   example per `kind` × `collection_mode` combination, plus deliberately invalid ones: an M0 record
   carrying `labels` or `content_digest` (evidence the device read content it was not permitted to
   read — §5.2 of the schema), an M3 record carrying `content_excerpt` (content in the envelope, which
   M3's path forbids), a `usage_rollup` carrying labels (the R7 leak the closed `kind` registry exists to
   prevent), a device submission carrying `received_at` (which would break the two-clock rule, C26), and
   a `kind` outside the registry. CI runs every fixture through the validator and **fails if any invalid
   fixture validates** — a validator that accepts everything passes the positive tests and fails the
   product.
4. **Round-trip gate.** Go and TypeScript types must marshal and unmarshal the same fixture to
   byte-identical canonical JSON, which is also how the dedup normalisation contract (Q3) is held.

This step is milestone 0 in master §6, and it is a prerequisite for every other pipeline: a component
that cannot demonstrate contract conformance does not get built, let alone deployed.

### 4.3 Classifier-artefact pipeline

Rules and models are promotable without a software release, evaluated in a non-enforcing mode before
they take effect, and reversible instantly (C20, brief §6). The pipeline is where "evaluated first" is
made mechanical:

1. Build rules bundle and model artefact; version both (`classifier_version`, e.g. `2026.10.1`).
2. Run the **evaluation set** built from consented pilot data (Q7, R6). Report per-class precision and
   recall, and the p95 classification latency (brief §8: ≤150 ms interactive).
3. **Shadow mode**: publish to the internal ring and to opt-in pilot tenants with
   `enforcement: shadow`. Labels are produced and stored with the candidate version; nothing blocks.
   Compare label distributions against the incumbent version on the same traffic (this is exactly why
   `classifier_version` is on every event).
4. Promote to enforcing **per ring**, never fleet-wide in one step. The release record carries the ring
   scope and the actor.
5. Every promotion and every reversal writes an audit entry (C4's discipline applied to content).

A bad release is reversed by publishing the previous version as the active one and flipping the
candidate to shadow: a content-lane operation with minutes of latency, no binary, no store review, no
ring. That is §9.5.

### 4.4 Server-tier gates, and how a change reaches production

**Build and unit tests** (failing test, `-race` failure in Go, TypeScript type error). **Contract
conformance** (§4.2 steps 2–4). **Static analysis and dependency scan** (a new high-severity finding on a
reachable path; a waiver needs an owner and an expiry date). **Image scan** (any critical vulnerability
with a fix available). **SBOM and signing** (deployment verifies the signature and refuses an unsigned
image). **Migration dry run** (must apply cleanly to a staging clone at production shape, and must be
N-1 compatible). **Staging verification** (WAF false-positive pass, burst load test at 500 events/s, SLO
metrics inside the §10 targets for 30 minutes). And **human approval**, required for production — §4.5
states where.

Promotion path: merge → build once → staging → automated verification → **human approval** → production
revision at 0% traffic → 10% → 50% → 100%, with automatic return to the previous revision if error rate
or p95 latency regresses beyond the staging baseline at any step. Deployment identity is a workload-
federated CI identity (§5.2), scoped per environment.

**As built:** the staged shift is not implemented. `azure/modules/container-app.bicep` sets
`activeRevisionsMode: 'Single'` with `latestRevision: true` at weight 100, so a new revision takes all
traffic as it becomes active and no previous revision is kept to return to. `azure/pipelines/` holds
the infrastructure, drift and policy-scan pipelines only; no pipeline there promotes an application
revision or weights traffic.

### 4.5 Where human approval is required

| Change | Approver | Why human |
|---|---|---|
| Production infrastructure (network, identity, keys, database parameters) | Platform lead | These are the changes a mistake cannot be rolled back quickly, and they are the ones an attacker would make |
| Production application promotion | Release approver on call | Automated gates cover correctness, not intent |
| **Any schema migration** | Platform lead **and** data owner | §3.4: migrations are not reverted |
| **Classifier promotion to enforcing** | Classification lead **and** a product owner | A classifier that blocks prompts is a fleet-wide incident (brief §6); enforcement is a product decision as much as a technical one |
| **Kill-switch activation** | Any on-call engineer, unilaterally | Deliberately *not* gated: the mechanism exists to be used in minutes, and its use is audited after the fact (§9.5) |
| **Client-compatibility window narrowing** (N-2 → N-1) | Platform lead | Once the server stops accepting N-2 clients, every device still on N-2 stops reporting and shows as a coverage gap |
| **Signing-certificate rotation** | Release engineering lead | It touches the artefact that can brick the fleet (§7) |
| **Tenant offboarding** | Two-person rule; the second approver is not the requester | §13: key destruction is irreversible |
| HSM key policy or quorum change | Security lead | Quorum change alters who can decrypt |

---

## 5. Identity and secrets

### 5.1 No stored credentials, anywhere

Every service runs as a **user-assigned managed identity**, and the grant is the security boundary rather
than the service's own code. `ingest-api` holds `INSERT` on `ingest.*` plus `SELECT` on the tenant and
device validation views, and blob write on the upload path only — it has no `SELECT` on content tables and
no unwrap. `control-api` holds the `ops` configuration, device-state and grant role, plus **sign** on the
policy-bundle signing key; it cannot read prompt content or an unwrapped key. `content-vault` holds
**unwrap and wrap** on per-tenant KEKs and blob read on ciphertext, and it has no user-facing endpoint at
all. `query-api` holds `SELECT` on `mart`, on a filtered view of `ingest` and on the audit surface, and
reaches content only by calling `content-vault`. `aggregator` and `reconciler` run as separate managed
identities and share one database role, `sac_ops`, which holds `mart` write, the reconciliation and
retention surface — including `DELETE` on `ingest.observation` and `ingest.submission` and row access to
`ops.content_object` for expiry — and no unwrap. `migration` holds DDL on all four schemas, is
assumed only by the migration job, and cannot be assumed by an app. **CI deploys under a workload-federated
identity with no secret at all**, scoped to resource-group deployment and ACR push, with no Key Vault or
database data-plane access. A human operator is an Entra ID principal with PIM-eligible roles and, by
default, infrastructure metadata only — no content, under customer-held key mode (master §4.3).

**As built:** `control-api`'s **sign** right is not yet assigned. Its only Key Vault assignment in
`azure/main.bicep` is the `secretsUser` operational role (*Key Vault Secrets User*), which is a
secrets-read role and carries no key-signing permission; the policy-signing and session-signing keys
reach it as Key Vault secrets mounted as files, not as keys it signs with in the vault. The composition
assigns Key Vault roles and the vault's Storage Blob Data Reader on the ciphertext account; the database
grants above are left to the migration. Two credentials are new with the identity service. The vendor's
Entra application authenticates as `control-api`'s managed identity, presented as a federated identity
credential, so no Entra secret exists. The `dashboard` server authenticates to `control-api`'s internal
sign-in API with a shared secret held in Key Vault (`sac-internal-token`): a stored credential, and a
stand-in until `control-api` verifies the dashboard's managed-identity token instead.

Every connection to PostgreSQL is Entra-token authenticated, so **no database password exists**. Row-level
security is forced on every table with every application role non-owner and without `BYPASSRLS`; the tenant
comes from the authenticated session and never the request body; a session that has not set a tenant reads
zero rows (C32, master §4.3).

### 5.2 Workload identity federation for CI

CI authenticates to Azure with an OIDC federated credential — subject-scoped to the repository,
branch/tag pattern and environment — so that **no long-lived Azure credential exists in the pipeline**.
Each environment has its own federated credential and its own service principal; a pull request from a
fork cannot obtain a token for any environment, and a compromised staging credential cannot reach
production. The same pattern is used for the Container Registry push identity, which is separate from
the deployment identity: pushing an image and deploying it are different privileges.

### 5.3 Key Vault access design

Authorization is **RBAC**, not legacy access policies: access policies are per-vault and opaque, they
do not survive a vault replacement cleanly, and they cannot be reviewed as a role assignment. Roles are
assigned to identities, never to groups of humans, and only two role assignments matter:

- `Key Vault Secrets User` and `Key Vault Crypto Service Encryption User` for the operational paths.
- **`Key Vault Crypto User` — unwrap — held by exactly one identity: `content-vault`.** This is C15 and
  D7 made mechanical: `ingest-api` cannot unwrap even if it is fully compromised, because no assignment
  exists to give it.

Additional invariants: purge protection on; soft delete 90 days; separate key purposes (KEK wrap,
policy signing, TLS) with no key serving two purposes; per-tenant KEK naming that carries the tenant
identifier so a key inventory can be reconciled against the tenant table; and a CI policy assertion that
fails the build if any principal other than `content-vault` holds unwrap on a KEK.

**As built:** the declared assignments do not yet meet the "exactly one identity" rule. Alongside
`content-vault`'s `Key Vault Crypto User`, `azure/main.bicep` (lines 242–246) assigns `ingest-api`'s
identity the `cryptoServiceEncryption` operational role at vault scope, which
`azure/modules/keyvault.bicep` (lines 47 and 96) resolves to `Key Vault Crypto Service Encryption
User`. That built-in role's data actions are understood to include key wrap and unwrap; the role
definition is not in the repository and has not been confirmed against a subscription. The CI
assertion (`azure/pipelines/policy-scan.yml`, `separation-of-duties`) inspects only
`unwrapPrincipalIds`, so it passes with this assignment in place. The rule stands; the assignment is
what has to change.

**Customer-held keys.** Where a customer supplies the key material, the tenant's KEK lives in Managed
HSM under a key policy whose quorum the vendor cannot satisfy alone. Destroying it destroys the
ciphertext (brief §4.4, C15) — see §12.5, which states exactly what is and is not recoverable
afterwards.

### 5.4 Deployment identity versus runtime identity

The distinction is not decorative. The deployment identity can create a container app, assign a managed
identity, change a scale rule and rewrite a firewall. It **cannot** read a row of `ingest`, cannot
unwrap a key, and cannot read blob content. The runtime identities can read rows and unwrap keys, and
cannot change infrastructure. A single compromised deployment credential therefore yields no customer
data, and a compromised runtime credential cannot escalate to infrastructure or to another tenant's
keys. The migration identity is a third class: DDL only, no data-plane reads, and it exists only for
the duration of a migration job.

**No secret is ever written into a pipeline variable, a container image, a Bicep parameter file, or an
environment variable whose value is a credential.** Container Apps environment variables reference Key
Vault; a secret in a plaintext environment variable reaches Log Analytics through revision logs, which
is a far wider audience than the secret's blast radius.

**As built:** the module supports Key Vault references (`keyVaultEnv` in
`azure/modules/container-app.bicep`) and secrets mounted as files (`keyVaultFiles`). `control-api` uses
both — the internal token and the directory key as references, the session-signing and policy-signing
keys as files — and the `dashboard` reads the internal token; every other app passes
`keyVaultEnv: []`, and the environment variables it does pass are non-secret coordinates. The
consequence is recorded in ADR 0019: `ingest-api` needs its TLS certificate, key and device-CA bundle
delivered this way, so a deployed container today "cannot authenticate a device".

---

## 6. Endpoint distribution

Distribution rides the **customer's** management plane, because §5.6 establishes that no EDR or MDM
platform permits in-sensor injection, and because the vendor has no management channel to a machine it
does not own. The customer's Intune or Jamf deploys a signed artefact the vendor built. Coverage is
therefore 70–85% by construction (E24), and the product's job is to make the remainder visible rather
than to pretend it away.

### 6.1 Artefacts

| Artefact | Platform | Contents | Delivery |
|---|---|---|---|
| `ShadowAICapture.msi` | Windows x64, Windows 10 21H2+ | `capture-core` service, `classifier-host`, document parser, CLI trust shim files, spool directory ACL, uninstall entries | Intune Win32 app (`.intunewin`), requirement rules on OS version and architecture, detection rule on product code **and version** |
| `ShadowAICapture.pkg` | macOS 13+ | LaunchDaemon (`capture-core`), `classifier-host`, parser, shim profile fragment | Jamf Pro policy, scoped to a smart group |
| Linux package | Linux, x86-64 and arm64 (userland service) | `capture-core` service, `classifier-host`, parser, spool directory ACL, `systemd` unit | The customer's configuration management (Puppet/Ansible/Chef) or an OS package repository. `installer/linux/install.sh` is the reference layout |
| `capture-extension` | Chromium MV3 | Observation, page-context attachment read, inline warn/block | **Enterprise policy force-install**, one policy per browser: Chrome and Edge `ExtensionSettings`. Firefox and Safari are out of scope (brief §5.5, E23) |
| Trust and proxy configuration | All | macOS: root CA into the **System** keychain with Always Trust, proxy settings, shell profile fragment — delivered as a Jamf configuration profile. Windows: root CA into **Local Machine → Trusted Root** (or the Enterprise/GPO store), WinHTTP/WinINET proxy, QUIC disabled — delivered as an Intune configuration profile or Group Policy. Linux: root CA into the system trust store, proxy via the environment, QUIC disabled — delivered by the same configuration management that installs the package | The customer's MDM or configuration management. **Not** installer payload |

**One MSI for every customer, and a tenant package around it.** The MSI is code only: it carries no
tenant, no key and no token, takes no install parameters, and is the same file for every customer, so
it can be signed once and its reputation built once (§8). What makes an install a customer's is one
small file beside it, `ShadowAICapture.tenant.env`, holding exactly four values — the tenant id, the
device endpoint, the tenant's **deployment key** and the credential mode (`dpop`). The customer's admin
downloads the two together from **Settings → Deployment**, as an `.intunewin` for an Intune Win32 app or
a `.zip` for Configuration Manager, Group Policy or another MDM, and each download mints a new
deployment key, revocable from the same page. The install command is `msiexec /i ShadowAICapture.msi
/qn` everywhere; the MSI copies the tenant file from the folder it runs from. Everything else a device
needs that is tenant-specific arrives after enrolment from `control-api` (the policy bundle, the
user-reference key), and everything vendor-wide (the policy-bundle trust anchor, the classifier release)
is in the MSI. The package is a credential in the sense that whoever holds it can try to enrol a device,
which is why a tenant whose devices Intune manages can also require each device to be one its Intune
manages ([02](02-ingest-and-transport.md) §5.1).

**Classifier content is deliberately not an installer payload**: rules, models and localised strings are
delivered as signed content by `control-api` (§9.5, C20), which is what lets a classifier be fixed without
a release and without an MDM round trip.

**Root certificates are honoured only from specific stores, and installing into the wrong store fails
silently** (E7). Windows: Local Machine → Trusted Root, or the Enterprise/Group Policy trust stores.
macOS: Default or System keychain with "Always Trust". Linux: the system trust store installed by
`update-ca-certificates` or the distribution's `p11-kit` module. All platforms are verified by a
**post-install self-test** that reports success or failure as data — the alternative is a silent TLS
failure that looks like a network problem to the user and like coverage to the vendor.

**QUIC is disabled by policy and UDP 443 is blocked at egress** (E6). Browser policy alone is
insufficient, and the product must not depend on a client honouring a setting it may ignore.

**As built:** `installer/release-msi.mjs` builds the generic `ShadowAICapture.msi` and reads
`release.json` (product code, upgrade code, version, package code, digest, size, publisher, whether it
is signed) back out of the built file rather than predicting it; `control-api` reads that folder
(`SAC_AGENT_RELEASE_DIR`) to build each tenant package ([installer/README.md](../installer/README.md),
which also holds the Intune, Configuration Manager and Group Policy steps). The MSI resolves its source
folder, checks for the tenant file there or an installed one, and copies it to the profile folder; with
neither it fails with exit 1603 before changing anything. An upgrade without a tenant file keeps the
installed one. The service runs `capture-core` with two `--config-file` arguments, the vendor file and
then the tenant file, the later winning. The signing step exists and is off by default; nothing has
been signed. The `.intunewin` is built by `control-api` in Microsoft's Win32 content format (an
encrypted inner `IntunePackage.intunewin` and its `Detection.xml`), so the admin need not run the
Content Prep Tool; in Intune it is a Win32 app with install command `msiexec /i ShadowAICapture.msi /qn`,
install behaviour System, and an MSI detection rule on the product code with version
greater-than-or-equal, and a later release is added with supersedence and upgrades in place. The
generic MSI has been built and inspected on the Windows build host but not installed there; the lab
MSI below is what has been installed. `capture-core` is registered as a `LocalSystem` service that
implements the SCM contract itself; `classifier-host` is installed beside it and run by it as a child
process; a full uninstall removes the service, both directories (tenant file included), the machine
environment entries and the trusted root. It departs from the table above in four ways:

- **The lab MSI carries more than a tenant package would.** `installer/lab-msi.mjs` builds the same
  MSI with the lab profile's files added (a signed policy bundle, a device CA pair, the lab edge's CA)
  and puts beside it a tenant file holding a full lab profile and a single-use enrolment token in place
  of a deployment key. It stays double-clickable. A tenant package carries none of those: the device
  fetches its bundle from `GET /v1/policy` and mints its own CA.
- **Classifier content is installer payload too**, contrary to the paragraph above: the signed release
  is a directory the MSI installs, because `control-api` does not yet deliver signed content.
- **Trust and proxy configuration is done by the agent, not by a configuration profile.** With no CA
  configured (a tenant package configures none), the agent mints a per-device interception CA on first
  start, keeps its key where only SYSTEM and Administrators can read it, and installs the root into
  Local Machine → Trusted Root; a CA that was the same on every device would make one device's key
  every device's interception capability. It writes the proxy and CA variables into the machine
  environment itself. The WinHTTP/WinINET system proxy is not set and QUIC is not disabled, so only
  clients that honour the environment variables are captured; a browser is not.
- **The document parser and the spool directory ACL are not separate payload items.** The parser is a
  mode of the `classifier-host` binary, and the state directory is created by the service with the
  ACLs `%ProgramData%` gives it; no ACL is set by the installer.

The macOS PKG source and the Linux package exist as the manifest renders them; the PKG has never been
built, and the Linux install is exercised in a container rather than under `systemd`.

### 6.2 Enrolment, and what the user sees

One-shot enrolment (brief §4.2, C11). The credential is either an `x509` certificate or an RFC 9449
`dpop` key — **a certificate is no longer mandatory**, so an MDM/PKI is optional rather than a hard
prerequisite (ADR 0020). Re-imaging returns the existing identity rather than creating a duplicate. The
user-visible part of deployment is deliberately small on Windows and on Linux and larger on macOS,
because **M0 → M1 is a permission boundary** (C2): M0 requires no content access, and M1 requires reading
prompts and attachments.

**What a packaged device does, with nobody at it.** The service starts, reads the deployment key from the
tenant file and enrols with it ([02](02-ingest-and-transport.md) §5.1), stating the identifiers the
operating system gives it — the Intune managed-device id, the Entra device id, the BIOS serial — which a
tenant with Intune verification on checks against its own Intune through Microsoft Graph. Its hardware
identity is seeded from the SMBIOS UUID and serial, so a re-imaged device returns as itself. The
response carries the tenant's user-reference key, with which the device names the console user
pseudonymously ([03](03-data-platform.md) §3.3). It then fetches its policy bundle from
`GET /v1/policy`, verifies it under the vendor key the MSI pins, caches it, and polls for changes; until a
verified bundle arrives it collects at M0. An Intune device is one product device, so a copied package
cannot enrol a second identity behind a real device's identifiers; a refusal is visible to the admin in
the audit trail.

**Linking the customer's identity provider comes first.** The vendor creates the product tenant and
issues a one-time onboarding link with the customer's email domains (`control-api tenant create`,
`tenant invite --domain`). The customer's admin opens it and chooses Microsoft Entra ID (admin consent to
the vendor's multi-tenant application, whose one Graph application permission is
`DeviceManagementManagedDevices.Read.All`) or another OIDC provider (issuer, client id, secret); the
first sign-in through that connection activates it and makes that person the tenant's admin
([06](06-security-and-threat-model.md) §4.1). From then on the admin downloads packages, turns on the
Intune check, and mints the SCIM token the identity provider provisions people with. In Entra that is a
separate, non-gallery enterprise application pointed at the SCIM base URL, until the product is listed
in the Entra application gallery; the setup maps `objectId` to `externalId`.

**M0 requires no content permission on any platform** — a service install on Windows, a `systemd` unit on
Linux, a LaunchDaemon install on macOS. That is the whole enrolment experience at M0. **M1 and above are a
permission boundary** (C2): on Windows and Linux nothing changes beyond the service install, because the
service reads what it is already entitled to read and any gap appears as a coverage gap; on macOS,
Files-and-Folders and Full Disk Access are pre-granted where the platform allows it (E17), and **Screen
Recording can never be pre-granted by MDM** (E16) — so no mechanism may depend on it, and the post-install
self-test reports its absence as a degraded capability rather than as an enrolment failure. Linux has no
per-application permission gate equivalent to macOS TCC: root reads what root can read, so an M1 gap on a
file the service cannot reach is a coverage gap, never a denial.

The policy bundle is signed, versioned and `304`-able, and a signature verification failure retains the
previous bundle and raises an error — never a fallback to unsigned or empty policy (C10). If there is no
previous bundle, content-reading modes are refused and the device falls to **M0**, not to "no policy
means no restriction".

### 6.3 Extension deployment, and its one irreversible property

Chrome and Edge are two policies, two stores or two hosted packages, and two review processes (E23).
The extension is force-installed by policy, which is also what preserves `webRequestBlocking` — the
capability that lets an enterprise-deployed extension warn or block inline rather than merely observe
(E1). This is a dependency on the customer's browser policy, not on the store: a store listing cannot
grant blocking, so **deployment through policy is not optional**.

The irreversible property: **a published extension version cannot be retracted**. The store will not
un-publish it, and the customer's policy will keep serving it. Mitigations, all of which must exist
before the first extension ships: pin by extension ID so a swapped listing cannot install; publish a
`max_version` pin the customer's policy can carry so a defective version stops being *eligible*; and
keep the extension's privileged actions off the wire — remote control (pause collection, disable a
provider, change sampling) arrives as **signed content** verified against a key compiled into the
extension, so a bad release is neutralised without waiting for propagation.

### 6.4 Clean uninstall

Uninstall must be a complete reversal of what the product did to a machine, in the reverse order of
installation, and it must be tested as a first-class path — because the product's failure mode on some
paths is *breaking the user's machine*, not losing a data point (E14).

Ordered removal, identical on every platform: (1) stop accepting new observations and drain or discard
the spool per tenant policy — **discard is the default**, and retained local content is deleted with the
deletion reported as an erasure receipt (C34, brief §3.4); (2) **release the loopback port before
anything else**, because an uninstall that leaves the port bound leaves the user's local AI broken (E14);
(3) restore the system proxy to its pre-install state and remove the CLI shim profile fragment; (4)
remove the root CA from the correct store and **verify removal** — a root left behind is a trust decision
the customer did not consent to keep; (5) remove the browser extension through the same enterprise policy
that installed it (the vendor cannot remove it; the customer's policy does) and clear the extension's
stored state; (6) delete local state — the spool directory (its segment log, counter file and lock), cached policy bundles, cached classifier content,
logs; (7) remove the service, LaunchDaemon or `systemd` unit and the binaries, leaving no scheduled task,
launch agent, unit file, firewall rule or Event Log source registration behind.

**Verification is a post-uninstall script that reports, as data, that steps 2–4 and 6–7 left nothing
behind.** An uninstall nobody verified is an uninstall nobody can promise.

**As built:** on Windows the MSI's uninstall stops the service, which removes the CLI shim
files and the machine environment entries it set and removes the root from the store it installed it
into (the generic vendor file and the lab profile both set remove-on-stop), then removes the service and
binaries and deletes `%ProgramData%\ShadowAICapture` whole: spool, sealed credential, the per-device CA,
the cached policy bundle, held M3 content, the tenant file, logs. An upgrade keeps that directory, and
with it the tenant file and the device's enrolment. This was checked once, by hand, on one host, by looking for the service, the two
directories, the environment entries and the root afterwards. There is no post-uninstall verification
script, no erasure receipt for discarded local content, and no drain-or-discard choice: the spool is
discarded after the bounded shutdown drain. Steps (3) system proxy and (5) extension do not apply,
because this build sets no system proxy and installs no extension.

---

## 7. Signing and notarisation

### 7.1 The 460-day clock

Code-signing certificates have a validity of **460 days** (brief §5.5, E19). Rotation must be automated,
and it must exist **before the first ship** — not because it is difficult, but because the failure it
prevents has a known date and a fleet-wide consequence.

**Windows MSI and binaries** sign with Authenticode, the key held in an HSM or a managed signing service —
never as a file, which is both a policy violation and a compromise waiting to happen. **macOS PKG and
binaries** sign with Developer ID Application and Developer ID Installer; notarisation is a separate
credential with its own lifetime. **The Linux package and its repository metadata** sign with the
distribution's packaging key (GPG for APT/RPM); Linux is therefore the one endpoint platform that is
**not** governed by the 460-day Authenticode/notarisation clock, though the packaging key has its own
inventory, overlap and rotation policy in §7.2. **The browser extension** is signed by the store publisher, plus the
vendor's own content-signing key, which is what makes remote control possible without a store round trip
(§6.3). **Content** — rules, models, policy — uses a separate key with a separate rotation and a separate
lifetime, because content must outlive code certificates and a content update must never be blocked by a
code-signing problem (C20, §9.5).

### 7.2 Automated rotation

**Inventory and expiry monitoring from day one.** Every signing credential is registered with its issue
date, expiry date, owner and the artefact families it signs; alerts fire at **90, 60, 30 and 14 days**,
escalating in severity; and CI fails any release that would ship an artefact whose certificate has under
**60 days** of remaining validity. **Two valid credentials in overlap**: at renewal the new certificate is
issued and registered while the old one stays valid, so a renewal failure is a task rather than an
incident. **A dry run at every rotation**, against a throwaway artefact family in staging. And
**timestamping is mandatory** — every signature requests an RFC 3161 timestamp, because a timestamped
signature remains valid after the certificate expires and an untimestamped one does not. Without
timestamping, expiry invalidates the *installed* fleet's signatures, which is the difference between an
update freeze and a trust failure.

### 7.3 macOS notarisation and stapling

Every macOS artefact is signed with the Developer ID certificate, submitted to Apple's notarisation
service, and **stapled** so that a machine with no network access at install time can still validate it.
A Jamf deployment is frequently an offline or restricted-network install, and an unstapled artefact
fails Gatekeeper on exactly those machines. Notarisation is a hard release gate: the PKG does not enter
a ring, and does not enter the customer's Jamf, until the notarisation ticket is stapled and verified on
a clean machine.

### 7.4 The operational consequence of an expired signing certificate

State it plainly, because it is the single largest scheduled risk in the delivery plan:

> **An expired code-signing certificate is a fleet-wide outage with a known date** (brief §5.5).
> On that date the vendor cannot sign a new Windows MSI, a new macOS PKG, a new Linux package, or a new
> extension package. There is no workaround, no exception, and no vendor-side appeal. (The Linux
> packaging key is not the 460-day certificate, but an expired key blocks a new package just the same.)

What exactly breaks, and what does not — the distinction is operationally important:

| Still works after expiry | Stops dead after expiry |
|---|---|
| The installed fleet keeps running | Shipping a new desktop-agent version — **including a fix for a defect that is breaking customers** |
| A **timestamped** previously-signed artefact remains trusted | Shipping a Windows MSI, macOS PKG, Linux package or extension package of any kind |
| Content updates: policy bundles, rules, models (separate key, §7.1) | Any hotfix delivered as a binary |
| Server-side changes (no endpoint signing involved) | Onboarding a new macOS installation whose PKG is not already stapled and trusted |
| Kill-switch and shadow-mode changes (server-side, §9.5) | Rolling back to a *re-signed* build, if the rollback path requires re-signing |

The mitigation that makes this survivable is the separation in §9.3: **because content is not code, an
expired code-signing certificate does not stop a bad classifier release being reversed** (C20). It stops
code. For a bounded period, the product can still be operated, tuned and made safe while it cannot be
updated.

### 7.5 Runbook: certificate expiry or signing unavailable

| Step | Action | Evidence |
|---|---|---|
| 1 | Confirm from the credential inventory (issue/expiry/artefact families) which artefacts are affected and what the actual remaining validity is | Inventory entry |
| 2 | If the certificate is expired or invalid: **freeze all endpoint releases.** Do not attempt to sign with an expired certificate | Release freeze entry in the change log |
| 3 | Verify the *installed* fleet is unaffected: signature validation on a sample of enrolled devices via the post-install self-test's last report | Sample of device health records showing signature status |
| 4 | Confirm content-lane operations still work: publish a policy bundle, push a classifier release to shadow, verify a device honours it | Device health showing the new bundle version |
| 5 | Execute rotation per §7.2; if the CA is the blocker, escalate to the certificate vendor with the renewal evidence in hand | Renewal ticket |
| 6 | Re-sign only the **currently shipping** version, then one release ahead; do not re-sign the historical ladder | Signed artefacts with fresh timestamps |
| 7 | Verify on a clean, isolated machine: install, self-test, proxy trust, uninstall | Self-test report |
| 8 | If any customer's fleet will be without a fixable defect for the duration, **notify them with dates** rather than letting them discover it | Customer communication record |
| 9 | Post-incident: make the trigger a 90-day alert rather than a 30-day one if the alert was late | Updated alert threshold and a note in the rotation runbook |

---

## 8. Reputation and the bake period

Brief §5.5 removes three assumptions that every endpoint product used to rely on, and this section is
what replaces them.

1. **An EV certificate no longer bypasses SmartScreen.** Reputation accrues over weeks and hundreds of
   installs, and there is no submission mechanism for consumer endpoints (E20).
2. **The product will not qualify for Microsoft's security-vendor allowlist** — it requires real-time
   malware protection plus annual independent anti-malware certification, neither of which this product
   has or wants (E21). Unwanted-software treatment during the reputation period is the planning case.
3. **A component that installs a root certificate and intermediates TLS resembles malware.** False
   positives from other endpoint security products are expected, not exceptional (E22, R4).

### 8.1 The exclusion artefact — a shipped deliverable, not advice (R4)

R4 requires exclusions to ship as an artefact. It is a **release deliverable**, versioned with the
product, present in the repository, in the installer's documentation bundle, and in the customer's
deployment runbook:

| Contents | Why |
|---|---|
| Process names and install paths for `capture-core`, `classifier-host`, the document parser child, and the loopback broker | Other security products act on process identity and path |
| The Authenticode publisher name and the certificate thumbprint(s) currently in use, with the rotation date | A rule pinned to a thumbprint breaks silently at rotation; shipping the date is what lets the customer's security team re-pin before §7.4's date |
| The loopback port range the broker may bind | Port-based heuristics are common and the broker's binding is legitimate |
| The root CA's subject and thumbprint, and the stores it is installed into | Distinguishes an enterprise-deployed root from an attacker's |
| The destinations the egress proxy will intercept, and the statement that everything else is blind-tunnelled | An allowlist request that is scoped to enumerated destinations is answerable; "intercept everything" is not |
| A one-page statement of what the component does **not** do: no keystroke logging, no filesystem indexing, no file-at-rest scanning, no response-side capture, no fleet-wide packet inspection (C9) | The first question a customer's security team asks, answered before it is asked |
| Named exclusion formats per major EDR vendor | The artefact is only usable if it is in the format the customer's product consumes |

The exclusion artefact is also the artefact that makes the **pilot** runnable: without it, the pilot's
first week is spent triaging other vendors' alerts instead of validating the product.

### 8.2 Staged rollout, and what to expect at each stage

| Stage | Population | Duration | Expected reputation behaviour |
|---|---|---|---|
| **Internal** | Vendor's own machines | 2–4 weeks | Every artefact is unrecognised; SmartScreen prompts on every install. This is where install, upgrade and uninstall are proven |
| **Circle of trust** | Design-partner IT teams and volunteers at 2–3 customers | 4–6 weeks | Still unrecognised. Prompts continue; the runbook below is exercised for real, by people who can be asked what they saw |
| **Pilot tenants** | A named pilot group per customer, 100–500 devices | 6–8 weeks | Reputation begins accruing with install volume. **"Unrecognised application" prompts are expected and must be planned for, not escalated as a defect** (E20) |
| **Bake** | Wider rollout inside pilot tenants | 4–8 weeks | EDR false positives (R4) surface here, because that is the first time the interceptor meets each customer's actual security stack. The kill switch is available throughout (§9.5) |
| **General availability** | Fleet-wide at a customer, subject to §9's rings | — | Reputation is a running property, not a finished one: a new signing certificate (§7) is a reputation event |

**Pilot expectations, stated as a script for the customer's helpdesk**, because the alternative is that
the pilot's first support tickets are all the same ticket: a SmartScreen "Windows protected your PC"
prompt, an Edge/Chrome "extension added by your organisation" notification, a macOS Gatekeeper
confirmation on first launch, and — during the bake stage — one or more alerts from the customer's own
EDR. Each of the four has a written response, an owner, and a resolution path that does not involve
disabling the security product.

### 8.3 Reputation-building plan

| Action | Effect | Owner |
|---|---|---|
| Install volume through managed deployment, not manual installs | Reputation follows installs; a manual install on one machine contributes nothing to the fleet's reputation | Release engineering |
| Consistent publisher identity across all artefacts and all versions | Reputation attaches to the publisher identity; changing it resets the clock | Release engineering |
| Timestamped signatures on every artefact (§7.2) | Keeps a signature valid after the certificate expires, which preserves accrued reputation across a rotation | Release engineering |
| Ship the exclusion artefact with every release, updated for the current thumbprint | Converts each customer's EDR from an opponent into a configured control | Product + security |
| Track customer-reported false positives as a first-class defect with a per-vendor counter | The only honest measure of whether the bake period is ending | Support + security |
| Never ship an artefact without the pilot install/uninstall rehearsal on the target ring | Reputation is lost faster than it is gained; a bad uninstall is a malware-shaped event | Release engineering |
| **Do not claim security-vendor status, allowlisting or SmartScreen bypass in any customer-facing document** | The product does not have them and will not (E21). A claim that is not true is worse than a warning that is | Product marketing + legal |

---

## 9. Update safety

Brief §5.5 makes five things hard expectations: deployment rings with automatic halt, signed update
manifests, strict separation of content from code, atomic install with rollback, and a server-side kill
switch. Each is defined below with the mechanism, the signal and the rollback, because "we will be
careful" is not one of the five.

### 9.1 Rings

Rings are populations of devices, defined per customer as well as per vendor, so that a customer can
hold a population back without the vendor losing the ability to ship.

| Ring | Population | Size | Soak | Promotion criteria | Halt criteria |
|---|---|---|---|---|---|
| **Ring 0 — internal** | Vendor's own machines, all supported platforms | ~50 devices | 72 h | Install and uninstall clean; self-test green on all supported platforms; no crash; proxy trust verified | Any install failure; any uninstall failure; any crash loop |
| **Ring 1 — design partners** | IT/volunteer populations at 2–3 customers, including macOS and Linux | 100–500 devices | 7 d | Health reporting ≥ 98% of ring; spool drop count unchanged from baseline; coverage per provider unchanged; no EDR escalation attributable to the update | Reporting below 95%; any provider's coverage down more than 20% relative; a new EDR false positive |
| **Ring 2 — pilot tenants** | Named pilot groups | 500–2,000 devices | 7 d | Same as ring 1, plus classification latency p95 within budget (≤150 ms) and no rise in degraded-classifier share | As ring 1, plus any rise in `degraded` share above the §10.3 threshold |
| **Ring 3 — general fleet** | All remaining devices, per tenant, subject to tenant-held groups | remainder | — | Ring 2 criteria sustained for a full week | Automatic halt on any §9.2 signal |
| **Ring H — hold** | Devices pinned to a known-good version | any size | n/a | n/a | Never auto-promoted. Devices land here on repeated failure rather than being retried forever |

**Ring 2 is where the egress proxy may first ship** (master §6, step 6), and it is deliberately the ring
that includes a customer's own security stack rather than the vendor's.

### 9.2 Halt signals — automatic, not discretionary

Any one of these halts promotion of the current ring **and** begins the rollback path in §9.4. Halt is
automatic because the alternative is a release engineer deciding at 02:00 whether a metric is noise.

| # | Signal | Threshold | Detected from |
|---|---|---|---|
| H1 | Crash rate of `capture-core` | > 1% of ring devices reporting a crash in 24 h | Health reports (C23) |
| H2 | Devices that stop reporting after the update | > 2% of the ring's previously-reporting devices | Device liveness (§10.2) |
| H3 | Spool drop counter increase | Any increase attributable to the updated version | Health report's dropped count (C22) |
| H4 | Batch upload failure rate | > 5% of batches failing (excluding 401 revocation) | `ingest-api` metrics by client version |
| H5 | Coverage regression per provider | Any provider's reporting share down > 20% relative to the pre-update baseline | Coverage snapshot (§10.2) |
| H6 | Classification latency | p95 > 150 ms in the ring for 15 minutes | Device-reported latency histogram |
| H7 | Degraded-classifier share | > 5% of events in the ring for 1 hour | `confidence = degraded` share (C21) |
| H8 | Install failure rate | > 1% of attempted installs | MDM-reported install status, reconciled against enrolment (C11) |
| H9 | Local-inference breakage | Any device reporting the loopback port held while the upstream is unreachable | Port-state health field (E14) — the one signal whose failure breaks the user |
| H10 | Policy signature verification failures | Any sustained increase | Device health error codes (C10) |

H9 exists because mode F's failure mode is inverted: everywhere else an unhealthy component loses
coverage, and here it stops the user's local AI from starting (E14). It is therefore the only signal
whose threshold is "any".

### 9.3 Content versus code

The separation is strict, and it is what makes most incidents cheap. **Code** is `capture-core`,
`classifier-host`, the extension package and the document parser; it is delivered by installer through the
customer's MDM or by enterprise policy, signed with the 460-day code-signing certificate (§7.1), rolled
back by ring supersedence and atomic install rollback over hours to days, and it requires a release.
**Content** is the policy bundle, the rules bundle, the model artefact, the destination allowlist and
per-collector feature state; it is a signed document the device fetches from `control-api` (C10), signed
with a separate long-lived content key that is *not* a code-signing certificate, rolled back by publishing
the previous version — devices pick it up on the next poll, in minutes — and it **requires no release at
all** (C20). Content is versioned and `304`-able, and a verification failure on a new bundle retains the
previous one and raises an error (C10). The consequence: **a bad classifier release never requires
shipping code**, and an expired code-signing certificate never blocks fixing a classifier (§7.4).

### 9.4 Atomic install with rollback

Each installer version installs side-by-side into a versioned directory, switches the service registration
to the new version as the last step, and verifies by self-test before declaring success. Failure at any
step switches back to the previous version, which is still on disk and still registered as the fallback.
The spool's on-disk format — an append-only segment log of AES-256-GCM-sealed frames
(`endpoint/capture-spool`), not a database with a schema — must stay readable across N-1 and N, so a
rolled-back binary can read the spool it inherited; a spool it cannot read is a spool it must report as unreadable rather than silently discard,
because **no collection path may fail into a state that reports success** (C25).

Uninstall is not the rollback path. Rollback keeps the product installed at a previous version;
uninstall is §6.4.

### 9.5 The server-side kill switch

**Scope.** The kill switch operates on **providers and on content**, independently: disable the egress
proxy provider, disable the loopback broker, disable the process detector, disable discovery of new
destinations, force a tenant to M0, force a classifier release to shadow. It is deliberately not one
global button, because the likely incident is one provider misbehaving on one platform.

**Latency.** A device applies a kill switch on its next policy poll. The poll interval is therefore a
product parameter with an operational meaning, and the maximum time to affect the fleet is stated in
the runbook rather than discovered during an incident.

**Fail-safe semantics.** If a device cannot reach `control-api`, it **retains its last signed policy**
and continues. It does not revert to "no restrictions" and it does not fall to M0 on a network blip —
falling to M0 would look like the kill switch firing every time a laptop closed its lid. If a *new*
policy bundle fails signature verification, the previous bundle is retained and an error is raised
(C10).

**Reversal of a bad classifier release without shipping code (C20).** Two server-side operations, in
either order:

1. Flip the release's enforcement state to **shadow** — devices continue to classify and label with the
   candidate version, but stop acting on it. Blocking stops immediately; the labels remain available for
   diagnosis.
2. Publish the previous version as active, scoped to the affected rings or tenants. Devices pick it up
   on the next poll.

Both are audit entries, and both are visible in the console, because a control that is used silently is
a control nobody can reconstruct afterwards.

### 9.6 Client compatibility window

The server must accept clients it cannot force to upgrade. Compatibility policy:

- Every device-facing endpoint accepts **N and N-1, and N-2 for a stated window** after an endpoint
  contract change. Narrowing the window is a human-approved change (§4.5).
- A device below the minimum supported version is **not** silently rejected: it is accepted where the
  contract permits, and reported as `degraded` with its version, so the fleet's version distribution is
  a visible fact rather than an assumption.
- The envelope's `schema_version` is validated per request, and unknown fields are rejected rather than
  ignored, so a newer client hitting an older server fails visibly with a reason code (brief §4.3's
  "per-reason rejection detail") instead of losing fields quietly.

---

## 10. Observability and SLOs

### 10.1 The brief's §8 targets as instrumented SLOs

All targets are brief §8 unless stated. Measurement windows are 28-day rolling. Error-budget policy:
100% burn in 1 h pages on-call; 50% burn over 6 h opens a ticket **and freezes risky deploys** (§4.4).

| SLO | Target | How it is instrumented, and why that is the right measurement |
|---|---|---|
| Classification latency, interactive path | ≤150 ms p95 | **On the device** — a `classifier-host` histogram reported in health. Only timings leave the device, never prompt text. The interactive path is device-local (C19), so a server-side measurement would measure the wrong thing |
| Warn/block decision | ≤300 ms p95 | On the device, observation to decision. This is the number the user feels, and brief §6 warns that beyond it users route around the product |
| Ingest availability | 99.9% | Application Gateway plus `ingest-api` 5xx rate, excluding device-auth 401s, which are a revocation rather than an outage. Brief §8 attaches the reason: devices buffer through outages |
| Event visible in query layer | <60 s from receipt | `received_at` to recomputed aggregate bucket (`mart` watermark). This is a **freshness** SLO and it is what keeps a stale dashboard honest — master §4.4 requires aggregation lag to be visible, not hidden |
| Dashboard aggregate query | <2 s p95 | `query-api` per route template, served only from `mart`. C27: when this regresses, it is almost always because something began scanning `ingest` |
| Content retrieval once granted | <30 s | End-to-end, grant issued to content streamed to the analyst — because the components in between are a chain and the customer experiences the chain |
| **Devices reporting** | **≥95% of enrolled devices within 24 h** | Device liveness from `ops.device.last_seen_at`, as derived by the `mart.v_device_liveness` view (`reporting` / `stale` after 24 h / `never_reported` / `revoked`). **ASSUMPTION:** the brief sets no target here. *Justification:* brief §5.5's 70–85% management coverage is a *planning* figure for the population the customer can manage at all, while devices that are enrolled and then go silent are a signal the vendor owns. An operational guardrail, **not a contractual promise** (§14) |

### 10.2 The signals that matter more than uptime

The cloud tier will be up almost all of the time. The product's credibility is decided by the signals
below, because each of them is a way the system can look healthy while collecting nothing (C25), and
because the brief's requirement is that the system can say **where it could not see** as clearly as what
it saw (R11).

| Signal | Definition and source |
|---|---|
| **Coverage per provider** | Share of enrolled, reporting devices whose provider `p` reports `healthy`, per platform, from the coverage snapshot (build step 3). Catches a mechanism that stopped working on one platform — the R5 browser-agent question |
| **Coverage per usage mode** | Share of reporting devices claiming capability for each of brief §2's modes A–I. Catches a matrix row that is unreachable in practice rather than in theory |
| **Undercount from spool drops** | Per-device `dropped_total` summed per tenant per day, from health reports (C22). Silent loss made countable |
| **Degraded-classifier share** | Share of events with `confidence = degraded` (C21), from event labels. Catches a classifier timing out on real prompt sizes — the failure that would otherwise be reported as "no sensitive data found" |
| **Devices not reporting** | Enrolled devices whose `last_seen` is older than 24 h, and older than 7 d, in two buckets, from the liveness job. Question 7 of brief §3.6; bucketed because "off on holiday" and "uninstalled" are different facts |
| **Dedup reconciliation drift** | Observation-level versus submission-level counts, plus non-reconcilable digests, from `reconciler` (R9, Q3). Catches double-counting that would inflate every number a customer sees |
| **Aggregate freshness** | Age of the oldest bucket not yet recomputed for the current window, from the `aggregator` run record. Catches a dashboard presenting stale numbers as current |
| **Certificate and credential expiry** | Days remaining for every signing credential, per artefact family, from the credential inventory (§7.2). Catches §7.4's known-date outage, and policy-signing key expiry |
| **Skew per device** | Distribution of the `received_at − occurred_at` deviation per device (C26), from the event dual clocks. Catches clocks that make ordering wrong; reported, never normalised away |
| **Tamper and signature-failure counts** | Devices in `tampered` and policy signature verification errors, from health error codes (C10, C24). A component that was stopped or altered, reported rather than inferred from absent events |
| **Grant denial reasons** | Rates of `retention_expired`, `not_policy_relevant`, `over_budget` and `mode_not_permitted` (C14), from `control-api` grant decisions. Catches a misconfigured budget or retention class before the customer notices missing content |
| **Revocation storm rate** | Revocations per hour, fleet-wide, from the device inventory audit. Catches §13's revoked-credential storm before it becomes a support queue |

### 10.3 Alerts

Every alert names what it means for a customer. An alert whose meaning cannot be stated that way is
either a dashboard or a defect.

| Alert | Threshold | Severity | What it means for the customer |
|---|---|---|---|
| Ingest availability burn | 100% of 28-day budget in 1 h | Sev-1, page | Nothing is arriving. Devices are spooling and will flush; if the spool fills, events are lost and counted |
| Ingest 5xx rate | > 1% for 5 min | Sev-1, page | Same, earlier |
| PostgreSQL unavailable or failover | Connection failure or HA failover event | Sev-1, page | Ingest and the dashboard are down; content retrieval is down. Collection continues on-device |
| Key Vault unwrap failure rate | > 1% for 10 min | Sev-1, page | Content retrieval is failing for every affected tenant; metadata and dashboards are unaffected |
| **Customer key disabled or destroyed** | Key state change on a tenant KEK | Sev-1, page + customer notification | That tenant's stored content is now unretrievable, **permanently if the key was destroyed**. Metadata is unaffected (§12.5) |
| Aggregate freshness | Oldest unrecomputed bucket > 15 min | Sev-2 | The dashboard under-reports by up to that window. Stated in the UI, not just in the alert |
| Aggregate freshness | > 60 min | Sev-1 | The dashboard is materially wrong; the event-visible SLO is breaching |
| Coverage collapse, one provider | Reporting share for a provider down > 20% relative, fleet-wide, 30 min | Sev-2 | A usage mode has stopped being collected. If it is the egress proxy, the customer's network path may also be affected |
| Coverage collapse, one tenant | All providers down > 50% for one tenant | Sev-2 | Likely a broken policy deploy or a mass uninstall at that customer |
| Spool saturation, fleet-wide | Devices with spool > 80% capacity > 5% of fleet | Sev-2 | Events are about to be dropped and counted. Something upstream has been unavailable longer than the spool can cover |
| Devices not reporting | > 10% of enrolled devices silent > 24 h | Sev-2 | The customer is paying for coverage they are not getting; possibly a failed rollout |
| Degraded-classifier share | > 5% of events for 1 h, any tenant | Sev-2 | Labels are rules-only, so the numbers understate sensitive-data traffic. Explicitly **not** "no sensitive data found" |
| Dedup reconciliation drift | Drift > 0.5% of submissions, or any non-reconcilable digest growth | Sev-2 | Counts a customer could challenge. Investigate before the customer does |
| Certificate expiry | 90 / 60 / 30 / 14 days remaining | Sev-3 → Sev-2 at 30 → Sev-1 at 14 | §7.4: on the expiry date the vendor cannot ship a fix to the endpoint |
| Client version below minimum supported | Any device below the N-2 floor | Sev-3 | That device's data may be incomplete after the compatibility window closes |
| WAF false positive | Blocked request rate from a known device user-agent class rising | Sev-2 | A customer's devices are being blocked from the product's own edge |
| Cost anomaly | Daily spend > 150% of the trailing 7-day median | Sev-3 | The attachment tier is the only unbounded line (brief §3.1); a spike is usually a retention or budget misconfiguration (§11.6) |
| Revoked-credential storm | > 50 revocations in 1 h | Sev-2 | Either an incident response at a customer or a defect; both need a human |

### 10.4 Telemetry discipline

OpenTelemetry from every service, one workspace, structured logs. Three rules, because they protect the
product's central promise rather than merely being tidy. **No content in telemetry, ever** — not prompt
text, not excerpts, not attachment bytes, not a content digest alongside identifying data; logging is a
second egress path and the same rule governs it. **Synthetic tenants in the same pipeline as real ones**,
because synthetic traffic exercises the SLOs and the alerts continuously and without it the alert set is
only ever tested by real incidents. And **sampling is configured, not assumed** — 10% of successful
traces, 100% of errors, all SLO-relevant metrics, with retention tiering; log ingestion grows with
verbosity rather than traffic, which makes it a decision rather than an outcome (§11.6).

---

## 11. Cost model

### 11.1 Pricing basis

> **All figures in this section are estimates, not measurements.** Basis: **Azure list prices, East US,
> pay-as-you-go, no committed-use discount, no enterprise agreement discount, no reservations, no
> savings plan.** Prices retrieved or recalled as of **2 October 2026** and **must be re-baselined
> against the Azure pricing calculator before any commercial commitment**. Ranges are ranges, not
> confidence intervals; they express two plausible sizing choices, not statistical uncertainty.

**ASSUMPTION (unit prices):** the unit prices below are the basis for every calculation in §11. Each is
a list price for East US on the date above; none includes a discount.

| Unit | Price used | Note |
|---|---|---|
| Container Apps active vCPU | $0.000024 / vCPU-second | ≈ $0.0864 per vCPU-hour |
| Container Apps active memory | $0.000003 / GiB-second | ≈ $0.0108 per GiB-hour |
| Container Apps requests | $0.40 / million | Above the monthly free grant |
| PostgreSQL Flexible Server `D2ds_v5` | ≈ $0.252 / hour | Zone-redundant HA doubles compute |
| PostgreSQL Flexible Server `B2s` | ≈ $0.0176 / hour | Burstable; dev and small-tenant variant |
| PostgreSQL storage | $0.115 / GiB-month | Same rate for HA standby storage |
| PostgreSQL backup (geo-redundant) | $0.10 / GiB-month | Above the free PITR allowance |
| Blob, hot | $0.0184 / GiB-month | |
| Blob, cool | $0.01 / GiB-month | |
| Blob, archive | $0.00099 / GiB-month | Retrieval charges not modelled |
| Blob, geo-redundant replication | $0.02 / GiB-month | RA-GRS |
| Front Door Premium, base | $330 / month | Per profile per region |
| Front Door / WAF requests | $0.012 / 10,000 | |
| Front Door egress | $0.085 / GiB | First-tier rate |
| Application Gateway `WAF_v2`, fixed | ≈ $0.44 / hour | ≈ $321 / month per gateway; the GA device ingress (ADR 0020) |
| Application Gateway capacity units | ≈ $0.014 / CU-hour | Estimate; a passthrough listener at this traffic uses few capacity units |
| Log Analytics ingestion | $2.76 / GiB | Pay-as-you-go; 90-day interactive retention included |
| Key Vault operations | $0.03 / 10,000 | HSM-backed keys carry an additional per-key monthly charge |
| Managed HSM | ≈ $4.6 / hour | ≈ $3,360 per month per pool, largely independent of use |
| Static Web App Standard | $9 / month | |
| Container Registry Premium | $50 / month | Plus geo-replication |

**ASSUMPTION:** tenant sizing uses the M1 default: **5,000 devices** (brief §3.1's upper bound),
~4,000 AI-active users, ~12,000 submission events/day. *Justification:* brief §3.1 gives these as the
sizing table; a smaller tenant is proportionally cheaper on the lines that scale with devices, and the
same on the lines that do not.

### 11.2 The arithmetic for a tenant at M1

**Event data volume.**

```
12,000 events/day × 1.5 KiB/event          = 18,000 KiB/day   = 17.6 MiB/day
17.6 MiB/day × 30                           = 528 MiB/month    ≈ 0.52 GiB/month
528 MiB/month × 12                          = 6.19 GiB/year
```

Brief §3.1 projects 5–9 GB/year for the same shape; 6.19 GiB sits inside that band, so the model is
consistent with the brief's own sizing rather than inventing a different one.

```
3-year event store at 1.5 KiB/event         = 18.6 GiB
plus indexes, TOAST and vacuum headroom ×2  ≈ 37 GiB
plus ops/compliance rows 3 years            ≈ 7 GiB
→ PostgreSQL storage line                    ≈ 45 GiB used of a 128 GiB provisioned disk
```

**Compute and platform lines.**

```
Container Apps  (active only, M1 steady state)
  4 services × 2 replicas × 0.5 vCPU × 24 h × 30 d   = 2,880 vCPU-h  (worst case, all always active)
  measured-shaped expectation at ~0.14 events/s       ≈   75 vCPU-h
  timed jobs: 2 jobs × 15 min/day × 1 vCPU × 30 d     ≈   15 vCPU-h
  → 90 vCPU-h × $0.0864                               = $7.78
  → memory ≈ 180 GiB-h × $0.0108                      = $1.94
  → requests: 43.2 M/month × $0.40/M                  = $17.28
  → Container Apps subtotal (excluding 2 migrations)  ≈ $27
PostgreSQL
  compute 730 h × $0.252 = $184, + HA standby $184    = $368
  storage 128 GiB × $0.115                            = $15
  geo-redundant backup ~90 GiB × $0.10                = $9
  → PostgreSQL subtotal                               ≈ $392
Front Door Premium
  base                                                = $330
  requests 43.2 M × $0.012/10k                        = $52
  egress ~4 GiB × $0.085                              = $0.34
  → Front Door subtotal                               ≈ $382
Application Gateway WAF_v2
  fixed 730 h × $0.44                                 ≈ $321
  capacity units ~2 CU × 730 h × $0.014               ≈ $20
  → Application Gateway subtotal                      ≈ $341
Static Web App Standard                               = $9
Key Vault: ~100k ops ($0.30) + ~30 HSM-backed keys ($2) ≈ $3
Log Analytics: ~5 GiB × $2.76 (metadata only)         ≈ $14
Blob (events + ops, ~40 GiB hot)                      ≈ $1
```

**Per-tenant monthly estimate, M1 default:**

> **Under review.** The Front Door Premium base is charged per tenant here although §11.1 prices it per
> profile per region and §2 describes the inventory as shared by all tenants in a region;
> `azure/COST-FINDING.md` records that contradiction and the two possible resolutions. The PostgreSQL
> line is charged per tenant on the same footing, against the same §2 statement. ADR 0020 adds the same
> ambiguity for Application Gateway: it is a per-region component shared by that region's tenants, but
> the line below charges its base per tenant like Front Door, so the added $341 is an upper bound on the
> per-tenant share until §11.3's allocation question is decided. The figures below, and those in
> §11.4–§11.6 that derive from them, stand unreconciled until that is decided.

| Line | M1 (default) |
|---|---|
| Container apps + jobs (incl. 2 migrations/month) | $27 |
| PostgreSQL (compute + HA + storage + backup) | $392 |
| Front Door Premium (base + requests + egress) | $382 |
| Application Gateway WAF_v2 (device ingress; base + capacity units) | $341 |
| Static Web App | $9 |
| Key Vault | $3 |
| Log Analytics | $14 |
| Blob — events and operational data | $1 |
| **Subtotal, in-tenant consumption** | **$1,169** |
| Shared regional baseline, allocated (§11.3) | $1–9 |
| Daily per-device health rollups and `mart`/ops growth not captured above | $1–3 |
| **Estimate per tenant per month, M1, 5,000 devices** | **≈ $1,170–1,180** |

**ASSUMPTION:** this is materially above the $300–700/month band quoted in master §1.4, and the reason
is a sizing choice, not a contradiction. The master's band is met by the same model with a **smaller
database** — `B2s` with zone-redundant HA is $31/month of compute instead of $368, which lands the total
at **≈ $490** — or by a `D2ds_v5` **without** zone-redundant HA, which lands it at **≈ $640**. The
$830 figure is the recommended production configuration because zone-redundant HA is the availability
floor for 99.9% ingest. **Action:** master §1.4's band should be read as "sized per tenant, default
production configuration ≈ $490–840 depending on database tier", and either the band or the recommended
SKU should be changed so the two documents agree. This is flagged rather than silently reconciled.

### 11.3 Shared regional costs

These do not vary with tenant count inside a region and are allocated rather than charged.

```
Container Apps environment (VNet-injected, internal LB)   ≈ $50/month
Container Registry Premium + geo-replication             ≈ $60/month
Log Analytics: workspace overhead, alerts, archive       ≈ $25/month   (scales with tenant count too)
Budgets, cost alerts, diagnostic settings                ≈ $10/month
  → shared regional baseline                              ≈ $145/month
  → at 100 tenants: 145/100                                 ≈ $1.45 per tenant
  → at 20 pilot tenants: 145/20                             ≈ $7.25 per tenant
```

The regional baseline is small enough that it is not the reason to add tenants, and large enough that a
pilot with three tenants carries a noticeable fixed cost per tenant. It is **multiplicative per region**
(§3.1): a second residency region doubles the baseline and does not reduce the per-tenant variable cost.
ADR 0020 adds Application Gateway `WAF_v2` as another region-shared component (≈ $321/month fixed, plus
capacity units). §11.2 charges that base per tenant, on the same under-review footing as Front Door, so
it is deliberately not counted again in the baseline above.

### 11.4 A tenant at M3, and the attachment tier

M3 adds stored content: prompt text and attachment bytes, encrypted per object under a per-tenant key
(C15). Two additions to the model.

```
Envelope growth at M2: excerpt ≤ 2 KiB/event, on the subset that matches policy
  12,000 events/day × 2 KiB × 30 × (match share, assumed 10%)   ≈ 72 MiB/month
  → immaterial to the storage line at any retention; it slightly raises row size
Blob growth at M3: brief §3.1 gives 50–500 GB/year for a tenant that retains attachments
  → average inflow 4.2–41.7 GiB/month
  → steady-state stored volume ≈ inflow × retention in months
     retention 12 months, hot:   50–500 GiB × $0.0184  = $0.92–9.20/month
     retention 24 months, cool:  100–1,000 GiB × $0.010 = $1.00–10.00/month
     retention 12 months, tiered (hot 30 d → cool 90 d → archive):
        ≈ 4.2–41.7 GiB hot × $0.0184                     = $0.08–0.77
        + 12.5–125 GiB cool × $0.010                     = $0.13–1.25
        + 33–333 GiB archive × $0.00099                  = $0.03–0.33
        ≈ $0.24–2.35/month
Additional at M3: grant and retrieval operations, audit volume, egress on retrieval
  grant decisions ~1,000/day × 30 = 30,000 ops           ≈ $0.10
  log volume increase (grant, retrieval, approval paths) ≈ +1 GiB = $2.76
  egress: retrieval is small volumes to a browser       ≈ $0.10
  → M3 operational addition                             ≈ $3
```

| Line | M1 (default) | M3 |
|---|---|---|
| Container apps + jobs | $27 | $27 |
| PostgreSQL (compute + HA + storage + backup) | $392 | $392 |
| Front Door Premium | $382 | $382 |
| Application Gateway WAF_v2 (device ingress) | $341 | $341 |
| Static Web App | $9 | $9 |
| Key Vault | $3 | $3 |
| Log Analytics | $14 | $17 |
| Blob — events and operational data | $1 | $1 |
| **Blob — attachment ciphertext** | $0 | **$1–9** (hot, 12-month retention) — **$0.24–2.35** if tiered |
| Grant/retrieval operations | $0 | $0.20 |
| **Estimate per tenant per month** | **≈ $1,170–1,180** | **≈ $1,175–1,190** |

**The attachment tier is the only line item with an unbounded tail, and the arithmetic shows why the
distinction matters.** At brief §3.1's own upper bound of 500 GB/year with a 12-month hot retention, the
storage line is **$9/month** — under 1.2% of the tenant's cost. The tail is not in the storage rate; it
is in the **accumulation**. The same 500 GB/year held for five years without tiering is 2.5 TB and
$46/month, and the growth continues indefinitely, because nothing in the storage design bounds it. At
M1 there is no attachment tier and no tail at all.

**What bounds it: the per-tenant content budget (brief §4.4).** The budget is enforced server-side in
`control-api` on the grant path — a device asks, the backend decides, and `over_budget` is one of the
four denial reasons (C14). That means the tail is bounded by a configured number rather than by
customer behaviour, and the boundary is visible: grants denied as `over_budget` are a metric (§10.2), so
a budget that is biting shows up as a signal rather than as a gap in the data. **The content budget is
therefore the single most important cost control in the product**, and it is a policy default, not
infrastructure.

**ASSUMPTION:** the default per-tenant content budget is **100 GB of stored ciphertext**, enforced as a
ceiling on grants. The schema's budget mechanism is a different unit: `ops.tenant.content_budget_bytes_per_day`,
a per-tenant ceiling on content bytes accepted **per day**, defaulting to 0. A stored-volume ceiling of
the kind assumed here is therefore not a column that exists today; the daily intake rate is. *Justification:* brief §3.1's attachment range is 50–500 GB/year; a 100 GB ceiling
admits a year at the low end and roughly a quarter at the high end, which is the range in which a
customer can still answer "what exactly was sent" for a recent investigation. The specific number is
Q6's to set; the mechanism is what this document is specifying, and Q6 must close before the first M3
tenant because the ceiling is a contractual number once a customer is sold one.

### 11.5 Fixed tenant cost versus one more device

The comparison that matters commercially, and it is not the one intuition suggests: the tenant in §11.2 is
already a **large-fleet tenant** (5,000 devices), so its fixed cost is the whole of §11.2 and the marginal
cost of one more device is measured against that baseline.

| Change | Δ Monthly | Why |
|---|---|---|
| **One more device** (average user, 3 events/day) | **≈ zero** | Storage: 3 events/day × 1.5 KiB × 30 = 135 KiB/month ≈ $0.0000025. Compute: 3 events/day is 0.00003 events/s, far below any scaling threshold. Requests: 288 batches/month out of 43.2 M |
| **1,000 more devices** (3,000 events/day more) | **≈ $0.01** | The same arithmetic, and it is still below the rounding of any line |
| **5,000 devices with a 10× traffic increase** (120,000 events/day) | ≈ $55–70 | Edge requests and capacity units ×10 (order +$50), Log Analytics +$5–10, blob +$5, Postgres storage +$2, no SKU change at 3 years (the 50M-row partitioning trigger, D2, is 11 years away at this rate) |
| **Tenant at M1, 5,000 devices** | **$1,170–1,180** | §11.2 |
| **Tenant at M3 with 500 GB/year attachments, 12-month hot retention** | **+$9** | §11.4 |
| **Tenant requiring Managed HSM** | **+$3,360** | A pool is ~$4.6/hour regardless of use; it is only justified by a contract that requires vendor-blind key custody |
| **A second residency region** | **+$145 baseline, +$392 per tenant if a tenant is duplicated** | §11.3. Residency is a per-tenant property, not a per-platform one |

**The conclusion the arithmetic forces:** the fixed cost of a tenant is the database and the two edges —
Application Gateway for devices, Front Door for analysts — and it is **~$1,150/month at the recommended
production configuration**. The marginal cost of a device is
effectively zero, and the marginal cost of *content* is the only line that grows without a designed
ceiling. Pricing that follows device count is therefore mispriced against the cost structure, and
pricing that follows content volume is aligned with it. That the brief's §3.1 observation — "the event
data is small and does not need a specialist store; the only unbounded cost is attachment retention at
M3" — is exactly what the model shows is a useful confirmation that the model is not wrong.

**ASSUMPTION:** engineering and support headcount is excluded from this model. *Justification:* this
section exists to price infrastructure per tenant; the operational-burden argument belongs in a staffing
plan. A team of two platform engineers plus one release engineer, on the analysis above, costs several
times the infrastructure of the first ten tenants — which is the honest statement of where this
product's cost actually is.

### 11.6 Sensitivity: what changes the bill most

| Driver | Effect | Lever |
|---|---|---|
| **Attachment retention at M3** | **Unbounded** without a ceiling; $9/month at 12 months hot, $46/month at 5 years untiered, and rising | Per-tenant content budget (brief §4.4, Q6) plus lifecycle tiering. **The only line with no natural bound** |
| **PostgreSQL HA configuration** | $368 → $184 (no HA) → $31 (`B2s` + HA) | The single largest addressing decision: zone-redundant HA is what makes 99.9% ingest defensible; removing it saves $184/month and accepts a failover-shaped outage |
| **Residency regions** | +$145 baseline and +$392 per tenant per additional region | Region count is a product decision (Q1), not a tuning decision |
| **Managed HSM** | +$3,360 per pool, ~4× the entire rest of the tenant | Per-contract only. If a customer's requirement can be met with Key Vault Premium in a dedicated vault, the saving exceeds everything else in this table combined |
| **Log Analytics verbosity** | $14 at M1 with sampling; ×5–10 without | Sampling, retention tiering and log-level discipline (§10.4). Grows with verbosity, not with traffic, so it is entirely under the vendor's control |
| **Front Door base fee** | $330, a large fixed fee before a single request (under review per `azure/COST-FINDING.md`: the base is per profile per region) | Fixed and unavoidable in this design, and the reason adding tenants is accretive |
| **Application Gateway `WAF_v2` base** | ≈ $321 plus capacity units, a second per-region fixed fee on the device side | New under ADR 0020. Fixed and unavoidable if devices authenticate through Application Gateway; it is why the device edge now appears twice in the fixed cost |
| **Event volume growth** | 10× traffic adds ~$55–70 | Only relevant if the brief's constraints are violated (C9 forbids per-keystroke capture). Not a lever; a boundary condition |
| **Reservations or savings plan** | 20–40% off compute lines, i.e. ~$80–160 per tenant | Deliberately not taken in v1 (§2.2). Becomes available once the fleet is real |

---

## 12. Disaster recovery and business continuity

### 12.1 Targets, with reasoning

| Component | RPO | RTO | Reasoning |
|---|---|---|---|
| Collection on devices | **0** | n/a | The spool is the DR plan for the last mile: bounded local buffering, encrypted at rest, dropping oldest only when full and counting the drop (C22, brief §7). A platform outage of a working day costs zero events *as long as* the spool's capacity exceeds the outage — which is why spool capacity is sized in hours of normal operation and stated to customers |
| Ingest availability | 0 | 15 min | Stateless container apps; Application Gateway origin health probes remove a failed revision. Devices retry with backoff and jitter through the outage |
| PostgreSQL, in-region | ≤5 min | **60 min** | Zone-redundant HA gives an automatic failover to the standby in the second zone; PITR at 5-minute granularity bounds loss. 60 min is a target, not a guarantee: the failover itself is minutes, and the remaining time is verification and, if PITR is needed, restore time proportional to the WAL to replay |
| PostgreSQL, cross-region | ≤15 min | **4 h** | Geo-restore from geo-redundant backups into the paired region, then verify, then repoint. Stated as hours because it is hours — a number under an hour here would be a claim the drill has not yet earned (§12.4) |
| Ciphertext blobs | ≤15 min | 30 min | RA-GRS replication; the secondary is readable. Object-level RPO is bounded by replication lag |
| Derived aggregates (`mart`) | n/a | 30 min | **Rebuildable by definition** (master §5.3): `aggregator` recomputes every bucket from `ingest` by replacing it (C28). Losing `mart` is an inconvenience measured in minutes, not a data-loss event |
| Configuration and audit (`ops`) | ≤5 min | 60 min | Rides the same PostgreSQL PITR. Audit entries are append-only and are the one table whose loss has a compliance consequence rather than an operational one |
| Dashboard | n/a | 30 min | Static, redeployable from the registry's geo-replica |
| Classifier content | 0 | 5 min | Signed content served by `control-api` from storage; devices fall back to their last verified bundle and never to an unsigned one (C10) |
| **Attachment content whose key was destroyed** | **Not applicable** | **Never** | §12.5. This is by design (D1, C15). For a `full_text` tenant the search index is a separate plaintext-derived copy that key destruction does not reach; it is removed by row deletion (06-security §6.4) |

The reasoning behind the two numbers worth defending:

- **Ingest RPO of 0 is achievable because the device holds the data, not because the server is
  redundant.** The server can lose everything recent and the events still exist on 5,000 machines with
  a bounded spool. This inverts the usual DR priority: the server's durability matters less than the
  spool's capacity, and the spool's capacity is a fleet-wide parameter stated in §12.4's rehearsal.
- **The cross-region RTO is 4 hours because the drill has to prove it before it can be promised.**
  Publishing a 60-minute cross-region RTO before the first full rehearsal would be exactly the kind of
  unearned number that the brief's R11 logic warns about: a coverage claim that is not measured.

### 12.2 Backup and point-in-time recovery

| Item | Setting | Notes |
|---|---|---|
| PostgreSQL automated backups | Enabled, **35 days** PITR, geo-redundant | 35 days covers an investigation that starts late, which is the normal case for a discovery product |
| Backup retention vs erasure | Stated contractually | **A backup cannot be selectively edited.** The product says so, and the promise is bounded (§12.5, §14): erasure is immediate in the live system and complete in backups within the backup retention window |
| Manual restore | Into a **side** server, never over the live one | Restores are for investigation and for §3.4's migration repair path. Promoting a restored server is a decision, not a step |
| Blob versioning + soft delete | Versioning on; soft delete 30 days | A ransomware or defect path that overwrites ciphertext is recoverable; an *erasure* is not, because erasure deletes versions and the keys |
| Key Vault | Purge protection on; soft delete 90 days | **Purge protection is not optional.** Without it, a key deletion is a tenant's data loss, and §12.5's unrecoverable class becomes reachable by accident rather than by decision |
| Key backup | Public key only; **never the private key material off the HSM boundary** | A backed-up KEK outside the HSM is a stored credential, which §5 forbids |
| Infrastructure | Reproducible from Bicep + parameter files, pinned by commit | No infrastructure backup exists or is needed; that is the point of §3 |
| Restore verification | Automated, on every restore: row counts, tenant row-level-security spot checks, sample content decrypt for one object per tenant | A restore that is not verified is a hope |

### 12.3 Blob redundancy and lifecycle

- **Redundancy:** RA-GRS for ciphertext (the readable secondary is the point of RA over GRS); ZRS for
  exports. The secondary is in the paired region, and reading from it is an explicit, audited action —
  not an automatic failover, because a tenant's residency commitment is about where the data is
  processed, and silently failing over across a residency boundary is a compliance event.
- **Lifecycle:** ciphertext hot for 30 days (the window in which an investigation almost always starts),
  cool at 30 days, archive at 90 days, deleted at the tenant's retention class. Exports: cool at 30 days,
  deleted at 90 unless the customer has taken delivery, because delivery is the customer's copy.
- **Object lock / immutability:** considered and **rejected for ciphertext**, because it would prevent
  subject erasure from completing (brief §3.4 requires a verifiable receipt, and immutability makes the
  receipt unkeepable). It is used only where a customer contract requires it and where the erasure
  commitment has been renegotiated in writing.
- **Erasure:** erasure deletes object versions and the wrapped key material, and writes the receipt
  (what was removed, when, by which mechanism — C34, brief §3.4). Two independent mechanisms must agree
  that the object is gone; `reconciler` reports where they disagree and never auto-corrects silently.

### 12.4 The restoration drill

| Drill | Cadence | Proves | Evidence |
|---|---|---|---|
| PostgreSQL PITR restore into a side server | **Monthly, automated** | Backup integrity; that a restore reaches a usable state without manual repair | Restore record: timestamps, row counts, spot checks |
| **Full DR rehearsal: lose a region** | **Twice a year, manual, on-call and a second engineer** | The §12.1 cross-region numbers are real: geo-restore, repoint, redeploy, verify | Signed rehearsal report, including what took longer than expected |
| Spool-capacity rehearsal | Twice a year | That the fleet can hold a working day of collection: measured spool depth over a simulated 24-hour ingest outage on the internal ring | Spool depth curve, drop count (must be zero) |
| Erasure and receipt rehearsal | Quarterly | That a subject erasure completes, that both mechanisms agree, that the receipt is produced, and that the restored-from-backup path replays the erasure ledger before promotion | Sample receipt plus reconciliation output |
| Key-destruction rehearsal | **Once, before the first customer-held-key tenant, and then quarterly in a non-production vault** | That §12.5's promise is exactly true: what is destroyed, what remains, what the receipt says | Signed-off test transcript with a named customer-facing statement |
| Uninstall rehearsal | Per release, on the target ring | §6.4 leaves nothing behind | Self-test output |

### 12.5 What is unrecoverable, stated as a promise

This section exists because the product makes a promise, and a promise that is not written down in the
operational document is a promise that will be broken by accident.

| Data | Recoverable? | Mechanism, or why not |
|---|---|---|
| Event metadata (tool, time, size, destination, digest, labels, policy decision) | **Yes** | PostgreSQL PITR and geo-restore (§12.1). This is the product's core record and it is protected by the ordinary backup path |
| Derived aggregates and findings | **Yes, and cheaply** | Rebuilt from `ingest` (master §5.3). Nothing in `mart` holds human workflow state, so a rebuild destroys no analyst judgement |
| Configuration, policy history, audit entries | **Yes** | Same PITR path |
| Ciphertext blobs, keys intact | **Yes** | RA-GRS plus versioning, provided the per-object wrapped key material in PostgreSQL is also restored — **the two must be restored to the same point in time**, which is why the drill restores both and verifies a sample decryption |
| **Content whose per-tenant key has been destroyed** | **No. Never. By design.** | Destroying the customer's KEK destroys the ability to unwrap every per-object key under it; the ciphertext remains, and is permanently meaningless. This is what makes "the vendor cannot read content" a fact rather than a policy, and it is the mechanism for customer-held-key mode and tenant offboarding (brief §4.4, C15; D1) |
| **Events already deleted by retention or erasure** | **No**, once the backup retention window has passed | Deletion is deletion (D1), and the receipt says what was removed. Within the backup window, a restore would resurrect them — which is why the erasure ledger is replayed onto any restored database **before it is promoted** (§12.2) |
| **Local content on a device that was never granted** | **No** | It never left the device. If the device is wiped, it is gone. This is a consequence of the product's central property, and it should be in customer-facing documentation rather than discovered |
| **Spooled events on a device whose spool overflowed** | **No** | Drop-oldest with a counter (C22). The loss is *counted*, which is the brief's actual requirement — silent loss is the defect, not loss |

**The operational consequences, in the order they matter.** Key destruction is a two-person action with a
written pre-condition: whoever destroys a tenant KEK is destroying data irreversibly, so §13's runbook
requires a named request, a second approver, a stated scope and a receipt saying what is now unreadable.
Purge protection and soft delete are what keep "irreversible" a decision rather than an accident, which is
why §12.2 makes both mandatory. A restore **must replay the erasure ledger before promotion**, or DR
resurrects data the customer was told was deleted — a compliance failure caused by a recovery procedure,
which is the worst kind because it happens when everyone is already having a bad day. And blob and
database must be restored **together**: a blob restored to T1 with keys restored to T2 produces ciphertext
whose key existed only in a state that was never live, which is exactly what the drill's sample decryption
is there to catch.

---

## 13. Runbooks

Each row is **trigger → immediate action → verification → rollback**. Every runbook names the audit
entry it produces, because an operational action taken without a record is indistinguishable from an
accident afterwards.

| Runbook | Trigger | Steps | Verification | Rollback / exit |
|---|---|---|---|---|
| **Ingest outage** | Ingest 5xx > 1% for 5 min, or the availability SLO burning | 1. Confirm scope (one region, one tenant, all). 2. Check Application Gateway origin health and WAF block rate before touching the app. 3. If a revision regressed, return traffic to the previous revision. 4. If PostgreSQL is the cause, go to *database failover*. 5. Do **not** raise device retry rates — devices already back off and spool | Batch success rate recovering; spool depth on devices falling rather than rising; **zero** increase in `dropped_total` | Previous revision restored; if the outage continues, devices keep spooling and the spool-capacity rehearsal (§12.4) says how long is safe |
| **Database failover** | Primary unreachable, or an HA failover event | 1. Do **not** restart the container apps; let connections re-establish. 2. Confirm the new primary and its zone. 3. Verify row-level security is still forced on every session (a failover does not change it; a *restore* might). 4. Check `aggregator`'s watermark and re-run the affected window if it failed mid-run | Writes succeeding; aggregate freshness recovering; no duplicate `(tenant_id, event_id)` rows | None needed — HA failover is the designed path. Escalate if it does not complete in 60 min (§12.1) |
| **Key Vault outage, or a customer key disabled** | Unwrap failure > 1% for 10 min; or a key state change event | 1. Separate the two cases: platform outage (all tenants) versus a customer disabling their own key (one tenant). 2. Platform: metadata and dashboards keep working; retrieval is degraded; **do not** attempt to decrypt by any other path — there is none, by design. 3. Customer key: notify the customer contact immediately, because their content is unretrievable until they re-enable. 4. Check whether retention is about to expire on unretrievable objects and whether a hold should be applied (C35) | Retrieval path returns an explicit key-unavailable error (never an empty result, C17); no tenant is silently returning empty content | Content retrieval only. If a key was **destroyed** rather than disabled, §12.5 applies: the content is gone and the notification says so |
| **Revoked-credential storm** | > 50 revocations in 1 h | 1. Determine whether it is one tenant (incident response, or a defect) or the fleet (a defect or a bad rollout). 2. If a rollout, halt the ring (§9.2) and check whether revocations correlate with the new version. 3. If one tenant, verify the actor is the customer and confirm the audit trail. 4. Ensure revoked devices are rejected and marked, and that their spool is not silently lost — the device stops sending and retains, which is visible as a device in `revoked` | Revocation rate returning to baseline; re-enrolment (C11) returns the **existing** identity rather than creating duplicates | If caused by the release: rollback per §9.4, and re-enrol the affected devices idempotently |
| **Coverage collapse on one provider** | Provider reporting share down > 20% relative, fleet-wide, 30 min | 1. Identify the provider and its platform(s). 2. Check whether a content or policy release correlates (§9.3 — this is usually content, and content is reversible in minutes). 3. Check device health error codes for a specific failure rather than a general one. 4. If it is the **egress proxy** or the **loopback broker**, treat as Sev-1: egress may be broken for users, and the loopback broker may be holding a port it cannot serve (E14) | Reporting share recovering; per-mode coverage back to baseline; for egress/broker, user-reported network/local-AI function restored | Disable the provider by kill switch (§9.5) — collection degrades by one coverage row rather than the product failing. Never leave a user's machine broken to preserve collection (E8) |
| **Spool saturation, fleet-wide** | Devices with spool > 80% > 5% of fleet | 1. This is almost always an upstream ingest problem; fix that first. 2. Confirm the drop counter is incrementing and that it is being reported (it must be — C22). 3. Do not increase spool caps remotely as a first response: a larger spool with a broken path only delays the drop. 4. Notify affected tenants with the undercount figure, before they notice it in the dashboard | Drop counters stop rising; spool depth draining; undercount reported per tenant | None: dropped events are gone. The obligation is that the loss is **visible and attributed**, which is the requirement (brief §7) |
| **Certificate expiry** | 90/60/30/14-day alert, or a failed signature verification | See §7.5 — the full procedure | Expiry date moved; new artefacts verified on a clean machine | Freeze endpoint releases until a valid certificate exists; content releases continue throughout |
| **Kill switch use** | On-call judgement, unilaterally | 1. Scope as narrowly as the incident allows: provider × platform × tenant before fleet-wide. 2. Activate; note the expected fleet latency (one policy poll). 3. Confirm adoption by watching the affected provider's coverage share fall and no other provider's coverage change. 4. File the audit entry with the incident reference **before** the end of shift | Coverage share for the disabled provider falls to ~0 within one poll interval; every other provider unchanged; user-reported breakage stops | Deactivate by publishing the previous policy scope. **Never** edit a signed policy by hand — the reversal goes through the same signed path (C10) |
| **Closing a gate (suspension)** | **A human decides; nothing is automated** (ADR 0015). The trigger is commercial: an unpaid invoice, a contract dispute, or a customer asking for a pause | 1. Read `mart.v_tenant_suspension_impact` **for that one tenant** and put the numbers in the ticket: devices enrolled, devices reporting, events spooled on devices, events already dropped. 2. Decide the two gates **separately** — `read_enabled = false` is fully reversible; `ingest_enabled = false` is not, because nothing else observes this usage (brief §1) and spooled events drop oldest-first (C22). 3. Default to reads-only unless a contract says otherwise. 4. Set the gate together with `status_reason`, `status_changed_by` and `status_changed_at` — the schema refuses the change without them. 5. Tell the tenant contact the spooled-event figure, so the undercount is disclosed rather than discovered | Gate state and its attributed reason visible on the tenant record; if reads-only was chosen, ingest continues and coverage is unchanged; `dropped_total` **not** rising | Fully reversible for `read_enabled`. For `ingest_enabled`, reopening the gate is reversible but the events dropped while it was closed are gone — the runbook says so at the point of decision, which is the only place it can be said usefully |
| **Tenant offboarding** | Signed offboarding request | 1. Two-person approval (§4.5). 2. Revoke all devices. 3. Deliver the contractually owed export and record delivery. 4. Apply the retention and erasure path per the tenant's contract, producing receipts. 5. **Destroy the tenant KEK** — the last step, and irreversible (D1, §12.5). 6. Delete blobs, drop the tenant's rows and derived aggregates | KEK gone (Key Vault and HSM where applicable); sample decryption fails; no tenant rows in `ingest`, `ops`, `mart`, or blob; receipts produced for each removal mechanism | There is none for step 5. Steps 1–4 are reversible until the key is destroyed, and the runbook says so explicitly at each step |
| **Reported or detected reconciliation drift** | `reconciler` reports drift, or a customer challenges a count | 1. Do **not** correct automatically (C34). 2. Quantify: observation-level versus submission-level counts, non-reconcilable digests, expiry-mechanism disagreements. 3. Determine which of the three it is — dedup merging (R9/Q3), route attribution, or expiry disagreement between the two mechanisms. 4. If a customer count is affected, tell them **before** they find it, with the explanation: observation-level and logical-submission counts, and which route won. 5. File the drift as a defect with the affected window | Drift explained and bounded; if it was a code defect, a regression test at the contract level (§4.2) | No automatic rollback: a correction that erases evidence is worse than a wrong number that is explained. Manual correction is a reviewed, audited data operation with the drift record as its input |

---

## 14. Go-live gates

Expressed as **evidence**, not tasks. Each line is a thing that exists and can be shown to someone who
was not in the room. The first paying tenant is the point at which the product's promises become
contractual, so anything asserted without evidence becomes a liability at that moment rather than a
gap.

### 14.1 Open questions that must be closed first

| # | Question | Closing evidence required | Why it blocks |
|---|---|---|---|
| **R1 / Q5** | Local-inference capture is unvalidated against real tools (brief §5.3, §9) | Lab report against the tools customers actually run, on all supported platforms (Linux is a new target and is not yet measured): which servers can be moved off default ports, which clients resolve a substitute, and the port-release behaviour under crash, kill and repeated failure | Mode F's failure mode breaks the user's local AI (E14). If it is not validated, mode F ships as detection-only — **and that changes a capability claim**, so it cannot be validated after go-live |
| **Q1** | Data residency | A written answer per design-partner tenant, and evidence that the region is enforced at ingest with a fail-closed check | The region column and the check are cheap now (master §7). Discovering the requirement after the data model is fixed is the expensive version |
| **Q2** | The organisational dimension | Either SCIM provisioning from the customer's identity provider proven against a real tenant, or an explicit written scope-out | Questions 2, 3 and 8 of brief §3.6 cannot be answered without it. Scope-out must be in the contract, not in a footnote |
| R3 | Accessibility pre-granting may be changing | Confirmation against current platform documentation on a supervised test fleet | No v1 mechanism depends on it (D4), but any coverage claim that implies it must not be made |
| R2 | Apple restricted entitlements | Filed applications, with the acknowledgement that v1 needs none | Schedule dependency, not a technical one — but the coverage upgrade timeline depends on it |
| R6 / Q7 | Classifier precision on real prompts | Evaluation set built from consented pilot data, with per-class precision and recall reported, and thresholds set from it | A noisy classifier makes the product worse than no product (brief §9) |
| Q6 | Default retention per class and the per-tenant content budget | Values set as policy defaults, with the budget enforced and `over_budget` denials observable | This is the only mechanism that bounds the product's only unbounded cost (§11.4) |

### 14.2 Delivery gates

| Gate | Evidence |
|---|---|
| Contract pipeline | CI green on the staleness check, the fixture validation gate (including every deliberately-invalid fixture), and the Go/TS round trip (§4.2) |
| Cloud spine | A full end-to-end pipeline answering all ten of brief §3.6's questions from synthetic data, deployed by CI, in staging and production (master §6 step 1) |
| Honesty layer | A dashboard that reports coverage, spool drops, degraded-classifier share, devices not reporting, dedup drift and aggregate freshness — **demonstrated on a fleet with deliberately broken providers**, not on a healthy one (C22–C25, R11) |
| SLO instrumentation | §10.1's SLOs live with 28 days of history, and every §10.3 alert fired at least once in a test, with the customer-meaning text reviewed |
| Identity | A demonstrated attempt by `ingest-api`'s identity to unwrap a KEK that **fails**, and the CI assertion that only `content-vault` holds unwrap (§5.3) |
| Endpoint distribution | MSI, PKG and Linux package installed, self-tested and cleanly uninstalled on all supported platforms on the internal ring, with the uninstall verification script's output as evidence (§6.4) |
| Extension | Force-installed by policy on Chrome and Edge at one customer, with the `max_version` pin exercised at least once (§6.3) |
| Signing | Rotation pipeline dry-run completed for the Windows and macOS certificates and for the Linux packaging key; credential inventory populated with 90/60/30/14-day alerts proven by test; **timestamping verified on an artefact whose certificate has been allowed to lapse in staging** (§7.2) |
| Reputation | Exclusion artefact shipped as a versioned deliverable and accepted by at least one customer's EDR review; a written pilot-expectations script handed to at least one customer's helpdesk (§8.2) |
| Update safety | A ring promotion performed and **automatically halted** by an injected regression (crash and coverage variants), with rollback completed and timed (§9.2, §9.4) |
| Kill switch | Activated and reversed in production during a rehearsal, with the fleet-wide latency measured rather than assumed (§9.5) |
| Classifier reversal | A bad classifier release reversed **without shipping code**, end to end, timed (§9.5, C20) |
| DR | Monthly PITR restore green; one full regional-failure rehearsal completed and signed, with the measured RTO replacing the target in §12.1 (§12.4) |
| Key-destruction rehearsal | Completed in a non-production vault, with the resulting customer-facing statement reviewed by legal (§12.4, §12.5) |
| Cost | The §11 model re-baselined against actual Azure spend for one full month at pilot scale, with the variance explained line by line |
| Legal and privacy | The excluded workstream (brief scope note) signed off: notice version recorded per user (brief §3.2), retention defaults approved, the processing record filed, the residency answer recorded |

### 14.3 Contractual promises that must not be made yet

Each of these is a statement the product will be able to make — later, and only after the enabling work
exists. Making it now would be a defect in the sales process rather than in the software, and it is the
failure mode the brief's §5.5 warnings exist to prevent.

| Do not promise | Until |
|---|---|
| "We capture every way your staff reach an AI model" | Coverage per usage mode is measured (R11) **and** the unobserved remainder (brief §5.5's 70–85%) is disclosed in the same breath |
| "Full content capture on every desktop application" | E8's boundary is measured per customer. Without a kernel-mode component (D3), coverage stops at applications that honour system proxy settings — and that is a *measured* boundary, not a hidden one |
| "Local AI capture" | R1 is closed against the tools the customer actually runs |
| "Zero-touch macOS deployment" | It is true for the components, and **never** for Screen Recording permission (E16). Nothing depends on it, and the enrolment experience must be described accurately |
| "Your users will see no prompts during rollout" | SmartScreen reputation accrues over weeks (E20). Pilot users **will** see "unrecognised application" prompts, and the engagement must say so before the first install |
| "No security-tool interference" | The interceptor resembles a man-in-the-middle pattern to other endpoint products (E22, R4). The honest promise is the exclusion artefact, the bake period, and a named support path |
| "Content search" as a universal feature, or alongside "we cannot read your content" | Content search is a per-tenant capability (ADR 0014, which supersedes ADR 0008; 06-security §6). `full_text` makes prompt text vendor-readable, requires M3, and cannot be stored together with customer-held keys; a customer-held tenant gets `attachment_names` at most, and for anything more its route is the Parquet export, in the customer's storage, under the customer's keys (C31). Promise the tier the tenant's custody mode admits, with that consequence stated |
| "Deletion from backups on request" | Backups cannot be selectively edited. The promise is the §12.5 wording: immediate in the live system, complete in backups within the retention window, and enforced on any restore |
| "Nothing is ever lost" | Spool overflow drops oldest and **counts it** (C22). The promise is that an undercount is always visible, which is a stronger and truer statement |
| "An outage costs you no data" | True up to the spool's capacity, which is a stated number in hours (§12.4). Beyond it, data is dropped and counted |
| "SOC 2 / ISO 27001 certified" | Until the audit exists — and no audit scope should be quoted before the retention, residency and erasure wordings in §12.5 and §14.2 are final, because those are what an auditor will ask for |
| Any specific RTO or RPO | The rehearsal in §12.4 has been run and the target has been replaced by a measurement |

---

*Companion documents: [00-architecture](00-architecture.md) · [01-collectors](01-collectors.md) ·
[02-ingest-and-transport](02-ingest-and-transport.md) · [03-data-platform](03-data-platform.md) ·
[04-dashboard-and-query](04-dashboard-and-query.md) · [06-security-and-threat-model](06-security-and-threat-model.md)*
