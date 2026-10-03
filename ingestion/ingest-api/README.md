# ingest-api — the validating write path

The one component that writes events ([ADR 0001](../../docs/adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md)).
Collectors hold no database credential; a device's only capability is to call the device-facing endpoints
as itself ([docs/02-ingest-and-transport.md](../../docs/02-ingest-and-transport.md) §2.4). The normative
specification for everything this service computes is §4 of that document; the endpoint and the reason
codes are §5–§7. Go, standard library only in the default build.

## The endpoint, exactly

`POST /v1/events` takes `protocol.EventBatch` (gzip optional, §5.3) and answers with
`protocol.EventBatchResponse` — consumed from `endpoint/protocol`, never re-declared, and a test feeds
the response bytes into the device's own validator so the shape cannot drift from the client that reads
it. **A batch that parses always returns 200**, even when every event inside it is rejected: the
per-event outcome is the contract. Batch-level failures use §5's common envelope
`{"error":{"code","detail","server_time"[,"retry_after_s","message"]}}`:

| Status | When | `code` |
|---|---|---|
| 400 | batch shape, `event_count` mismatch, unsupported batch version, or an element that is not an object or has no `event_id` | `schema_violation`, `unsupported_schema_version` |
| 405 | any method but POST (the response carries `Allow: POST`) | `schema_violation` |
| 400 | fewer than 1 or more than 500 events (§5.3 lists the count under 400; §7 calls it `oversize`) | `oversize` |
| 413 | body over 8 MiB compressed / 32 MiB decompressed, or one envelope over 256 KiB | `oversize` |
| 401 | no, bad, expired or revoked credential | `revoked_device` |
| 403 | unknown or inactive tenant; region mismatch | `unknown_tenant`, `region_mismatch` |
| 409 | `batch_id` already accepted inside the replay window | `duplicate_batch` |
| 503 | write failure — nothing committed, retryable | `schema_violation` |

`server_time` is on every response so the device can measure clock offset (C26). Per-event rejections
carry a code from §7's **closed** set plus `detail{pointer, expected, supported?, presence_map}`, and
`expected` never echoes the offending value, because it can be content. `won_fields` means "the
submission's contest-winning route is this observation's route", not "this observation changed a
field" — see the comment on `store.EventOutcome`.

## What the device is allowed to see

A device is an untrusted reader, so a response may carry the closed reason code, the JSON Pointer, the
violated constraint's *shape*, the presence map, `server_time`, and a sentence written by hand for that
reader — and nothing derived from a driver, a socket or the schema loader. `ingest.Error` enforces the
split: **`Message` is device-facing, `Cause` is internal**, and `internal/httpapi` logs the cause and
sends only the message. One field serving both audiences is how a PostgreSQL error string reaches a
response body, which is what happened before this comment existed (`invalid input syntax for type uuid:
"sha256:aaa…"`, from the old `mapStoreError` default). `devicefacing_test.go` asserts the *class* rather
than the site — an unclassified store failure, an unclassified authentication failure and a malformed
development identity each produce a clean code and no internal text, while the detail is verified to have
reached the log — and reverting the fix makes those tests fail with the original string. The one
deliberate exception is a JSON *decode* failure, which returns `encoding/json`'s own message.

## The integration seam

One transaction per batch. The write is exactly `SELECT event_outcome, event_submission_id FROM
ingest.record_event($1::jsonb, $2::timestamptz)`: `$1` is the envelope **as the device sent it** (never
re-marshalled), `$2` is the batch's single receive time, and it returns `inserted` | `merged` |
`duplicate`. `internal/store/sql.go` holds every statement as a constant, and `db_integration_test.go`
(through the helpers in `psql_test.go`) PREPAREs and EXECUTEs those exact strings against a live PostgreSQL 17, so what is verified is the text,
not a paraphrase. The order is: set the transaction-local RLS tenant; re-check the credential **before
commit** (§2.3, so a revocation between admission and commit writes nothing); one `record_event()` call
per accepted event in request order; for a `duplicate` read back the first receipt, for a `merged` read
back the store's own winner; then quarantine the rejections in the same transaction (§6 step 5).
Validation happens entirely **in memory before the transaction opens**, because `record_event()` refuses
an M0 record carrying content by *raising*, which aborts the whole transaction — a device defect
validated late would be a batch that can never commit. Nothing here re-implements the ladder: idempotency
is the `(tenant_id, event_id)` key and merging is the stored procedure's, per §6's "enforced by the
store, not by a check in application code". `store.Memory` transliterates `record_event()`'s semantics
and is not trusted: `internal/ladder` holds the §4 conformance fixture and the same scenario table runs
against `Memory` *and* the live stored procedure, so a drift fails a test rather than becoming a
production surprise.

