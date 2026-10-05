# Waiting on the owner

Everything the backlog has left for the owner to run, check or answer, in one list.

Agents add lines and never tick or remove them. The owner ticks a line when it is done. An agent
whose task relies on an unticked line does not assume it happened: check, and if it has not, say so.

Each line names the task that left it. The exact commands are in that task's `REPORT.md`.

## Run

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
- [ ] **Point the directory sync at a real Microsoft Entra ID tenant.** The harness has no Entra
  tenant, so the Graph provider is the one path not exercised here. Register an application, grant
  it `User.Read.All` (application permission), and run
  `control-api sync-directory --store sql --dsn "$SAC_PG_DSN" --provider entra --entra-tenant <tenant>
  --entra-client-id <id> --entra-client-secret <secret>
  --entra-user-ref-attribute onPremisesSamAccountName --directory-key "$SAC_DIRECTORY_KEY"`.
  The control-api binary carries the subcommand after `go build -tags sac_sql_driver`, and the
  auth-lab image after `node localdev/build.mjs --auth`. (06)
- [ ] **Route the minted retrieval URL to the vault in any deployment**, and set
  `SAC_RETRIEVAL_URL_BASE`: the vault mints `GET /v1/content/retrieval/{tenant}/{grant}`, the browser
  fetches it, and the analyst ingress (or the web tier) must forward that path to `content-vault`. The
  lab's dashboard forwarder does; `azure/main.bicep` does not yet. (10)
- [ ] **Assign `Storage Blob Data Reader` on the ciphertext account to the vault's user-assigned
  identity.** `azure/main.bicep` passes `SAC_BLOB_IDENTITY=managed`, and nothing grants the identity
  the read, so a deployment's first retrieval would be refused. (10)

## Verify

On the owner's dashboard, `http://127.0.0.1:8787`, with the real device.

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
