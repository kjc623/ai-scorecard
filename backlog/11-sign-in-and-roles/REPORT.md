# Task 11 — Sign-in and roles: report

> Sections 1–7 are the first pass. §8, the enterprise onboarding and deployment pass the owner asked
> for on 2026-10-05, replaces its sign-in design (the dashboard no longer talks to an IdP itself;
> query-api and the vault no longer read `SAC_OIDC_*`) and adds a schema migration.

Branch `backlog/11-sign-in-and-roles`, cut from `backlog/10-content-retrieval-path` at
`b9286e3`. No schema change, so no migration. The lab was rebuilt and restarted
(`node localdev/build.mjs --auth`, then `docker compose -f localdev/authlab.compose.yaml up -d
oidc query-api dashboard dashboard-sample content-vault`); the lab CA volume was untouched.

## 1. Verdict

| # | Clause | Verdict | Tenant | Evidence |
|---|---|---|---|---|
| A1 | An unauthenticated request to the dashboard or to `/v1/query` is refused. | met | sample | Browser, no `--sign-in`: the address ends at the issuer's account chooser, not a page (`observe/2026-10-05T17-11-03-224Z-index-html-transport-live.txt`). `curl`: `POST /v1/query` → 401, `GET /session` → 401, `GET /explore.html` → 302 `/login`. The API refusal is 401; a browser page request is redirected to sign-in, which is the browser form of the same refusal. |
| A2 | Signed in as a viewer, Search cannot be opened and content cannot be read. | met | sample | `observe 'explore.html?transport=live' --sign-in viewer.sample@lab.test` → 403 `{"result_state":"unauthorised_role","code":"role"}` (`…17-09-37-450Z-explore-html-transport-live.txt`). The dashboard nav as viewer offers Overview, Tools/classes, Teams and Devices and no Search (`…17-09-32-191Z-index-html-transport-live.txt`). `curl` as viewer: `/v1/content-search` → 403, `/v1/content/retrieval` → 403 `role`. |
| A3 | Signed in as a content reader, Search opens and content can be read. | met | owner | `observe 'explore.html?transport=live' --sign-in reader@lab.test`, with the prompt box filled and submitted: the page issued `POST /v1/content-search` and rendered hits (`…17-09-51-776Z-explore-html-transport-live.txt`). A retrieval as the reader minted a single-use URL and the unauthenticated redemption returned the stored prompt; as `viewer@lab.test` the same call is 403 `role`. |
| A4 | The audit trail shows each user's own identity. | met | owner | `observe '#audit' --sign-in admin@lab.test` shows rows whose actor is `analyst@lab.test`, `reader@lab.test` (`…17-10-16-998Z-index-html-transport-live-audit.txt`). `ops.audit` grouped by actor confirms `reader@lab.test content_search/content_retrieval_*` and `analyst@lab.test query.events`. |
| A5 | Tests cover each role boundary on the server. | met | — | `query/query-api`: `test/auth.test.mjs` (9, RS256/JWKS/claims) and 6 role-boundary tests in `test/http-server.test.mjs`. `vault/content-vault`: `TestContentRoutesRequireAContentRole` and `internal/auth` (2). `query/dashboard`: `test/session-tools.test.mjs` (9). All pass. |
| O1 | Sign-in against a real Microsoft Entra ID tenant. | owner | — | The owner's clause. First do the Run line in §2; the report below names the configuration. |

## 2. For the owner to do

- `OWNER-TODO.md` Run: **Point the dashboard's session at a real Microsoft Entra ID tenant** — register
  an app, define the four app roles, carry the shadow tenant uuid in a claim (`sac_tenant` by default), and
  set `SAC_OIDC_ISSUER`/`SAC_OIDC_AUDIENCE` on `query-api`, `SAC_OIDC_ISSUER`/`SAC_OIDC_CLIENT_ID`/
  `SAC_OIDC_CLIENT_SECRET` on each dashboard, redirect URI `<origin>/callback`.
