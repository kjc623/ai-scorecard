# Task 11 — Sign-in and roles: report

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
