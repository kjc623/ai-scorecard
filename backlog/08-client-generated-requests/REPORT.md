# 08. Client-generated requests and classification scope

## Verdict

| Clause ("Done when") | Verdict | Tenant | Evidence |
|---|---|---|---|
| A captured Claude Code titling request is marked client-generated, run through the endpoint's own code path. | met | — | `endpoint/capture-core/core/promptkind_test.go` `TestPipelineMarksATitlingRequestClientGeneratedAndDoesNotClassify`: the titling body yields `prompt_kind=client_generated`, an empty label set and **no classifier call**. |
| The capital-of-Australia request is a user prompt and gets no `source_code` label. | met | — | `TestPipelineClassifiesTheAuthoredTextNotTheWholeBody`: the classifier receives exactly the authored text, not the body that carried the tool schema; `prompt_kind=user`. |
| A prompt that really contains source code or a card number is still labelled. | met | — | `TestPipelineStillLabelsAuthoredSourceCode` (authored `source_code`); the classifier-input rule is `classifyInput` and the kind table's card-number case in `TestDecidePromptKind`. |
| A client-generated event sent through the device path into the sample tenant is stored with its kind. | met by substitute | sample | `node localdev/tools/simulate-devices.mjs --edge https://edge:8443 --devices 1 --events 8 --client-generated 3` → 12 accepted; `psql` on the sample tenant: `client_generated 3`, `user 6`. **The simulator fabricates `prompt_kind`**; the endpoint decides it (tests above), but only an installed build can decide it on the lab. |
| That event can be filtered in `query-api`. | met | sample | `POST /v1/query` q8: `prompt_kind=client_generated` returns the `u_clientgen` rows; `prompt_kind_not=client_generated` returns `user` rows and keeps NULL rows (via `coalesce(s.prompt_kind,'unknown')`). |
| The vault does not index a client-generated request. | met by substitute | sample | Direct `POST /v1/content/object/finalise` (service `control-api`) with `prompt_kind=client_generated` and a `prompt_body` unit → `{"indexed":0}`, no `ingest.search_text` row; the same call with `prompt_kind=user` → `{"indexed":1}`, row `quota`. The device simulator cannot upload content, and the live derived-index path is covered by the vault's own tests. |
| Hidden in Search by default with a way to include it, observed in the browser. | met | sample | `.integration/observe/2026-10-05T14-35-05-025Z-explore-html-transport-live-events.txt` (`--absent u_clientgen`, 1 check held); `2026-10-05T14-35-14-836Z-…` (`--click '[data-act=include]' --expect u_clientgen`, 2 checks held, hash `#events?include=1`). Console errors / failed requests / other-host requests: 0. |
| Tests cover the kind decision and the classification input. | met | — | `core/promptkind_test.go` (11-case kind table), `core/pipeline_test.go` cases above, `core/envelope_test.go` `TestEnvelope_PromptKindIsPromptOnlyAndM1Plus`, plus query-api/vault/control-api/dashboard suites. |
| The agent build for the owner's machine is produced, and the report says what the owner must run. | met by substitute | — | `node installer/build.mjs --os windows --arch amd64` cross-compiles both Windows binaries with this change and stages `installer/.stage/windows-amd64/`; the `installer` gate passes. The MSI itself is built on the Windows host: `lab-msi.mjs` refuses off Windows (it needs WiX), so the owner runs it (below). |

The owner verifies, after installing the new build:

| Clause | Verdict | What the owner must do first |
|---|---|---|
| Searching prompt text for "Australia" no longer returns the titling request. | owner | Reinstall for task 08 (below), then start a **new** Claude Code session. A titling request indexed **before** this fix stays searchable (its content object predates the kind); the fix stops new ones, and only retention removes the old row. |
| A new "What is the capital of Australia" prompt carries no `source_code` label. | owner | Same reinstall; the new device classifies the authored text, not the body. |
| A prompt containing a test card number is still labelled `payment_card`. | owner | Same reinstall. |

## For the owner to do

`OWNER-TODO.md`, added under Run and Verify:

- Apply 08's migration after 06's, on any database but the lab's (the lab already has it):
  `psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/08-client-generated-requests/MIGRATION.sql`.
- Rebuild and reinstall the lab MSI on the Windows host, with the lab up:
  `node localdev/build.mjs --auth`, then `node installer/lab-msi.mjs`,
  then `msiexec /i installer\dist\ShadowAICapture.msi`.
- After the reinstall, from a new session: the capital-of-Australia prompt has no `source_code`
  label, a card-number prompt still says `payment_card`, and the new session's titling request
  does not appear in prompt search (see the caveat in the verdict).

## Decisions

- **Field and values.** The envelope carries `prompt_kind` (`user` | `client_generated` | `unknown`);
  it is optional in the contract (an old device sends nothing, read as `unknown`) and permitted
  only for `kind: prompt` at M1 and above, refused at M0 (its closed list has no body to decide
  from) and for the rollup/detection kinds. Additive, so `schema_version` stays `1.0`, following
  `subject_name` (ADR 0021). Changing it later is a contract change plus a column migration.
- **Classify the authored text.** The pipeline hands the classifier C1's output when C1 identified
  an authored turn, and the whole body only when it could not (already `degraded`). A
  `client_generated` request is not classified at all: empty labels, `confidence: high`, the last
  known classifier version. This is the fix for the reported `source_code` false positive.