- `OWNER-TODO.md` Verify: the dashboard now asks the owner to sign in; sign in as an analyst and as an
  admin and confirm the Audit trail names each account and the Search nav follows the role.

## 3. Decisions

- **The role set** is the owner's reconciliation of the brief's proposal with `docs/06`: `viewer`,
  `analyst`, `content_reader`, `admin`. `approver` and `investigator` are retired (they existed for the
  removed second-approver step); `auditor` and `privacy_officer` fold into `admin`. `viewer` sees the
  Devices page including the person (the brief's "no per-person data" sentence is set aside for that one
  screen); `analyst` keeps prompt-text search and `content_reader` additionally opens stored content. To
  change any of this later: `query/query-api/src/roles.js`, `query/dashboard/tools/session.mjs`, and the
  two token builders are the four places; the token role names are the durable part.
- **Tenant and role come from the signed token.** The lab's access token carries `roles` and `sac_tenant`;
  `query-api` reads them from the verified claims (claim names configurable with `SAC_OIDC_TENANT_CLAIM`
  and `SAC_OIDC_ROLES_CLAIM`). No schema change; a real Entra tenant provides the tenant through a
  claims-mapping policy or optional claim. Changing this means changing how the token maps to a tenant.
- **The session is server-side and zero-dependency.** `tools/serve.mjs` runs authorization code + PKCE
  against the issuer and stores the tokens in an in-memory map keyed by an opaque HttpOnly cookie; the
  browser never holds a token. No third-party dependency was added. A restart drops sessions.
- **The development header stays**, only behind `SAC_DEV_TRUST_PRINCIPAL=1`, for the memory lab; the auth
  lab no longer sets it, and a token can never produce the lab-only `dev` role.
- **The vault re-checks the role** on `POST /v1/content/retrieval` (`content_reader`) and
  `POST /v1/content-search` (`analyst` or `content_reader`), via `X-Sac-Roles` set by `query-api` from the
  verified token. The minted `GET /v1/content/retrieval/{tenant}/{grant}` stays unauthenticated: the grant
  is the capability (task 10).

## 4. Not finished, not verified

- Sign-in against a real Entra ID tenant was not exercised; the lab runs a stand-in.
- A signed-in role can still open a page it cannot use by typing its address, and the Overview reads its
  findings tile for every role, so a `viewer` sees a role-refusal block rather than no tile. The read is
  refused server-side. Recorded in `FOLLOWUPS.md`.
- The retrieval request still accepts and records `case_reference`/`second_approver`/`justification`;
  nothing gates on them, per the product's removal of the approval step.
- `tools/serve.mjs`'s routing is exercised by the browser observations, not by a unit test; its reusable
  logic (`tools/session.mjs`) is tested.
- The default memory lab (`localdev/docker-compose.yml`) still runs the development principal.

## 5. Downstream impact

Read the remaining briefs (12, 13). No sentence in either is contradicted by this work: 12 expects the
`admin` role and 13 gives list export to `analyst` and subject export to an admin, all of which exist.
Added to `FOLLOWUPS.md` under Deferred work, two rows: the per-screen role tailoring above, and the lab
OIDC stand-in being a fixture (fresh signing key per start, `login_hint` chooses any account, no
passwords).

## 6. What was built

Used: task 09 (the search the vault composes), task 10 (the retrieval URL and its redemption), task 04/06
(the device account and directory names the pages show), task 03 (the finding-review write), task 05
(the tool catalogue). No endpoint/agent change.

- `query/query-api`: `src/http/auth.js` (RS256 + JWKS verification), `src/roles.js` (the closed role set
  and a source/endpoint → capability map), `src/http/server.js` (principal from the token; capability
  gates on `/v1/query`, both writes and both content reads), `src/http/content.js` (forwards
  `X-Sac-Roles`), `src/http/config.js`, `src/http/main.js`, `src/plan.js` (a plan names its `source_id`).
- `vault/content-vault`: `internal/auth` (a `Roles` field and `HasAnyRole`), `internal/httpapi/server.go`
  (role gate on retrieval-mint and search).
