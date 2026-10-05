# Waiting on the owner

Everything the backlog has left for the owner to run, check or answer, in one list.

Agents add lines and never tick or remove them. The owner ticks a line when it is done. An agent
whose task relies on an unticked line does not assume it happened: check, and if it has not, say so.

Each line names the task that left it. The exact commands are in that task's `REPORT.md`.

## Run

- [ ] **Register the vendor's multi-tenant Microsoft Entra app** (replaces task 11's per-dashboard
  app and its `SAC_OIDC_*` settings, which no longer exist). In your own tenant: App registrations →
  New, "Accounts in any organizational directory (Multitenant)". Web redirect URIs
  `{SAC_PUBLIC_URL}/callback` and `{SAC_PUBLIC_URL}/onboard/entra/callback` (Entra allows `http`
  only for `localhost`). App roles `viewer`, `analyst`, `content_reader`, `admin` (value = name,
  Users/Groups). API permissions: delegated `openid profile email offline_access`; application
  `DeviceManagementManagedDevices.Read.All`; **not** `User.Read.All`. Credential: in Azure, a
  federated credential for control-api's managed identity (`az bicep build` output
  `entraFederatedCredential` has the values); in the lab, a client secret in
  `SAC_ENTRA_CLIENT_SECRET`. Set `SAC_ENTRA_CLIENT_ID` on control-api. (11, enterprise)