- **The detector is a seed, not a closed rule.** `core/promptkind.go` matches known client
  request phrases and the body shape; an unmatched prompt is `user`. Recorded in `FOLLOWUPS.md`.
- **`ne` is null-safe.** The query dimension compiles to `coalesce(s.prompt_kind,'unknown')`, so the
  dashboard's default-hide (`prompt_kind_not='client_generated'`) does not drop pre-decision rows.
- **The kind rides on the content object.** `ops.content_object.prompt_kind` was added so the SQL
  reindex honours the kind without granting `sac_vault` a read on `ingest.submission`. Without it
  the teammate's reindex gap would re-index a client-generated object. A later task may prefer the
  grant instead, but that widens the vault's reach.
- **The old titling index row is not cleared**, because its stored object predates the kind and
  re-deriving it in the vault would re-create the downstream patch this task removed. Recorded in
  `FOLLOWUPS.md`.

## Not finished, not verified

- The real device has not run the new build; every owner clause above waits on the reinstall. The
  lab event's `prompt_kind` was fabricated by the simulator, not decided by the endpoint.
- A client-generated prompt already in `ingest.search_text` stays searchable until retention
  removes it (the content object predates `prompt_kind`). The fix is forward-looking.
- The endpoint's marker list is a seed for Claude Code's known requests; a reworded client request
  falls back to `user` (shown), which is the safe direction.
- `lab-msi.mjs` / the MSI could not be run here (it refuses off Windows); only the Windows payload
  was staged.

## Downstream impact

Read briefs 09–13: none is made wrong. 09's prompt search gains a source that never contains
client-generated requests; no field it names changes. Two `FOLLOWUPS.md` rows were added under
Deferred work (the pre-existing index row; the detector is a seed).

## What was built

- Branch `backlog/08-client-generated-requests`, cut from `backlog/07-class-filter-bug` at `8d4acdc`.
- Used tasks 01–07's read path, aggregates, findings, identity, tool catalogue and directory work.
- **contracts/** — `prompt_kind` in the schema, the four kind/mode branches, the generator mapping,
  regenerated Go/TS.
- **database/** — `prompt_kind` on `ingest.observation`, `ingest.submission` and
  `ops.content_object`; the M0/rollup/detection constraints extended; `ingest.record_event` stores
  and carries it; `MIGRATION.sql`; the T39/T40/T42 forbid sweeps and the checker's fixture
  perturbations updated.
- **endpoint/** — `protocol.PromptKind`; `core/promptkind.go` (the decider); `core/envelope.go`
  (field policy); `core/pipeline.go` (decide once, classify the authored text, skip classification
  for client-generated); docs/01 §9.8.
- **control-api + vault** (teammate `vault-path`) — the kind threads from `ingest.observation`
  through the finalise call; the vault skips the caller and derived index paths and clears a
  client-generated object on reindex. The lead added the `ops.content_object` column and SQL store.
- **query-api** (teammate `query-api`) — the `prompt_kind` dimension and q8 `prompt_kind` /
  `prompt_kind_not` params, snapshot, `DSL.md`.
- **dashboard** (teammate `dashboard`) — the events list hides client-generated by default with an
  "Include client-generated requests" toggle, and offers `prompt_kind` as a filter; pages
  regenerated; docs/04.
- **harness** — `simulate-devices.mjs --client-generated N`; `check-seams.mjs` treats contract enum
  values as vocabulary.

## Evidence

- `node localdev/build.mjs --auth` rebuilt the lab; `ingest-api`, `content-vault`, `query-api`,
  `dashboard` recreated. Schema migration applied twice (idempotent). No `run.mjs`.
- `database/schema.sql` applied from empty in a throwaway `postgres:17-alpine` container with
  `psql -v ON_ERROR_STOP=1`: exit 0, and `ingest.observation`, `ingest.submission` and
  `ops.content_object` all carry `prompt_kind`.
- `node tools/accept.mjs`: gates 9, pass 5, fail 3 (`packages`, `seams`, `db`), skip 1 (`browser`) —
  identical to `BASELINE.md`; `packages` 18 / 16 pass / 2 fail, the same `endpoint/classifier-host`
  (2) and `ingestion/ingest-api` (5) baseline failures. `database/tools` 41/41 after the fixture
  edits. `seams` back to its one baseline finding. `contract` and `invariants` pass.
- `node --test` no-arg: `query/query-api` 234 tests / 226 pass / 8 skip / 0 fail (baseline 214/199/15,
  which predates 05–07); `query/dashboard` 151 / 145 / 6 / 0 (baseline 145/139/6).
- Go: `endpoint/protocol`, `endpoint/capture-core`, `control/control-api`, `vault/content-vault` all
  pass; the vault's `internal/store` sql-schema tests run against the live lab schema.
- `node database/tools/check-schema.mjs`: 74/74.
- Edits to shared files beyond the task: `tools/check-seams.mjs` (contract enum values are not
  near-miss field names), `localdev/tools/simulate-devices.mjs` (`--client-generated`),
  `database/tools/check-schema.test.mjs` (constraint-text fixtures), `database/invariants.test.sql`.
  No edit to `AGENTS.md` or `ENVIRONMENT.md`.