- `query/dashboard`: `tools/session.mjs` (OIDC client, PKCE, session store, role→pages), `tools/serve.mjs`
  (the sign-in routes, the auth gate, bearer forwarding, the retrieval exception, the Search page gate),
  `src/session.js` + `src/transport.js` `loadSession` + `src/app.js`/`src/explore-app.js` (nav hiding),
  `tools/observe.mjs` (`--sign-in`), `tools/build-index.mjs`, generated `index.html`/`explore.html`,
  `Dockerfile`, `README.md`.
- `localdev`: `oidc/` (the stand-in, its image and tests), `authlab.compose.yaml` (the service and the
  session env), `build.mjs`, `tools/check-config-agreement.mjs`.
- `docs/04` §2.2 (§8.1, §4 table, §15.6, A16), `docs/06` §4.1 and A7 and the B13 as-built note;
  `query/query-api/README.md`; `backlog/ENVIRONMENT.md`.

## 7. Evidence

- Suites: `query/query-api` 253 tests, 245 pass, 8 skip, 0 fail; `query/dashboard` 164 tests, 158 pass,
  6 skip, 0 fail; `vault/content-vault` all packages pass; `localdev/oidc` 4 pass. The added tests are
  named in A5.
- `node tools/accept.mjs`: the same three failing gates as `BASELINE.md` (`packages`, `seams`, `db`) and
  the same skip (`browser`); no gate that passed now fails.
- `node localdev/tools/check-config-agreement.mjs` is now green. It had been red before this task on
  three names other tasks left unaccounted (`SAC_CONTENT_SEARCH_SCOPE`, `SAC_VAULT_URL`,
  `SAC_UPLOAD_SIGNING_KEY`); those are now named as gaps, and the five new `SAC_OIDC_*` names are listed.
- No schema change; nothing to apply to another database.
- Edits to `backlog/ENVIRONMENT.md`: added the `--sign-in` requirement and the lab identity provider.
  `localdev/tools/check-config-agreement.mjs` gained the OIDC vocabulary; `query/dashboard/README.md`,
  `query/query-api/README.md`, `docs/04`, `docs/06` updated as in §6.
- Browser artefacts under `.integration/observe/` named in §1 (not committed).

## 8. Enterprise onboarding and deployment (second pass)

Asked by the owner in session, not by a brief: a true enterprise enrolment and installation path for
customers on Microsoft Entra ID and Intune, with no MSI flags, that also works for customers who do
not use Entra. Owner decisions: vendor creates the tenant and the customer's admin links it; any OIDC
provider (found by email domain) plus Entra; users by SCIM only (no `User.Read.All`); people keyed by
UPN and Entra object id; devices by a per-tenant deployment key plus an Intune check where the tenant
asks for it; a tenant-specific package (`.intunewin` / `.zip`) around one generic MSI; MSI unsigned
for now with the signing step built.

### 8.1 Verdict (device-auth lab, sample tenant `5a3c0de0-…`; files under `.integration/observe/`)

