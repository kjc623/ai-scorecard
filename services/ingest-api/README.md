# ingest-api — the validating write path

The one component that writes events (`docs/00-architecture.md` ADR 0001). Collectors hold no
database credential; the device's only capability is to call the device-facing endpoints as itself
(`docs/02-ingest-and-transport.md` §2.4).

Serves `POST /v1/events` per §5.3 and §6. Everything below is here rather than in a log file,
because the raw evidence logs under `evidence/` are git-ignored (`.gitignore`: `*.log`) and a fresh
clone will not carry them. Regenerate them with the commands in [Running the checks](#running-the-checks).

## The endpoint, exactly

```
POST /v1/events
  Content-Type: application/json
  Content-Encoding: gzip          optional (§5.3)
  TLS 1.3, client certificate required (§2.1)

  200 { "schema_version", "batch_id", "received_at", "server_time",
        "counts": { "accepted", "duplicate", "rejected" },
        "results": [ { "event_id", "outcome", "submission_id"?, "dedup_tier"?,
                       "won_fields"?, "first_received_at"?, "reason"?, "detail"? } ] }
```

* The request body is `protocol.EventBatch` and the response is `protocol.EventBatchResponse`
  (`device/protocol/batch.go`) — consumed, never re-declared. A test feeds the response bytes into
  the device's own `EventBatchResponse.Validate(sentIDs)`, so the shape cannot drift from the client
  that reads it.
* **A batch that parses always returns 200**, even when every event inside it is rejected. The
  per-event outcome is the contract.
* Batch-level failures use §5's common envelope
  `{"error":{"code","detail","server_time"[,"retry_after_s","message"]}}`:

  | Status | When | `code` |
  |---|---|---|
  | 400 | batch shape, `event_count` mismatch, unsupported batch `schema_version` | `schema_violation`, `unsupported_schema_version` |
  | 400 | fewer than 1 or more than 500 events | `oversize` (§5.3 lists the count under 400) |
  | 413 | body over 8 MiB compressed / 32 MiB decompressed, or one envelope over 256 KiB | `oversize` |
  | 401 | no/bad/expired/revoked credential | `revoked_device` |
  | 403 | unknown or inactive tenant; region mismatch | `unknown_tenant`, `region_mismatch` |
  | 409 | `batch_id` already accepted in the replay window | `duplicate_batch` |
  | 405 | any method but POST | `schema_violation` |
  | 503 | write failure — nothing committed, retryable | `schema_violation` |

  `server_time` is on every response so the device can measure clock offset (C26).

* Per-event rejections carry a code from §7's **closed** set plus
  `detail{pointer, expected, supported?, presence_map}`. `expected` never echoes the offending
  value (it can be content); §7 keeps field *names*, which is what `presence_map` is.

* `won_fields` means "the submission's contest-winning route is this observation's route" — not
  "this observation changed a field". See the comment on `store.EventOutcome`.

## The integration seam

One transaction per batch, and this is the write:

```sql
SELECT event_outcome, event_submission_id
  FROM ingest.record_event($1::jsonb, $2::timestamptz)
```

`$1` is the envelope **exactly as the device sent it** (never re-marshalled), `$2` is the batch's
single receive time. It returns `inserted` | `merged` | `duplicate`.

Transaction order (`internal/store/sql.go` holds every statement as a constant):

1. `SELECT set_config('app.tenant_id', $1, true)` — the RLS session tenant, transaction-local.
2. `SQLPrincipalStatus` — the credential re-check **before commit** (§2.3). A revocation between
   admission and commit writes nothing.
3. `SQLRecordEvent` once per accepted event, in request order.
4. For a `duplicate`: `SQLFirstReceivedAt`. For a `merged`: `SQLSubmissionWinner`, so `won_fields`
   reports what the store decided rather than a second opinion.
5. `SQLInsertRejected` per rejected event — the quarantine rides the same transaction (§6 step 5).

Validation happens entirely **in memory before the transaction opens** (§6). That is not stylistic:
`ingest.record_event()` refuses an M0 record carrying content by *raising*, which aborts the whole
transaction — so a device defect validated late would be a batch that can never commit.

Nothing in this service re-implements the ladder. Idempotency is the `(tenant_id, event_id)`
primary key and merging is `ingest.record_event()`'s, per §6's "enforced by the store, not by a
check in application code".

### The in-memory store

`store.Memory` is a test double that transliterates `ingest.record_event()`'s semantics so the
service can be tested without a database. It is not trusted: `internal/ladder` holds the §4
conformance fixture, and the same scenario table runs twice — once against `Memory`, once against
the real stored procedure in `internal/store/db_integration_test.go`. A drift between them fails a
test instead of becoming a production surprise. The Go mirror of `ingest.weak_dedup_key()` is
compared value-for-value against the stored function for the same reason.

## Contract validation, and why it is two checks

`internal/contract` validates each envelope twice, deliberately:

1. **`contracts/generated/go/envelope`** (T1) decodes it. The generated type is the compile-time
   field vocabulary: `envelope.DecodeDeviceSubmission` is called for every event and
   `envelope.AllKinds()` is the closed kind registry, so a renamed or retyped field breaks the build.
2. **`contracts/event-envelope.schema.json`** is walked for the JSON Pointer and violated-constraint
   text §7's `detail` is made of — the generated decoder names fields in prose and cannot produce a
   pointer.

A record must pass both. Where they can disagree, the schema walk wins, in exactly one place:
draft 2020-12 treats `format` as an annotation, so the generated decoder asserts only that a uuid
field is non-empty, while the walk asserts the uuid shape. Ingest must assert it, because
`record_event()` casts those fields with `::uuid` and a malformed value would abort the batch inside
the database. `TestTwoValidatorsAgreeAndTheSchemaWalkIsStricter` pins both verdicts.

Neither check re-declares the envelope: the wire shape lives in the contract, and `contract.Envelope`
is a name-keyed projection over the raw object used for reading fields, redaction and the presence map.

## Reason codes

The wire vocabulary is `protocol.ReasonCode` (docs/02 §7), closed. `store.QuarantineReason` maps it
onto `ingest.rejected.reason_code`, which now uses the same spellings. Two codes have **no honest
quarantine row**, and are reported to the device without one:

| Wire code | Why no `ingest.rejected` row |
|---|---|
| `tenant_mismatch` | `.tenant_id` is NOT NULL and RLS-scoped; a body claiming another tenant has no honest tenant to file under, and filing it under the authenticated one would misattribute the defect |
| `duplicate_batch` | batch-level by construction (§5.3): there is no per-event envelope to quarantine |

`TestQuarantineMappingIsTotal` asserts that every code in the closed set is either mapped or one of
those two, so a new code cannot quietly lose its quarantine row. dbuilder's
`db/tools/check-schema.mjs` checks the other direction across the seam (every code this service
emits is one the CHECK accepts).

## Running the checks

No network access, no `go get` (`GOPROXY=off`); PostgreSQL is reached through `docker exec psql`
because there is no wire driver available offline.

```powershell
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
cd services/ingest-api
go build ./... ; go vet ./... ; gofmt -l . ; go test ./... -count=1
go test ./internal/store -run TestLive -v -count=1   # live PostgreSQL: the ladder, the key mirror, the quarantine
```

The `TestLive*` cases need a running PostgreSQL 17 with `db/schema.sql` applied. They find it by
probing running containers whose image is PostgreSQL (override with `SHADOWPG_CONTAINER`), run every
script inside a transaction that ends in `ROLLBACK`, and **skip** when none is found — so
`go test ./...` is green on a machine without Docker rather than red for the wrong reason.

The whole-module gate is `node tools/verify-all.mjs --only go`.

### Running it locally

```powershell
cd services/ingest-api
go build -o ingest-api.exe ./cmd/ingest-api
.\ingest-api.exe -addr 127.0.0.1:18449 -store memory -dev-trust-principal `
  -dev-seed-principal "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222"
```

Then POST with `X-Dev-Tenant-Id` / `X-Dev-Device-Id`. `-dev-trust-principal` is loudly logged and
refuses to run alongside `-tls-client-ca`; `-dev-seed-principal` is refused with `-store sql`
(there the principal comes from `ops.*`); and without a client CA or the dev flag the binary refuses
to serve unauthenticated. In a deployment the flags are
`-tls-cert/-tls-key/-tls-client-ca` (TLS 1.3 minimum, `RequireAndVerifyClientCert`), `-region`, and
`-store sql -driver … -dsn …`. Route ranks are never compiled in: `-routes-file` carries
`ref.route_fidelity` rows (default `testdata/route-fidelity.seed.json`, the rows `db/schema.sql`
seeds).

## Test map

| Test | What it proves |
|---|---|
| `internal/ladder` + `internal/ingest.TestLadderAgainstTheStore` + `internal/store.TestLiveRecordEventRunsTheLadder` | the §4 ladder: same event two routes, lower-fidelity-last, Tier-T-then-Tier-S **not** merged (two rows), bucket boundary, idempotent replay, rollup supersede, detection collapse — the same table against the mirror and against the live stored procedure |
| `TestM0PromptCarryingContentIsRejected` | docs/01-collectors §7.1: an M0 prompt carrying `content_digest`/`labels`/… is rejected `mode_violation` with the offending pointer |
| `TestReasonCodeVocabulary` | each §7 code is reachable through the check that produces it |
| `TestQuarantineMappingIsTotal` | the mapping is total over the closed set, with two documented exceptions |
| `TestEndpointShapeIsTheProtocolShape` | the 200 body survives the device's own validator, field by field |
| `TestBatchThatParsesAlwaysReturns200` | §5.3's central promise, including an all-rejected batch |
| `TestRevokedCredentialInsideTheTransaction` | §2.3: a revoked principal writes nothing |
| `TestMTLSAuthenticatorOverARealHandshake` | a real TLS 1.3 handshake with a §2.2-shaped certificate drives tenant (OU) and device (CN) extraction |
| `TestLiveWeakDedupKeyMatchesTheGoMirror` | the in-memory double decides on the same material the database does |
| `TestAdoptionTakesTheKeyAndTheDigestTogether` | the adopt path takes the exact key **and** its digest, and promotes `merge_confidence` |
| `TestWriteBatchIsOneTransaction` | no partial commit on a mid-batch failure |

## Not verified

Stated so a reader does not infer more than the tests show.

1. **The `database/sql` plumbing is not executed.** This repository has no PostgreSQL wire driver
   offline, so `store.NewSQL` is never driven end to end. What *is* verified is the statement text —
   executed through `PREPARE`/`EXECUTE` against the live schema — and the stored procedure's
   semantics. A binary embedding this service must register a driver; `sql.Open` without one fails
   with a message that says so.
2. **No concurrency testing of the SQL path.** The memory store is mutex-serialised; the database
   path relies on the unique constraints and row locks, which were not stressed.
3. **`duplicate_batch` is a per-process window** (`internal/batchguard`). With more than one ingest
   instance, a cross-instance replay degrades to per-event `duplicate` outcomes — §6 calls that
   correct for the data and wrong only for the diagnosis.
4. **No rate limiting.** There is no 429 path; `retry_after_s` is emitted only on 503.
5. **mTLS is tested against a generated CA**, not real CA material or the edge; the region check is
   a string comparison against `ops.tenant.residency_region`.
6. **§4.2 canonicalisation has no NFC.** It needs `golang.org/x/text/unicode/norm`, which cannot be
   fetched offline, so `dedup.DefaultCanonical` uses the identity normaliser. It is off the request
   path — the wire carries the finished `content_digest`, so nothing this service writes depends on
   it — and the gap is tracked as its own task (`device/canon/`).
7. **`won_fields` is route-based**, as described above.
8. **Interpretations made where the documents leave room**, each also in a code comment:
   an element of `events` that is not a JSON object, or has no usable `event_id`, is a batch-level
   400 (a per-event result is keyed by `event_id`, so there is nothing to report it against); the
   count outside 1–500 is 400 with code `oversize` while body/envelope caps are 413, following
   §5.3's status list; the wire `presence_map` carries present names only, while the quarantine row
   carries `{"present":[…],"missing":[…]}`; the quarantine drops `content_digest` as well as
   `content_excerpt`, because `db/schema.sql`'s `rejected_no_digest` is stricter than §7's wording
   and its CHECK is the rule that must be satisfied.
9. **§7 has no `device_mismatch` code.** A body `device_id` that disagrees with the credential is
   reported as `schema_violation` against the authenticated value rather than under a code that
   would misname it. A named code would be a contract change.
10. **A latent spec-vs-store divergence, for whoever owns §4 conformance:** §4.5's "tier dominates
    rank" is implemented and tested in `dedup.Beats`, but the deployed tie-break compares raw
    `ref.route_fidelity` ranks only. Today a Tier-S and a Tier-T observation cannot contend (they
    never auto-merge; adoption is the only path), so this is latent rather than live.
