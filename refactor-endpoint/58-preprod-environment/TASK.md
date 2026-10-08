# 58. Pre-prod environment and test tenant

Needs:
- the build tasks the owner wants verified merged to `main`, because pre-prod deploys from
  `main` (`AGENTS.md`, the device phase);
- the owner available for every step marked **owner**: they create things in Fly.io, Supabase,
  DNS, Entra, GitHub and Intune that an agent may not.

## Problem

Every on-device check in this plan runs against pre-prod, never the local lab, in the device
phase (`AGENTS.md`). Pre-prod doesn't exist yet, and it runs on SaaS (`DECISIONS.md`,
2026-10-07): the services on Fly.io, PostgreSQL on Supabase. Intune and Entra are unchanged. The
repository has no deployment for either service, so this task writes one, then brings pre-prod up
with the owner. It opens the device phase: tasks 59 and 60 follow it.

## Goal

A push to `main` deploys pre-prod to Fly.io and Supabase, and pre-prod passes the Verify checks in
`fly/RUNBOOK.md`. It has a test tenant, linked to the reference VM's Entra tenant, that agents may
create data in. The owner's own tenant (`11111111-1111-1111-1111-111111111111`) stays untouched.
The values later tasks need are recorded in `refactor-endpoint/TESTBED.md`.

## Design

Fixed by this brief. Verify each Fly.io and Supabase detail against their current documentation
before relying on it, and record what you checked in `DECISIONS.md` (`AGENTS.md`).

