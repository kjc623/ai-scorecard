# AGENTS.md — repository agent instructions

Entry point for agents working anywhere in this repository. **`backlog/AGENTS.md` governs the dashboard
backlog only** (one task per numbered folder under `backlog/`); read this file first, then that one if
you are picking up a backlog task.

## Current focus: a pre-prod Azure deployment for a device test

The repository is being prepared to run a **pre-prod** environment on Azure and test, on real hardware:

1. MDM installation (Intune delivers the agent package)
2. Device enrolment
3. Data reaching PostgreSQL
4. The dashboard

**Nothing has been deployed.** Every Azure claim in this repository is either static-verified
(`azure/tools/`) or a plan. There is no subscription and no observation.

**Start here:**
- [`azure/RUNBOOK.md`](azure/RUNBOOK.md) — the ordered go-live sequence, with the repository location
  named at every step, and the current blockers.
- [`azure/README.md`](azure/README.md) — the exact `az` commands and the static-vs-deployed distinction.
- [`azure/pipelines/`](azure/pipelines/) — `infra.yml` / `drift.yml` / `policy-scan.yml`. They are
  **inert** until installed under `.github/workflows/`.

### Decisions already made

- **Device authentication is the product-issued x509 model ("Shape A").** Intune delivers the agent
  package and a per-tenant deployment key; `control-api` signs a device leaf from the CA in Key Vault
  (`sac-device-ca-cert` / `sac-device-ca-key`) and `ingest-api` re-verifies the forwarded leaf against
  the same certificate. No customer PKI; DPoP is **unwired**. [ADR 0022](docs/adr/0022-customer-issued-device-certificates-are-registered-not-signed.md)
  (customer-issued certificates) is a documented **optional** mode, dormant unless a tenant sets
  `device_ca_pem`.
- **Front Door Premium, not Standard.** The Private Link origin that keeps the Container Apps
  environment private is Premium-only. Standard is documented as an optional cost lever with its
  public-origin deviation in the runbook — do not adopt it silently.
- Two edges: **Application Gateway** is the device ingress, **Front Door** the analyst ingress
  ([ADR 0020](docs/adr/0020-device-transport-is-application-gateway-with-a-pluggable-authenticator.md)).
  `content-vault` is internal-only.

### Still blocking (full list in the runbook)

1. **Services run in-memory.** `azure/main.bicep` never sets `SAC_STORE`, and the production Dockerfiles
   build without `-tags sac_sql_driver`, so nothing persists until both are fixed.
2. **No `migrations` image** and no `reconciler` source — the migration job cannot apply the schema.
3. **No image build/push pipeline** — images must exist in the ACR before a revision can start.
4. **No policy-bundle authoring** and **no signed MSI**; the MSI can only be built on Windows.
5. Operator prerequisites, not code: Intune licensing, DNS/certificates, the Entra app.

## Working rules (all agents)

- Run `node tools/accept.mjs` (or the specific gate) before claiming the repository works. A gate that
  cannot run is reported `SKIPPED`, and **a skipped gate is not a pass**.
- Never commit `.claude/` or `skills-lock.json`.
- Work on a branch; never commit to `main`; do not push or open a pull request unless asked.
- Never run `node localdev/run.mjs` with any flag — it can regenerate the lab CA and orphan an installed
  device. Never change the owner's tenant (`11111111-1111-1111-1111-111111111111`).
- The static checks for this work: `node --test azure/tools/index.mjs` (50 checks),
  `node database/tools/check-schema.mjs`, `node localdev/tools/check-config-agreement.mjs`, the Go suites
  in `control/control-api`, `ingestion/ingest-api` and `endpoint/capture-core`, and the database
  invariants (`database/invariants.test.sql`).

## Where things are

The component map is in [`README.md`](README.md); the design record is [`docs/`](docs/) and its
decisions are [`docs/adr/`](docs/adr/README.md). Backlog task instructions are
[`backlog/AGENTS.md`](backlog/AGENTS.md).
