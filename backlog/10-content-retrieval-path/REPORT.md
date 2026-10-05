# Task 10 — content retrieval path

## 1. Verdict

| Done-when clause | Verdict | Tenant | Evidence |
|---|---|---|---|
| "Opening an event in Search, observed in the browser (`node query/dashboard/tools/observe.mjs 'explore.html?transport=live#events' --click 'tr.x-row'`), shows the prompt in the Event drawer as it does today." | met | owner's | `.integration/observe/2026-10-05T16-18-42-334Z-…open-1c878461….txt`: an event opened by submission id, `--expect 'Read-only code review'` held, and the PROMPT section shows the typed prompt. The clause's own `--click tr.x-row` run (`…16-17-54-020Z-…events.txt`) shows the captured content of the first row ("Everything captured for this request (194 KB)"). 0 console errors, 0 failed requests in both. |
| "The browser fetched that content from the vault's retrieval URL, and `query-api`'s response carried no content." | met | owner's | The `data requests:` header of both observe runs lists `POST /v1/content/retrieval` then `GET /v1/content/retrieval/11111111-…/<grant>`; `other-host requests: 0`. A direct `POST http://query-api:8080/v1/content/retrieval` answered `{state, event_id, grant_id, raw_digest, expires_at, retrieval_url}` — no `content`. `content-forward.test.mjs` asserts the same. |
| "The lab still works offline, with a storage stand-in that refuses a read without a credential." | met | owner's | `GET contentlab:8080/blob/<tenant>/<object>` answered 401 with no `Authorization`, 401 with a wrong one, 200 with `Bearer sac-lab-only-blob-read-credential`. The vault logged `reindexed … indexed=44 cleared=128 failed=0`, i.e. its own reads through that credential succeeded. |
| "Tests cover expiry, single use and a replayed URL." | met | — | Go: `TestRetrievalURLIsMintedAndRedeemedOnce` (single use), `TestRedeemURLExpiredAndUnknown` (expiry), `TestRetrievalURLOverHTTP` (replayed URL → 403). Live: first redeem 200 / 199156 bytes, replay 403 `grant_already_used`. |
| **Owner:** "Nothing on the device. The vault reading real blob storage with its own identity needs a deployment the harness does not have; say in the report what was and was not exercised against `azure/`." | owner | — | See §4. Built and unit-tested: `SAC_BLOB_IDENTITY=managed` fetches an IMDS token and presents it; `azure/main.bicep` now passes `managed`. Not exercised: any request to a storage account, and no role assignment grants the identity a read. |

## 2. For the owner to do

Added to `OWNER-TODO.md` (§Run): route `GET /v1/content/retrieval/{tenant}/{grant}` to `content-vault`
at the analyst ingress and set `SAC_RETRIEVAL_URL_BASE`; assign `Storage Blob Data Reader` on the
ciphertext account to the vault's identity. (§Verify) open a newly captured prompt and confirm the
drawer fetches it from the vault. (§Answer) decide the fate of the internal `POST /v1/content/redeem`.

Exact commands are the plain ingress/role configuration; there is nothing to run on the device.

## 3. Decisions

- **The retrieval URL carries the tenant and grant and nothing else** — no token column, so no schema
  change. It is single-use (the existing atomic grant claim) and short-lived (the grant TTL), which is
  the capability §11 describes. Changing to an opaque token later is an additive migration.
- **Its redemption is unauthenticated by design.** The grant is the credential; minting still requires
  the session principal. This is recorded for task 11 in `FOLLOWUPS.md`.
- **The lab's analyst web tier forwards the URL straight to `content-vault`** (`serve.mjs`,
  `SAC_CONTENT_VAULT_URL`), never through `query-api`; a deployment's ingress takes that part. The
  vault's own ingress stays internal.
- **Storage credential seam**: new `internal/blob` with `ManagedIdentity` (IMDS, no cloud SDK) and the
  lab's `StaticBearer`; an endpoint with no credential is now a startup refusal, not an anonymous GET.