- **Apps.** One Fly.io app per component, named `<prefix>-<component>` (`TESTBED.md`), in region
  `iad`:
  - `edge`: the device entry point. A raw TCP service on port 443 with no handlers, on a dedicated
    IPv4 address, so the edge itself terminates TLS, requests the client certificate and forwards
    it in `X-Client-Cert`.
  - `dashboard`: an HTTP service on the analyst hostname, with a Fly.io-managed certificate.
  - `ingest-api`, `control-api`, `content-vault`, `query-api`: no public service. They are reachable
    only on the organization's private network, by the edge and the dashboard.
  - `jobs`: one machine running `jobs aggregate`, `jobs erase` and `jobs expire` on the schedule in
    `services/jobs/README.md`, with supercronic (Fly.io's documented cron). Exactly one machine, so
    no job runs twice.
  - `migrate`: an app with no standing machine, holding the administrator's secrets. The deploy
    workflow runs the `migrate` image in it once per deploy.
- **Settings.** Each app gets the settings its component's README lists. In addition:
  - `SAC_REGION=eastus`, the residency region tenants are pinned to.
  - Every service listens on `[::]:8080` (`SAC_HTTP_ADDR`), because Fly.io's private network is
    IPv6. Services reach each other at `http://<prefix>-<component>.internal:8080`.
  - control-api authenticates as the vendor Entra application with `SAC_ENTRA_CLIENT_SECRET`, not
    `SAC_ENTRA_FIC`.
- **Database.** A Supabase project on the Pro plan in East US (North Virginia), next to `iad`.
  - Logins have passwords. `migrate` creates logins only where the server offers Entra database
    authentication; elsewhere it expects them to exist and grants each its role. So the logins
    (`ingest-api`, `control-api`, `content-vault`, `query-api`, `jobs`) and an administrator with
    `CREATEROLE` that owns the product's database are created once beforehand, in the same shape
    as the lab's `db-setup` (`localdev/compose.yaml`).
  - The product's database: the lab names it `shadow`. If Supabase's pooler can't reach a database
    other than `postgres`, use `postgres`, and record it in `DECISIONS.md`.
  - Connections go through the session pooler: it is reachable over IPv4 and supports the prepared
    statements the Go services use. The pooler's user name is `<login>.<project-ref>`. TLS is
    required (`SAC_PG_SSLMODE=require`).
- **Secrets** are Fly.io secrets, each set only on the apps that read it:

  | Secret | Apps |
  |---|---|
  | Device CA certificate (`SAC_CA_CERT_PEM`) | ingest-api, control-api |
  | Device CA key (`SAC_CA_KEY_PEM`), directory key (`SAC_DIRECTORY_KEY`) | control-api |
  | Session and policy signing keys, as files at `/mnt/secrets/sac-session-signing-key` and `/mnt/secrets/sac-policy-signing-key` | control-api |
  | Entra client secret (`SAC_ENTRA_CLIENT_SECRET`) | control-api |
  | Internal token (`SAC_INTERNAL_TOKEN`) | control-api, dashboard |
  | Content keys (`SAC_CONTENT_KEYS`) | content-vault |
  | Cursor key (`SAC_CURSOR_KEY`) | query-api |
  | Database password (`SAC_PG_PASSWORD`) | each app with a database login, its own only; `migrate` gets the administrator's |
  | Device hostname certificate and key (`EDGE_TLS_CERT_PEM`, `EDGE_TLS_KEY_PEM`) | edge |
- **Browser routes.** The dashboard is the only browser-facing app, and it forwards `/onboard/*` to
  control-api. Pre-prod doesn't serve `/scim/v2/*` or `/.well-known/*` to browsers, because
  endpoint tests don't use directory sync. Record that in `DECISIONS.md`.

## Scope

Two parts: build, then bring up after the owner merges (`AGENTS.md`, the device phase).

### Build (agent, on `refactor-endpoint/58-preprod-environment`)

1. **`services/edge/`**: move `localdev/edge` here as a product component, with a README in the
   repository's component style.
   - Point the lab's image list (`localdev/lab.mjs`) at its Dockerfile. `tools/accept.mjs` tests
     every tracked Go module, so its tests keep running.
   - Before devices reach it: add server read, write and idle timeouts and a maximum header size,
     each a constant.
   - The allow-list, the certificate forwarding and the discarding of client-supplied
     `X-Client-Cert`, `X-Forwarded-Proto` and `X-Forwarded-Host` stay exactly as they are, with
     their tests.
   - Add its row to the repository `README.md`'s table.
2. **`fly/`**: the deployment.
   - A `fly.toml` per app.
   - `fly/jobs/`: a Dockerfile from the `jobs` image that adds a pinned supercronic release
     (checksum verified), and its crontab.
   - `fly/scripts/setup-database.sh`: creates the administrator, the five logins and the database
     once, with generated passwords, and sets each password as its app's secret. It prints no
     password, and refuses to run if the logins exist.
   - `fly/scripts/create-secrets.sh`: generates the environment's keys and token, in the formats
     the components read, and sets each on the apps in the table above.
     - It never replaces a secret that already exists.
     - It prints the policy public key for `SAC_POLICY_PUBLIC_KEY`.
     - It tells the owner to keep a copy of every generated key outside Fly.io (`DECISIONS.md`).
   - `fly/scripts/tenant-admin.sh <args>`: runs `control-api tenant <args>` in the control-api app
     (`fly ssh console -C`) and prints its output.
   - `fly/RUNBOOK.md`: the owner's sequence from empty accounts to a managed device whose events
     appear on the dashboard. It ends with a **Verify** section:
     1. Every app's machines are started and pass their checks.
     2. Only the edge and the dashboard have public addresses (`fly ips list`).
     3. `curl https://<device hostname>/v1/health` reaches control-api, and
        `curl https://<device hostname>/admin/v1/` is refused by the edge.
     4. The analyst hostname serves the dashboard's sign-in page with a valid certificate.
     5. The deploy run's `migrate` step succeeded, and a second run logs nothing to apply.
     6. The jobs app's log shows `aggregate`, `erase` and `expire` running without errors.
     7. On a managed device the app installs, the deployment key's enrolment count rises, and the
        device appears on the dashboard's Devices page by hostname.
     8. After prompts from the device: submissions on Tools, Users and Data classes, and a prompt
        with a test card number raises a finding.

     For the vendor Entra application and for devices (Intune), the runbook points to the existing
     steps: Entra and Intune are unchanged.
3. **`.github/workflows/deploy.yml`**: on a push to `main`, deploy pre-prod to Fly.io.
   - Keep the `agent-release` job as it is: artifact `agent-release`, version
     `1.0.<run number>`. Task 59 depends on both.
   - Replace the jobs that deploy `azure/` with ones that:
     1. push the images to Fly.io's registry, tagged with the commit;
     2. run `migrate`. If it fails, fail the run, with its output in the run's log.
     3. deploy each app with that commit's image.
   - The GitHub environment `preprod` holds `FLY_API_TOKEN` (an organization deploy token), and
     the organization and app prefix as variables. The existing `SAC_*` variables and signing-key
     secrets keep their names.
   - `azure/` stays as it is.
4. **`tools/check-config.mjs`**: check the Fly.io configuration too, so every setting it passes is
   one its component reads.
5. `node tools/accept.mjs` passes. Report **ready to merge**: the branch, what the first deploy
   needs from the owner (Bring-up, step 1), and the checks you will run.

### Bring-up (after the owner merges)

1. **Owner:** `fly/RUNBOOK.md` up to the first deploy.
   - Fly.io organization, Supabase project, DNS names, and the device hostname's certificate from
     a public CA, so devices need no extra trust profile.
   - The vendor Entra application's client secret.
   - The GitHub environment, `setup-database.sh`, `create-secrets.sh` (keeping the key copies),
     and `SAC_POLICY_PUBLIC_KEY`.
   - Then re-run the deploy, or merge again.
2. **Owner:** DNS records for the edge's addresses and the dashboard's certificate.
3. **Owner:** create the read-only token (`fly tokens create readonly`) and save it at the
   `TESTBED.md` path.
4. **Agent, read-only:** Verify checks 1–6, with `fly status`, `fly machine list`, `fly ips list`,
   `fly logs`, `gh run view --log` and `curl`, under `TESTBED.md`'s rules.
   - Report each check's command and result.
   - Confirm `fly logs` works with the read-only token. If it doesn't, stop and tell the owner.
5. **Owner:** the test tenant.
   - Create it with
     `fly/scripts/tenant-admin.sh tenant create --name "Endpoint Test" --ceiling m3 --actor <owner>`.
   - Invite the VM's Entra domain, consent, and sign in as admin.
   - Assign the console user and the second user (`TESTBED.md`) the `viewer` role, and the owner
     `admin`.

   The ceiling is `m3` so tasks can exercise every mode; each task sets the mode it needs on the
   Settings page.
6. **Owner:** from the dashboard, **Settings → Deployment → Download Intune package**, in the zip
   form (MSI plus tenant file).
   - Save its `ShadowAICapture.tenant.env` as `%USERPROFILE%\.sac-testbed\ShadowAICapture.tenant.env`
     on the PC. It holds the deployment key; it never goes in the repository.
   - The agent checks the file names the test tenant and the device endpoint, without printing the
     key.
7. **Owner:** fill in the environment rows of `TESTBED.md`:
   - Fly.io organization, app prefix and read-only token;
   - Supabase project ref;
   - device hostname;
   - analyst hostname;
   - test tenant id;
   - GitHub repository.
8. **Agent:** where a `fly/RUNBOOK.md` step turned out wrong or missing, fix it on
   `refactor-endpoint/58-preprod-environment-fix-N`. Keep the edit short and factual.

## Done when

- `node tools/accept.mjs` passes on the task branch, including the edge's tests and the
  configuration check over `fly/`.
- The deploy run on `main` succeeds: the agent release, `migrate`, and every app.
- Verify checks 1–6 pass, and the report shows the commands and output. Checks 7 and 8 need the
  agent on a device: report them as pending. Task 59's Done when covers check 7; check 8 follows
  once the VM runs the agent (task 60).
- The owner confirms they are signed in to the dashboard as admin of the "Endpoint Test" tenant.
- The tenant file is saved, and `TESTBED.md`'s environment rows are filled in.
- `node tools/accept.mjs` passes on `main`.