| Clause | Verdict | Evidence |
|---|---|---|
| An unauthenticated page is sent to sign-in; the sign-in page offers Microsoft and work email | met | `2026-10-05T18-43-41-672Z-index-html-transport-live.txt` and the `/signin` observation at 18-43-31 (0 console errors, 0 other-host requests) |
| Sign-in through a customer OIDC provider; tenant decided by our mapping; role from the IdP | met | viewer, analyst, content reader and admin each signed in via the stand-in (issuer `http://oidc:8080` → sample tenant): `18-45-22-322Z`, `18-45-36-727Z`, `18-45-39-645Z`, `18-45-41-903Z` |
| Roles enforced: a viewer has no Search, Audit or Deployment and is refused them by address; a content reader opens an event | met | `18-45-22-322Z` (3 absences held), `18-45-32-175Z`, `18-45-34-349Z` (403 refusal page), `18-45-39-645Z` |
| A cross-site state change is refused; an unknown email domain is refused plainly; sign-out ends the session | met | `curl` POST `/v1/query` with `Origin: https://evil.test` and a valid session → 403; `18-46-08-839Z` (`no_sso_connection`); `18-46-06-636Z` (`/signin?notice=signed_out`) |
| Settings → Deployment: connection, verification, packages, keys, a SCIM token shown once | met | `18-45-41-903Z`, `18-46-00-501Z` (download, minted key named), `18-46-03-875Z` ("will not be shown again") |
| A tenant package holds the generic MSI and only the four tenant keys | met | downloaded `.zip`: `ShadowAICapture.tenant.env` holds `SAC_TENANT_ID`, `SAC_DEVICE_ENDPOINT`, `SAC_DEPLOYMENT_KEY`, `SAC_AUTH_MODE` only; the MSI's sha256 equals `release.json` (`33e7f9e1…`); `.intunewin` 200, 5,190,658 bytes |
| A device enrols with the package's deployment key and fetches signed policy | met by substitute | the real Linux `capture-core` in a container on the lab network, configured from the downloaded package's tenant file plus lab overrides (edge address, dev CA, trust/proxy off), stood in for a Windows device: enrolled `05e56e01-…` (dpop), `policy: new bundle in force version=1791226080`, cache written. Server: the key's `enrolment_count` is 1, an `ops.device` row, `device.enrol` and `policy_bundle.publish` audit rows. A Windows device through Intune is the owner's check |
| SCIM from an Entra-shaped client: create, rename, deactivate | met | through the edge with a `sacscim_` token: 401 without it; create, `op: "Replace"` rename and `"active": "False"` → success; DB: one canonical ref, 3 aliases (old UPN, new UPN, object id), department Research, `inactive`; audit `scim.user.*` as `scim:<token>` |
| Entra admin consent, Entra sign-in, the Intune check, Intune accepting the `.intunewin`, a Windows install from a package | owner | needs a real Entra/Intune tenant and a clean VM: the `OWNER-TODO.md` lines tagged "(11, enterprise)" |
| The owner's tenant is not changed | met | still 1373 submissions and 0 identity connections after the migration and every check |

### 8.2 For the owner

The lines tagged "(11, enterprise)" in `OWNER-TODO.md`: register the vendor Entra app; onboard your
own Entra tenant as the test customer (`control-api tenant create`, then `tenant invite`); set up
SCIM (a separate non-gallery Entra app until a gallery listing); apply
`backlog/11-sign-in-and-roles/MIGRATION.sql` to any other database; the Key Vault secrets;
publisher verification; rebuild the lab MSI on the new path; the Intune, device, clean-VM and SCIM
checks; and two questions (how you sign in to port 8787 now, and whether to keep the Azure
dashboard container app).

### 8.3 Decisions

- **control-api is the one identity service.** It is the relying party for every customer IdP and
  mints ES256 product tokens (≤ 10 min); query-api, the vault and control-api's admin API verify only
  those. Forwarding customer access tokens cannot work for Google or Okta, so the first pass's design
  was replaced. Sessions live in `ops.auth_session`, so any dashboard instance serves any session.
- **The product tenant comes from our mapping** (Entra `tid`, OIDC `iss`, a vendor-set email
  domain), never from a claim; an Entra token's issuer must be its own tid's.
- **`user_ref`** is `protocol.DeriveUserRef` (an HMAC under a per-tenant key issued at enrolment). The
  device prefers the console user's UPN; SCIM fixes the canonical ref at creation and aliases the
  rest; `ingest.record_event` stores the canonical ref. This replaces task 06's
  `onPremisesSamAccountName` mapping.
- **The audit chain's read was granted** (`SELECT (tenant_id, audit_seq, row_hash)` to the roles that
  insert audit rows). Every service's audit insert failed under its real role before; the lab hid it
  by connecting as a superuser. Three agents found it independently.
- **query-api has one dependency, `jose`** (pinned, itself dependency-free), for token verification.
  The vault and control-api verify with the Go standard library.