Two validators run over every envelope, deliberately: the generated types
(`contracts/generated/go/envelope`, T1) decode it — the compile-time field vocabulary, so a renamed or
retyped field breaks the build — and `contracts/event-envelope.schema.json` is walked for the JSON
Pointer and violated-constraint text §7's `detail` is made of, which the decoder cannot produce. A record
must pass both, and where they can disagree the schema walk wins, in exactly one place: draft 2020-12
treats `format` as an annotation, so the decoder asserts only that a uuid field is non-empty while the
walk asserts the uuid shape — and ingest must assert it, because `record_event()` casts those fields with
`::uuid` and a malformed value would abort the batch inside the database.
`TestTwoValidatorsAgreeAndTheSchemaWalkIsStricter` pins both verdicts.

## Reason codes

The wire vocabulary is `protocol.ReasonCode` (docs/02 §7), closed. `store.QuarantineReason` maps it onto
`ingest.rejected.reason_code`, which uses the same spellings. Two codes have **no honest quarantine
row** and are reported to the device without one: `tenant_mismatch`, because `.tenant_id` is NOT NULL and
RLS-scoped, so a body claiming another tenant has no honest tenant to file under and filing it under the
authenticated one would misattribute the defect; and `duplicate_batch`, which is batch-level by
construction (§5.3) and has no per-event envelope to quarantine. `TestQuarantineMappingIsTotal` asserts
every code in the closed set is either mapped or one of those two, and `database/tools/check-schema.mjs`
checks the other direction across the seam: every code this service emits is one the CHECK accepts.

## Configuration and containers

Where a setting has both a flag and an environment variable, **a flag wins** — precedence is
decided by asking the flag package which flags were passed, so `--region ""` is a request rather than an
absence. Not every setting has both: `SAC_BLOB_CIPHERTEXT_ENDPOINT` and `SAC_APPINSIGHTS` are
environment-only, the `SAC_TLS_*_PEM` variables carry PEM text while the `--tls-*` flags take file
paths, and `--dsn`, `--driver`, `--pg-port`, `--replay-window`, `--shutdown-grace`, `--verify-dedup-key`
and the `--dev-*` flags have no environment variable. The names are the deployment's names, and the agreement is asserted rather than assumed:
[`cmd/ingest-api/infra_agreement_test.go`](cmd/ingest-api/infra_agreement_test.go) fails if a name is
passed to something that never reads it or read without being accounted for, and
`node localdev/tools/check-config-agreement.mjs` adds both Dockerfiles and the lab compose file. Three
settings are deliberately inert today: the blob ciphertext endpoint is read, validated and reported
**unused** (the ingest path does no blob I/O); `SAC_APPINSIGHTS` is read, validated and never logged,
because this build exports no telemetry; and `--region` is passed by nobody, so §12 region pinning is
inert in Azure — a gap the agreement test prints on every run. `--routes-file` carries
`ref.route_fidelity` as data, never compiled in (§4.4). Flag names, environment names, TLS material,
container stages and why `/readyz` exists are in [cmd/ingest-api/README.md](cmd/ingest-api/README.md).

## Running the checks