- [ ] **Onboard a customer tenant end to end** (your own Entra tenant is the test customer):
  `control-api tenant create --name … --region … --key-custody vendor --ceiling m1`, then
  `control-api tenant invite --tenant <id> --domain <your-domain>`; open the printed link as a
  Global/Cloud Application Admin, consent, sign in once (you become that tenant's admin). In
  Enterprise applications, set "Assignment required" and assign people to the four roles. The
  owner's lab tenant is deliberately not linked to anything: linking it is a write to that tenant,
  so it is yours to do. (11, enterprise)
- [ ] **Set up SCIM for that tenant**: Settings → Deployment → SCIM → Create token (shown once);
  in Entra create a *non-gallery* enterprise app "Shadow AI Capture provisioning" (an app added by
  consent cannot provision until the product is in the gallery), Provisioning → Automatic, Tenant
  URL `{SAC_PUBLIC_URL}/scim/v2`, the token, Test Connection; change the mapping `externalId` to
  `objectId → externalId`; scope "assigned users and groups"; assign; start. Okta: same token,
  header auth, `userName` = the Windows UPN. (11, enterprise)
- [ ] **Apply the enterprise migration to any database other than the lab's**, after 09's:
  `psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/11-sign-in-and-roles/MIGRATION.sql`. Additive and
  idempotent; it also grants the audit-chain read every service role needed (audit inserts failed
  under the real roles before). (11, enterprise)
- [ ] **Create the four Key Vault secrets and deploy**: internal token, directory key, session
  signing key (P-256 PEM), policy signing key (Ed25519 PKCS#8 PEM); commands in `azure/README.md`.
  Keep the policy key's public half: the generic MSI pins it (`installer/release-msi.mjs
  --policy-key`). Losing the directory key loses every tenant's user-reference key. (11, enterprise)
- [ ] **Apply for Microsoft publisher verification, and consider an Entra app gallery listing.**
  Verification removes the "unverified" consent warning that some tenants block; a gallery listing
  lets one enterprise app carry both sign-in and SCIM provisioning. (11, enterprise)
- [ ] **Run task 00** before task 05, on a clean tree at the head of `main`. It expects
  `git status` to show nothing but its own changes. (review)
- [ ] **Rebuild and reinstall the lab MSI on the Windows host**, from the head of the stack, with
  the lab up: `node installer/lab-msi.mjs`, then `msiexec /i installer\dist\ShadowAICapture.msi`.
  One reinstall covers both tasks that asked for it. On 2026-10-04 the real device's row on Devices
  still showed no hostname, agent version or mode, so this had not been done. (02, 04)
- [ ] **Apply the schema changes to any database other than the lab's.** The lab's database has
  had all four applied by the agents. Anything else needs 02's four statements (in its report),
  then `backlog/03-findings/MIGRATION.sql`, then `backlog/04-device-identity/MIGRATION.sql`, then
  `backlog/05-tool-catalogue/MIGRATION.sql`, in that order. (02, 03, 04, 05)
- [ ] **Apply 05's tool catalogue to any database other than the lab's**, after 04's migration:
  `psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/05-tool-catalogue/MIGRATION.sql`. It creates
  `ref.tool_catalogue`, seeds it, adds `ops.tool_display_name()` and grants the sanction write.
  (05)
- [ ] **Apply 06's directory migration to any database other than the lab's**, after 05's:
  `psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/06-directory-sync/MIGRATION.sql`. It adds
  `ops.user_dim.display_name`; the lab's database already has it. (06)
- [ ] **Apply 08's request-kind migration to any database other than the lab's**, after 06's:
  `psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/08-client-generated-requests/MIGRATION.sql`. It adds
  `prompt_kind` to `ingest.observation`, `ingest.submission` and `ops.content_object`, extends the
  three observation constraints, and replaces `ingest.record_event`. The lab's database already
  has it. (08)
- [ ] **Apply 09's prompt-search grant to any database other than the lab's**, after 08's:
  `psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/09-prompt-search-filters/MIGRATION.sql`. It extends
  `sac_vault`'s column grant on `ingest.submission` with `tool_fingerprint`, `collection_mode` and
  `received_at`, so the vault can compose a search filtered by person, tool, device, mode and
  received-at window. It is idempotent, and the lab's database already has it. (09)
- [ ] **Rebuild and reinstall the lab MSI for task 08**, on the Windows host, with the lab up:
  `node localdev/build.mjs --auth` (the lab services changed), then `node installer/lab-msi.mjs`,
  then `msiexec /i installer\dist\ShadowAICapture.msi`. The harness cannot build the MSI (the
  script refuses off Windows); task 08's report has the command. (08)
- [ ] **Point the directory sync at a real Microsoft Entra ID tenant.** Superseded: the Graph user
  pull was removed (the Entra app no longer asks for `User.Read.All`); people now arrive by SCIM —
  see "Set up SCIM" above. `sync-directory` remains for the lab's JSON file only. (06)
- [ ] **Route the minted retrieval URL to the vault in any deployment**, and set
  `SAC_RETRIEVAL_URL_BASE`: the vault mints `GET /v1/content/retrieval/{tenant}/{grant}`, the browser
  fetches it, and the analyst ingress (or the web tier) must forward that path to `content-vault`.
  Now in `azure/main.bicep` (through the new dashboard container app; the vault stays internal) —
  deploy it to close this line. (10)
- [ ] **Assign `Storage Blob Data Reader` on the ciphertext account to the vault's user-assigned
  identity.** Now in `azure/main.bicep`'s ciphertext module — deploy it to close this line. (10)
- [ ] **Rebuild the lab MSI on the new generic path and reinstall**: with the lab up,
  `node installer/lab-msi.mjs`, then double-click `installer\dist\ShadowAICapture.msi` (the lab
  tenant file sits beside it; that is the customer's copy path). This is a major upgrade of the
  agent installed now. (11, enterprise)

## Verify

On the owner's dashboard, `http://127.0.0.1:8787`, with the real device.

- [ ] The dashboard now asks you to sign in; sign in against the real Entra tenant, once as an
  analyst and once as an admin. Confirm the Audit trail names each person's own account, and that a
  viewer's navigation does not offer Search while a content reader's does. (11)
- [ ] **Intune end to end** on a test device: Settings → Deployment → Download Intune package;
  Intune → Apps → Windows → Windows app (Win32); install `msiexec /i "ShadowAICapture.msi" /qn`,
  uninstall `msiexec /x {product code} /qn`, detection MSI product code + version ≥, x64,
  Windows 10 21H2+; assign Required to a device group. Expect: Intune accepts the file (not
  verified anywhere yet), the app installs and is detected, the key's enrolment count rises, the
  device appears. With Device verification = Intune, a device not in Intune is refused
  `device_not_managed`. (11, enterprise)
- [ ] **On that device** (as admin): `capture-core --print-config --config-file
  "C:\ProgramData\ShadowAICapture\profile\capture-core.env" --config-file
  "C:\ProgramData\ShadowAICapture\profile\tenant.env"` shows the Intune device id, the
  `dsregcmd /status` DeviceId and the BIOS serial; `health.jsonl` shows `user_ref_source=upn` and
  the user's UPN; switching user changes it within ~15 s; `policy_fetch` goes `served` then
  `not_modified`; `icacls …\device-ca\ca.key` shows only SYSTEM and Administrators. Repeat on a
  hybrid-joined device. (11, enterprise)
- [ ] **Clean-VM installer checks**: run the MSI from a package folder (installs, enrols); run it
  alone from another folder (fails 1603, Error 1722 `CheckTenantConfig` in the log, nothing
  installed); upgrade to a rebuilt `--version 0.1.1` with no tenant file (keeps it, same device id);
  uninstall (service, folders, root CA and environment entries gone). (11, enterprise)
- [ ] **SCIM from Entra**: provision one user with a department; their events appear under their
  department on Teams; rename their UPN in Entra and confirm their history stays one person;
  unassign them and confirm their dashboard session ends within ~10 minutes. (11, enterprise)

- [ ] After sending prompts from the device: Tools shows tools with submission and people counts
  and a time chart; Users shows a series for `lab-user`; Data classes shows the classes seen; no
  "no_watermark_row" banner. (01)
- [ ] The device shows "Reporting" with a recent last-seen time, and the two cards on Devices
  agree. (02)
- [ ] After the reinstall: the device sends its heartbeat, and its collectors appear as observed
  in the coverage banner. (02)
- [ ] A prompt containing a test card number produces a finding in Search > Findings and on the
  Overview; opening it shows the event; "Open findings" counts it. Task 03 showed this with a
  direct database call, not with the device. (03)
- [ ] After the reinstall: the device appears by hostname on Devices and in search results, with
  its agent version and mode, and the user shown is the person who typed the prompt. (04)
- [ ] New Claude Code traffic from the device shows as "Claude Code" on Tools, in Search rows and in
  prompt search results; a destination the seed catalogue does not hold shows as "Unrecognised tool"
  with its raw fingerprint. Task 05 observed all four existing `tls_*` fingerprints resolving this
  way (three to Claude Code, one unrecognised); the new traffic proves the derivation is unchanged.
  (05)
- [ ] Open a newly captured prompt in Search and confirm the drawer shows it: the browser fetches it
  from `content-vault`'s single-use retrieval URL, not from `query-api`. Task 10 observed this with
  the prompts already stored from the real device (a read only). (10)
- [ ] After the task 08 reinstall, from a **new** Claude Code session: a new "What is the capital of
  Australia" prompt appears in Search with no `source_code` label, and a prompt containing a test
  card number is still labelled `payment_card`. Task 08 already proves the decision on the
  captured bodies in the endpoint's tests and the downstream in the sample tenant. A titling
  request indexed *before* the fix stays searchable (its content object predates the kind), so
  search for "Australia" can still return that older hit until retention removes it; the point to
  check is that the new session's titling request does not appear. (08)

## Answer

- [ ] **Which mode does the Devices "Mode" column show?** It was built on the agent's
  recommendation (the device's effective base mode) because the question was left open. Task 12
  displays the same value. (04)
- [ ] **How do you sign in to your own lab tenant's dashboard (port 8787)?** The lab's stand-in
  sign-in provider now serves only the sample tenant, so 8787 has no way in until your tenant is
  linked. Options: link it to your real Entra tenant (the onboarding line above, with
  `SAC_PUBLIC_URL=http://localhost:8787`); link it to a second stand-in provider; or run 8787 in
  the development-principal mode it had before task 11. Each is a write to, or a weakening of, your
  tenant, so it is yours to choose. (11, enterprise)
- [ ] **Keep the Azure dashboard container app?** `azure/main.bicep` now runs the dashboard's server
  (the sign-in BFF) as a container app and no longer instantiates the Static Web App, which would be
  a second, unconnected sign-in path. (11, enterprise)
- [ ] **Decide the fate of the internal `POST /v1/content/redeem`.** It still returns plaintext to an
  allowed service; the product path is the retrieval URL. Keep it as a service-to-service path or
  retire it. (10)
- [ ] **Go through `FOLLOWUPS.md`.** Seven rows say a remaining brief is now wrong, in 06, 11, 12
  and 13; edit the brief or strike the row. One deferred item blocks task 12: nothing writes the
  signed policy bundle, and no task builds it. (review)
- [ ] **Remove the simulated devices from the owner's tenant, and restore the device task 02
  backdated?** Eight of the ten enrolled devices there are simulated. Simulated data now goes to
  the sample tenant, but what is already there stays until it is deleted. (01, 02, 04)
- [ ] **Decide the disposition of the four baseline failures** that `backlog/BASELINE.md` records:
  the two `endpoint/classifier-host` tests that need a Windows job object; the five
  `ingestion/ingest-api/internal/store` live tests that reuse the owner's tenant id and so fail
  against the lab database; the `seams` gate finding at `endpoint/protocol/content.go:63`; and the
  `db` gate, which reports FAIL instead of SKIP when `powershell` is absent. For each, say whether
  the repository is fixed or the failure stays in the baseline as known. (00)