- **Lab:** the stand-in IdP serves the sample tenant only (one issuer maps to one tenant), so port 8787
  has no sign-in until the owner decides (`OWNER-TODO.md`, Answer).

### 8.4 Not finished, not verified

Not run anywhere: real Entra consent and sign-in, the Intune Graph check against Intune, Intune
accepting the `.intunewin`, a Windows install from a package, attestation on an Intune-enrolled or
hybrid-joined device, and an Azure deployment of the Bicep (it compiles). Known gaps are the
`FOLLOWUPS.md` rows tagged "11 (enterprise)", including: an admin-only user's Overview shows two
refusal panels (role design); the deployment-key rate limit is per process; the Intune check is an
inventory match, not proof of possession; the internal token is a shared secret.

### 8.5 Downstream impact

`FOLLOWUPS.md`: four brief rows (12 twice: the bundle writer now exists and Settings → Deployment
exists; 13: the SCIM, alias and session tables in subject export and erasure; 06: SCIM replaces the
Graph pull) and eleven deferred rows.

### 8.6 What was built

On top of the first pass (`4d2e7f2`), same branch. `endpoint/protocol`: deployment key, attestation,
`user_ref_key`, `DeriveUserRef`, the policy response. `control-api`: `session`, `identity`, `onboard`,
`entraapp`, `scim`, `deploy`, `intune`, `policyserve`; enrolment by deployment key; `tenant create` and
`tenant invite`; wiring in `cmd/control-api/enterprise.go`; the Entra Graph user pull removed.
`query-api` and `content-vault`: product-token verification. `query/dashboard`: a BFF over control-api,
sign-in pages, Settings → Deployment, role-shaped navigation, sessions in `observe.mjs`.
`endpoint/capture-core`: layered config, key enrolment with attestation, console-user `user_ref`,
policy fetch and cache, a per-device CA, hardware-seeded identity. `installer`: the generic flagless
MSI, `release-msi.mjs`, the signing step (off), the lab MSI on the same path. `database`: the contract
tables, `sac_resolver` lookups, alias resolution in `record_event`, two grants. `localdev`: identity
material and seed, edge routes, compose. `azure`: routes, the federated-credential output, Key Vault
secrets, a dashboard container app, the blob reader; the composition now compiles.

### 8.7 Evidence

- Schema from empty, and `MIGRATION.sql` twice on HEAD's schema: exit 0; invariants 68/68 on both.
  As `sac_control` and `sac_ingest`: audit inserts chain per tenant, the catalogue is readable, audit
  contents stay unreadable. Applied to the lab with `docker exec -i sac-authlab-postgres-1 psql -U
  postgres -d shadow -v ON_ERROR_STOP=1 < backlog/11-sign-in-and-roles/MIGRATION.sql`, then `node
  localdev/build.mjs --auth`, the dashboard image, `node installer/release-msi.mjs` and `docker compose
  -f localdev/authlab.compose.yaml up -d`. The CA volume was untouched.
- Suites: control-api 167 top-level PASS, 0 FAIL (baseline 32); query-api 276 tests, 268 pass, 8 skip,
  0 fail with the lab up (baseline 214/199/15/0); dashboard 219/213/6/0 (baseline 145/139/6/0);
  content-vault 80 (48); capture-core 299/0 on Linux (the endpoint agent's run) and 298/5 on this
  Windows host, where the 5 predate this work (`trust` confirmed failing at HEAD in a scratch worktree).
- `node tools/accept.mjs` on Windows: contract, vocab, invariants, endpoint, browser and db pass;
  `seams` is the baseline finding; `installer` passes in full on Linux (`node:22-alpine`) and its MSI
  checks pass on Windows; `packages` fails only on the pre-existing Windows `trust` test.
- `node localdev/tools/check-config-agreement.mjs`: ok. Lab compose additions in this pass:
  `SAC_AUTH_ALLOW_INSECURE_IDP=1` and `SAC_AUTH_REDIRECT_URIS` on control-api; without the first,
  control-api's outbound guard refuses the stand-in IdP's private address and sign-in fails.