The gates run with `GOPROXY=off` by choice and the default build carries no PostgreSQL driver, so the
live checks reach PostgreSQL through `docker exec psql`:
`go build ./... ; go vet ./... ; gofmt -l . ; go test ./... -count=1`, plus
`go test ./internal/store -run TestLive -v -count=1` for the ladder, the key mirror and the quarantine
against a live server. The `TestLive*` cases need a running PostgreSQL 17 with `database/schema.sql`
applied; they find it by probing running containers whose image is PostgreSQL (override with
`SHADOWPG_CONTAINER`), run every script inside a transaction that ends in `ROLLBACK`, and **skip** when
none is found, so `go test ./...` is green on a machine without Docker rather than red for the wrong
reason. The whole-module gate is `node tools/verify-all.mjs --only go`; the local run, the dev flags and
the lab are in [cmd/ingest-api/README.md](cmd/ingest-api/README.md). The raw evidence logs under
`evidence/` are git-ignored (`*.log`) and a fresh clone will not carry them.

## Test map

The tests that hold the claims above are: the §4 ladder — same event two routes, lower-fidelity-last,
Tier-T-then-Tier-S **not** merged, bucket boundary, idempotent replay, rollup supersede, detection
collapse — from one table in `internal/ladder` against both `store.Memory` and the live stored procedure
(`TestLadderAgainstTheStore`, `TestLiveRecordEventRunsTheLadder`); §5.3's endpoint promise
(`TestEndpointShapeIsTheProtocolShape`, `TestBatchThatParsesAlwaysReturns200`); the reason vocabulary
(`TestReasonCodeVocabulary`, `TestQuarantineMappingIsTotal`); §2.3
(`TestRevokedCredentialInsideTheTransaction`, `TestMTLSAuthenticatorOverARealHandshake`); the write
boundary (`TestWriteBatchIsOneTransaction`); and the mirror plus adoption
(`TestLiveWeakDedupKeyMatchesTheGoMirror`, `TestAdoptionTakesTheKeyAndTheDigestTogether`).

## Not verified

1. **The `database/sql` plumbing is verified only under the build tag.** Under `-tags sac_sql_driver` it
   is exercised against a real PostgreSQL — pooling, the transaction boundary, `record_event`, driver
   error mapping, the §2.3 revocation re-check — and the tagged binary has run against the lab's database
   with events landing in `ingest.observation`. Unproven: **concurrency** (the memory store is
   mutex-serialised; the database path relies on unique constraints and row locks) and any server but the
   lab's. `pgx` v5.11.0 is required by `go.mod`, but the gates run with `GOPROXY=off` and their own
   `GOMODCACHE` (`.tools/gopath/pkg/mod`) — hence the tag rather than a dependency the default build
   needs.
2. **`duplicate_batch` is a per-process window**, so with more than one instance a cross-instance replay
   degrades to per-event `duplicate` outcomes — correct for the data, wrong only for the diagnosis (§6).
   Relatedly, **mTLS is tested against a generated CA**, not real CA material or the edge, and the region
   check is a string comparison against `ops.tenant.residency_region`.
3. **§4.2 canonicalisation has no NFC in this service**: `dedup.DefaultCanonical` uses the identity
   normaliser, because the standard library has none and this module imports neither
   `golang.org/x/text/unicode/norm` nor the repository's own NFC implementation, `endpoint/canon/`. It
   is off the request path — the wire carries the finished `content_digest`.
4. **Interpretations made where the documents leave room**, each also in a code comment: an element of
   `events` that is not a JSON object, or has no usable `event_id`, is a batch-level 400 (a per-event
   result is keyed by `event_id`); the count outside 1–500 is 400 with code `oversize` while body/envelope
   caps are 413; the wire `presence_map` carries present names only, while the quarantine row carries
   `{"present":[…],"missing":[…]}`; the quarantine drops `content_digest` too, because the schema's
   constraint is stricter than §7's wording; and **§7 has no `device_mismatch` code**, so a disagreeing
   body `device_id` is reported as `schema_violation` against the authenticated value rather than under a
   code that would misname it.
5. **A latent spec-vs-store divergence, for whoever owns §4 conformance:** §4.5's "tier dominates rank"
   is implemented and tested in `dedup.Beats`, but the deployed tie-break compares raw
   `ref.route_fidelity` ranks only. A Tier-S and a Tier-T observation cannot contend today (they never
   auto-merge; adoption is the only path), so this is latent rather than live.