- **Kept `POST /v1/content/redeem`** (principal- and event-bound) for the grant-matrix tests and any
  service caller; the product path is the URL. Flagged for the owner and in `FOLLOWUPS.md`.
- **No schema change**, so no migration; the lab's database was untouched apart from the reads above.

## 4. Not finished, not verified

- No request was made to Azure Blob and no managed identity was exercised: this host has neither. The
  IMDS token fetch/refresh and the `Authorization: Bearer` the reader sends are unit-tested against a
  fake IMDS (`internal/blob`).
- The deployment cannot redeem a minted URL yet: the analyst ingress does not route that path, and the
  vault's identity has no `Storage Blob Data Reader` assignment. Both are owner lines.
- Expiry is covered by a moving-clock test, not live (the TTL is five minutes).
- The sample tenant has no stored content, so the browser clause was observed against the owner's
  stored prompts (reads only; audit rows written as expected).

## 5. Downstream impact

`FOLLOWUPS.md`: one brief-made-wrong row for **11** (the role gate must sit on minting, and the URL's
redemption is the deliberate unauthenticated exception); three deferred rows (ingress routing,
`Storage Blob Data Reader`, the internal `redeem` route). Read briefs 11–13; 12 and 13 are unaffected.

## 6. What was built

Branch `backlog/10-content-retrieval-path`, cut from `backlog/09-prompt-search-filters` (09's filtered
search and 08's `prompt_kind` are used unchanged; the content path itself is tasks 00–07's).

- **`vault/content-vault/internal/blob`** (new): a credential seam (`StaticBearer`, `ManagedIdentity`)
  and `HTTPReader`, which presents the credential and treats a refusal or absence as an error.
- **`vault/content-vault`**: `Retrieve` mints `RetrievalURL`; `RedeemURL` + `GET
  /v1/content/retrieval/{tenant}/{grant}` serve it; `Options.Blobs`/`RetrievalURLBase`; the binary
  builds the reader from `SAC_BLOB_IDENTITY`/`SAC_BLOB_READ_CREDENTIAL`/`SAC_BLOB_CIPHERTEXT_ENDPOINT`.
- **`query/query-api`**: `content.js` relays `retrieval_url` and no content; the redeem call is gone.
- **`query/dashboard`**: the page fetches the URL via `content.readUrl`; new stub shape; `serve.mjs`
  forwards the URL to the vault; `index.html`/`explore.html` regenerated.
- **`localdev/contentlab`** refuses an uncredentialed read; **`localdev/authlab.compose.yaml`** passes
  the credential and the vault URL; **`azure/main.bicep`** passes `SAC_BLOB_IDENTITY=managed`.
- **Docs**: `00`, `02` §11, `03` §8, `04` §8.1, `06` §5.2 and the vault/query/localdev READMEs.

## 7. Evidence

- Tests, current tree: vault `go test ./...` all packages pass (new: 5 blob, 4 vault, 2 httpapi tests);
  `query/query-api` `node --test` 238 tests / 230 pass / 8 skip / 0 fail; `query/dashboard` 155 / 149 /
  6 / 0. `tools/verify-all.mjs`: 18 packages, 2 fail (`endpoint/classifier-host`,
  `ingestion/ingest-api` live) — the baseline failures only. `node tools/accept.mjs`: the baseline
  gates (packages, seams, db) and no new one.
- `node localdev/tools/check-config-agreement.mjs`: content-vault clean (one gap note for
  `SAC_RETRIEVAL_URL_BASE`); the three pre-existing control-api/query-api failures remain.
- Lab: `node localdev/build.mjs --auth` then `docker compose … up -d content-vault contentlab dashboard
  dashboard-sample`; all healthy. The schema service re-ran idempotently and the data was intact.
- Edited headers so the next agent finds them: `observe.mjs` (the `data requests` header and why),
  `serve.mjs` (the vault passthrough and why), `contentlab/main.go` (the refused uncredentialed read).
  No edit to `AGENTS.md` or `ENVIRONMENT.md`.
