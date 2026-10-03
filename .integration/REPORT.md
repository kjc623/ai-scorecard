# Independent integration verification (task-14)

Verifier: contractor (owner of `contracts/` only). This report covers the components I did **not**
author: `endpoint/capture-core`, `endpoint/capture-spool`, `endpoint/classifier-host`,
`extension`, `ingestion/ingest-api`, `query/query-api`, plus the seams between them.
`contracts/` is covered only where a consumer shows it working (see §1e) — the Lead reviews the
contract itself.

Every check below states the exact command, the raw output trimmed to what decides the verdict, and
a verdict of **PASS**, **FAIL**, **PARTIAL** or **BLOCKED**. A defect claim is reproducible from this
document alone. Nothing in this report was fixed by me: findings go to the Lead, who routes them.

Generated: 2026-10-02, build round 2. Evidence directory: `.integration/`.

Everything below describes the repository as it stood at round 2, including every statement that a
component "does not exist", that Chromium is not installed, and every `file:line` citation.
`query/dashboard`, `vault/content-vault` and `azure/` have all landed since, and the three harness
defects in §5.1–§5.3 are fixed in `tools/`; each of those sections says where.

---

## 0. What the five tools prove, and what they do not

I read all five before running them. The "does not prove" column is the part that matters: it is
where my own checks had to go.

| Tool | Proves | Does **not** prove |
|---|---|---|
| `tools/accept.mjs` | Runs 6 gates and gives one exit code; a gate that cannot run is SKIPPED with a reason; the DB gate is judged on its TALLY line, not the runner's exit code. | That the six invariants hold: `check-invariants` exits 0 with INV-1 and INV-3 BLOCKED, so "[PASS] invariants" in this output means "no FAIL", not "all six verified". Also inherits the two holes below. |
| `tools/verify-all.mjs` | Each declared package's own suite runs; `MISSING` (no directory/go.mod) and `NO-TESTS` are failures, not passes; a root `go.work` is a failure. | Nothing about cross-component fit. Discovered Node tests must be named `*.test.{mjs,cjs,js}`, so `contracts/tools/verify.mjs` is invisible to it (§1e). It has **no per-package timeout** (§5.1) — one hanging package blocks the entire run. |
| `tools/check-seams.mjs` | Envelope field names in 6 components against the schema, with an explicit allow-list for non-envelope seam fields; `received_at` on a device is a finding; a component naming no contract field must carry a documented reason. | Field names only: not nesting, not optionality, **not types/encodings**. The allow-list can mask drift (e.g. `digest` is allow-listed for `endpoint/protocol` although the contract's name is `content_digest`). Test files are excluded. Only names sharing a first token with a contract field are flagged as near-misses. Non-envelope seams (native frames, classifier request/response, batch) are not compared at all. |
| `tools/check-vocab.mjs` | 8 closed vocabularies compared **by value** between `endpoint/protocol` (Go) and `extension` (JS); a value missing on either side is a finding. | Only values, not names. A file that does not exist is reported `ABSENT` and **exits 0** (§5.2). Only the extension is compared; no other consumer, and no vocabulary a component re-derives at runtime. JS extraction only reads single-quoted keys in a plain exported object literal. |
| `tools/check-invariants.mjs` | INV-2 by grep; INV-3b by a hostile-input probe plus an interpolation scan; INV-4 by a mutating-method grep on the spool; INV-5 by asking the live PostgreSQL catalog (not the DDL); INV-6 by the presence of the four states and the counters; INV-1/INV-3 report BLOCKED when their component is absent. | INV-6's own footer says it: state names existing is not "a dead path reports absent". INV-4's grep does not prove no raw-handle rewrite. INV-2's grep does not prove a device never reaches a database another way. INV-1/INV-3 cannot be verified at all yet. |

Generalisation from the Lead's L8 (a test that agrees with the bug): none of these five compare a
component against the *other side's real bytes*. That is where §1 and §2 looked.

---

## 0.5 The five gates, re-run by me, and reconciled against LEAD-VERIFICATION.md

| Gate | My command | My result | Lead's log | Reconciled? |
|---|---|---|---|---|
| seams | `node tools/check-seams.mjs` | clean, 6 components, 7-26 contract fields each, `extension` 0 with a documented reason, exit 0 | L7 "6 components ... no findings" | yes |
| vocab | `node tools/check-vocab.mjs` | 8 vocabularies `agree` (13/7/7/4/3/7/4/32 values), exit 0 | L7 "8 vocabularies ... no drift" | yes |
| invariants | `node tools/check-invariants.mjs` | INV-2 PASS, INV-3 BLOCKED, INV-3b PASS (15/15 rejected), INV-4 PASS, INV-5 PASS (live catalog 31+31), INV-6 PASS, INV-1 BLOCKED; exit 0 | L6 same six verdicts | yes, with the caveat that "PASS" for INV-6 is state-name presence only, and the tool exits 0 while two invariants are BLOCKED |
| contract drift | `node contracts/tools/generate.mjs --check` | match, exit 0 (also after my ADR 0018 change) | gate 2 of accept.mjs, "passes" | yes |
| packages | per-package with `go test -timeout 90s ./...` / `node --test` | see the table in §5.5; **`endpoint/capture-core` hangs** | L4 "capture-core compiles with core tests passing" | **no — L4 is contradicted by §6.1** |
| accept | `node tools/accept.mjs` | never returned (>45 min, `packages` gate stuck in capture-core) | not claimed to have been re-run | n/a |
| accept, hanging gate skipped | `node tools/accept.mjs --skip packages` | contract, seams, vocab, invariants, **db all PASS**; 5 pass / 0 fail / 1 skipped; exit 0, verdict "NOT a complete acceptance: gates above were skipped" | L3 DB suite | yes — and the DB suite has grown: **43 distinct assertions, 45 PASS notices, 0 failures** (L3 records 37, then 38) |

Notes on the Lead's log, checked rather than accepted:

- **L1 (endpoint/protocol 22 tests)** — reproduced as part of the per-package run: `ok` (0.29s).
- **L3 (DB suite, 37 then 38 assertions, judged on TALLY)** — I did not re-run the database suite this round (see §7). The reasoning recorded in L3 for judging on TALLY rather than the exit code is sound and is what `accept.mjs` does.
- **L6's two corrections** (INV-5 via the live catalog, INV-3b via a runtime probe) are visible in the tool and are the right shape: both replaced a text pattern with a property. The INV-3b probe's wording ("rejected with a typed error") is stronger than its evidence — it counts any throw; my own probe (§4) confirms the throws really are `QueryError`s with a reason from the closed set, so the claim is true, just not verified by the tool.
- **L8 (the test that agrees with the bug)** — the native `content` defect in §1a is the same class one level up: both sides' tests agree with their own model and no test compares the JSON one side writes with the type the other side declares.

---

## 1. Cross-component wire shapes, diffed by machine

### 1a. Extension -> capture-core native frames — **FAIL (confirmed defect)**

**Defect:** `content` is encoded as base64 by the Go seam type but as raw text by the extension.

- Go: `endpoint/protocol/native.go:87` — `Content []byte \`json:"content,omitempty"\``; `encoding/json`
  decodes a JSON string into `[]byte` as base64.
- JS: `extension/src/pipeline.js:137,158` — `contentText = body.decode.text` for
  non-binary payloads and `content: contentText` on the wire; only the binary case base64-encodes
  (`base64Of`, line 575).
- The convention is documented in the same tree: `extension/src/attachments/sender.js:187`
  — "encoding/json marshals []byte as base64, so the frame carries base64 too".
- No Go consumer parses `ObservationMessage` yet (grep: only `endpoint/protocol` and its tests, plus the
  extension's own `contract.test.mjs` which reads the Go source text). So the defect is **latent**, and
  the first core-side parser will hit it.

**Reproduction** (both files are mine, under `.integration/repro/`; no component was edited):

```
node .integration/repro/native-content-seam.mjs
cd .integration/repro/goconsume
go run . ..\frame-text.json
go run . ..\frame-base64-looking.json
go run . ..\frame-binary.json
```

Raw output (trimmed):

```
frame-text.json body.content = "Summarise the attached contract, please."
..\frame-text.json: body unmarshal FAILED: json: cannot unmarshal string into Go struct field
    ObservationMessage.content of type []uint8: illegal base64 data at input byte 9
..\frame-base64-looking.json: decoded OK content_bytes=11 content_text="hello world"
    (the user's text was "aGVsbG8gd29ybGQ=" - silently decoded to different bytes)
..\frame-binary.json: decoded OK content_bytes=18 content_text="binary-ish payload"
```

Blast radius: every M1+ text observation. Two failure modes — an outright decode error, or silent
substitution of different bytes while `content_digest` still digests the original, which breaks the
canonical digest (dedup key, `content_digest` evidence) for that record.

Why no suite catches it: `extension/contract.test.mjs:163-176` compares
`json:"name"` fields only; `test-support/fake-core.mjs` checks presence; `native.test.mjs` uses
`content: 'hi'`; `protocol_test.go` builds Go structs directly. Names agree, types do not — exactly
the class `check-seams` states it cannot see.

**Decision needed (not a fix I may make):** base64 the text path in the extension, or declare
`Content string` in the protocol type. Owner: extension-dev and/or the Lead (protocol).

### 1b. capture-core <-> classifier-host — **PARTIAL (not fully machine-diffed)**

Both sides are Go structs in one module family, so JSON field names cross via `protocol.ClassifyRequest`
/ `ClassifyResponse` and `parser.Header`. Static read: `ClassifyRequest` carries `content`, `mode`,
`media_type`, `content_digest`, `release_id`, `budget_ms` and no identity field, with a package-init
guard (`endpoint/protocol/classifier.go:205-221`) that panics if `json.Marshal` ever yields one of
`IdentityFieldNames` (line 197-200). `classifier-host/parser/parser.go:145` names its private
parent-child header field `digest` while `classify/host.go:51` names its own `content_digest` — both
are Go-struct-to-Go-struct (no JSON across a language boundary), so this is a naming inconsistency,
not a wire defect.

Not yet done: a byte-level round trip of a real frame through both sides. Verdict PARTIAL rather than
PASS, and it is listed in §7.

### 1c. capture-core <-> ingest-api batch shape — **PARTIAL**

`endpoint/protocol/batch.go` declares the batch and per-event result shapes; `ingestion/ingest-api`
validates against the schema at runtime and consumes the generated types
(`internal/contract/envelope.go:12,62`). `node tools/check-seams.mjs` reports 21 contract fields named
in that tree and no findings. Not yet done: a byte-level batch diff (device JSON -> ingest-api
decoder). Verdict PARTIAL; listed in §7.

### 1d. ingest-api <-> query-api <-> dashboard — **BLOCKED**

`query/dashboard` does not exist, so the dashboard half of this seam cannot be checked
(`check-invariants` INV-3 reports BLOCKED for the same reason). query-api reads the mart tables with
SQL rather than envelopes, so there is no envelope seam between ingest-api and query-api to diff.

### 1e. All components against contracts/generated — **PASS (as consumption)**

- `node tools/check-seams.mjs` -> clean for all 6 components; `extension` names 0
  contract fields and carries a documented reason.
- The Go consumer path is real: `ingestion/ingest-api/internal/contract/envelope.go:62` calls
  `envelope.DecodeDeviceSubmission`, so a renamed or retyped generated field breaks that build. I ran
  `cd ingestion/ingest-api && go build ./...` after my own contract change (ADR 0018) -> EXIT=0.
- `node contracts/tools/generate.mjs --check` -> match, EXIT=0; `node --test contracts/tools/` ->
  12/12 pass.
- Hole found: `tools/verify-all.mjs` looks for `*.test.{mjs,cjs,js}` under `contracts/tools`, and my
  suite is `contracts/tools/verify.mjs` (the name task-1 specifies), so that package will report
  **MISSING** once the run can reach the end. See §5.3.

---

## 2. The six invariants

### INV-2 (collectors hold no database credential) — **PASS (static)**

`node tools/check-invariants.mjs` -> PASS: no database driver, DSN or credential in 132 device-side /
browser-side source files, and no device `go.mod` requires a Postgres driver. Reproduced by me.
What it does not prove: that a device never reaches a database by other means (a shelled-out `psql`),
which needs a runtime trace.

### INV-5 (isolation is structural) — **PASS (live catalog)**

`node tools/check-invariants.mjs` -> PASS from the live container: 31 tenant-scoped tables have RLS
enabled **and forced** with 31 policies; the 6 without it are all global reference data
(`ref.classifier_release, ref.collector, ref.data_class, ref.retention_class, ref.route_fidelity,
ref.rule`). This is the check the Lead's L6 says was rewritten after the DDL-based version
undercounted by an order of magnitude; the live-catalog version is the right one.

### INV-6 (a dead path reports degraded/absent, never zero) — **PASS, behavioural, with one observation**

Executable evidence (my harness, exported API only): `.integration/repro/inv6`.

```
cd .integration/repro/inv6
go run .
```

Raw output (trimmed) and what it shows:

```
start proxy.tls    err=<nil>
start cli.shim     err=listen: port already in use
start proc.detect  err=core: provider proc.detect panicked during Start: provider exploded during Start
row   proxy.tls    state=healthy  last_success=false counters=[all 7 present]
row   cli.shim     state=absent   last_success=false counters=[all 7 present]
row   proc.detect  state=absent   last_success=false counters=[all 7 present]
ok  a provider that refused to start is NEVER healthy, although its own Health() claims healthy
ok  a panicking Start degrades only its own row, and that row is absent
ok  proxy.tls / cli.shim / proc.detect each carry all 7 counters
INV-6b: pipeline with a classifier that fails, at M1
  outcome: emitted=true degraded=true reason="classifier_degraded" mode="m1" err=<nil>
  counters: observed:1 emitted:1 errors:1 (others 0)
  spooled envelope: confidence="degraded" labels_present=true (empty label set, as the schema requires)
ok  the failure is never a silent success
INV-6: every assertion held
```

The strongest part: `cli.shim`'s provider **lied** (`Health()` returned `healthy` with a zero counter
set) and the registry overrode it to `absent` because its `Start` failed. That is the structural
property INV-6 needs, tested with a provider that is hostile rather than cooperative.

**Observation (not a defect claim).** On the degraded path the pipeline still records success:
`core/pipeline.go:498` calls `p.MarkSuccess(...)` whenever the entry was spooled, degraded or not,
while `pipeline.go:228-229` describes that value as the basis of health ("Health derives from this
positive observation, never from the absence of errors"). No code reads `LastSuccess` today (grep:
only the setter and getter exist), so nothing is currently wrong — but a provider that builds its row
as `Healthy(...)` from `LastSuccess` alone would report healthy for a route whose last observation was
classified `degraded`. Either the row must also consult `errors`/`degraded`, or `MarkSuccess` should
not fire on a degraded outcome. Blast radius: latent; owner's decision.

Also noted: an `absent` row carries `detail=""` (no cause) for a provider that failed to start — the
start error is in `StartResult.Err`, not in the row. The invariant ("never healthy") holds; a cause
would make the row more useful. Not a defect.

### INV-4 (append-only) — **PASS (static) + crash test: see §3**

`node tools/check-invariants.mjs` -> PASS: no payload-mutating method on the spool in 13 files. This
is a grep over method names; it does not prove the file layer cannot rewrite a record in place, which
is why §3 exists.

### INV-1 (content crosses only on a per-event grant) — **BLOCKED**

`vault/content-vault` does not exist. `check-invariants` reports BLOCKED and names the fact that 4
device-side files mention a grant — a mention is not an enforced egress path. My own check confirms
the missing half: there is no server-side vault component to hold content, so "upload everything is
unreachable" cannot be demonstrated. Correct verdict today: **not yet implementable**, not PASS.

### INV-3 (the dashboard cannot produce SQL) — **BLOCKED for the dashboard; INV-3b PASS for query-api**

- Dashboard: `query/dashboard` does not exist -> BLOCKED (nothing to feed hostile input to).
- query-api: reproduced `check-invariants`' probe -> 15 hostile values across three query shapes, all
  rejected with a typed error, 0 reached SQL text. I additionally ran my own probe with different
  payloads (§4). The one static interpolation site is `query/query-api/src/compile.js:121`
  (`select.push(\`__ord_${t.by}\`)`), an ORDER BY identifier; a static check cannot tell an
  allow-listed identifier from an injection, which is why the runtime probe is the verdict.

---

## 3. INV-4: append-only under a real crash — **PASS**

Executable evidence: `.integration/repro/inv4` (exported API only, outside the package's own tests).

```
cd .integration/repro/inv4
go run .
```

Raw output:

```
INV-4: spool dir C:\Users\...\Temp\inv4-spool-2247615360
  appended and closed 3 records
  before crash: depth=3 digest=d03b00df5bae8d75
  ok    the three committed records are readable before the crash (3)
  child killed while appending
  after crash:  depth=5003 digest=66c6908265985482
  ok    the spool reopened after a killed writer (writer lock released by the crash)
  ok    at least the three committed records survived (5003)
  ok    the first three records are byte-identical after the crash (append-only: nothing was rewritten)
  ok    each pre-crash record kept its sequence and payload bytes
  child records that reached the spool whole: 5000
  ok    the killed child did get records onto the spool before dying (5000)
  ok    final read after reopen: <nil>
  ok    a second reopen returns the same bytes (idempotent recovery)
INV-4: every assertion held
```

What it proves: a second process was killed (TerminateProcess, no clean close) while appending 5,000
records; the 3 records committed before the crash came back byte-identical and in sequence; the spool
reopened (writer lock released by the crash); a torn frame would have made `Peek` fail, and it did
not, so every record it returned is whole; a second reopen returned identical bytes.

What it does not prove: power-loss durability (the harness opens with `SyncEvery: -1`, exactly as the
package's own crash tests do, so the crash under test is a process kill rather than a power cut); and
a torn *tail* frame is the package's in-package `crash_test.go` territory, which passes in the
package run.

**Observation about the bound (not a defect).** My first attempt appended until the default bound and
then asserted the original three records survived — they did not, because `DefaultBounds()` is 25,000
entries and a full spool evicts the oldest by design (`spool.go:34-39`). The correct reading of
"append-only" here is: **no retained record is ever rewritten; eviction is a counted drop, not a
rewrite.** The harness now opens unbounded to test the rewrite property in isolation. The operational
consequence deserves a line in the coverage story: a device whose spool reaches 25,000 undelivered
records starts dropping the oldest, and that must surface in the `dropped` counter rather than as a
gap in the numbers.

---

## 4. Independent hostile-input probe: query-api — **PASS**

`tools/check-invariants.mjs` drives the compiler with hostile values in *identifier* positions. That
leaves the other half unprobed: a hostile value in the *value* position, where the correct behaviour
is not rejection but parameter binding. My probe (`.integration/repro/inv3b-probe.mjs`) uses the
component's own `validate -> guard -> compile` path and its own document builder, with a different
payload set.

```
node .integration/repro/inv3b-probe.mjs
```

Raw output (trimmed; the full run prints 32 value assertions and 12 identifier assertions):

```
INV-3b a) hostile values in the value position (tool eq <payload>)
  ok  payload "'; DROP TABLE ingest.observa" is not present in the SQL text
  ok  payload "'; DROP TABLE ingest.observa" contributes no SQL keyword or literal
  ok  payload "'; DROP TABLE ingest.observa" is carried as a bound parameter
  ... (8 payloads x 4 assertions, all ok: DROP TABLE, OR '1'='1, $$; pg_sleep, UNION SELECT,
       quote-escape, a"b`c, NUL byte, percent)
INV-3b b) hostile identifiers in identifier positions
  ok  group_by payload rejected as a QueryError (unknown_dimension)
  ok  order.by payload rejected as a QueryError (unknown_measure)
  ok  filters[].field payload rejected as a QueryError (unknown_dimension)
  ... (4 payloads x 3 positions, 12/12 refused)
INV-3b probe: no hostile value reached SQL text and every identifier payload was refused or resolved away
```

Two things this adds beyond the existing gate: the value-position property is now checked at all, and
the rejections are checked to be the project's own `QueryError` (`reason` in the closed `REASON` set,
`result_state` in §13's table) rather than "something threw". The dashboard half of INV-3 remains
BLOCKED because `query/dashboard` does not exist.

---

## 5. Harness defects found while running the gates

### 5.1 `verify-all.mjs` has no per-package timeout — **FAIL (confirmed)**

`node tools/accept.mjs` never returned on my run: `packages` -> `verify-all.mjs` -> `go test ./...` in
`endpoint/capture-core` hung for >45 minutes with no output. Two concurrent accept runs were stuck the
same way. Root cause is a component defect (§5.4), but the harness turns it into "the acceptance
command hangs forever" instead of "one package failed".

**Resolved since:** every package now runs under a hard budget and `TIMEOUT` is its own status
(`tools/verify-all.mjs:31`, `:226-259`).

### 5.2 `check-vocab.mjs` treats a missing file as a pass — **FAIL (confirmed, by construction)**

`tools/check-vocab.mjs:128-131` records `ABSENT` and `continue`s when either the Go or the JS file is
missing; findings stay empty and the script exits 0 with "No vocabulary drift". A deleted consumer is
therefore a green gate. At round 2 no vocabulary was ABSENT (output shows all 8 `agree`), so this was
a latent hole, not a live false pass.

**Resolved since:** a missing file is now recorded as a `consumer-absent` finding, and any finding
makes the script exit non-zero (`tools/check-vocab.mjs:128-137`, `:225`).

### 5.3 `verify-all.mjs` cannot see `contracts/tools` — **FAIL (confirmed by code + discovery rule)**

`findNodeTests` (lines 89-105 at round 2; now 133-149) matches `*.test.{mjs,cjs,js}` only. `contracts/tools/verify.mjs` does
not match, so the package reports `MISSING` ("no *.test.{mjs,cjs,js} under contracts/tools") once the
run completes. The task-1 acceptance command (`node --test contracts/tools/`, which works through
`contracts/tools/index.js`) is not the same discovery path. Fix is either a one-line
`contracts/tools/verify.test.mjs` shim (my scope, if the Lead wants it) or a harness rule for the
documented `verify.mjs` name (Lead's scope).

**Resolved since:** a package with no `*.test.*` file is now run through its `index.{mjs,cjs,js}`
entry (`tools/verify-all.mjs:176-182`, `:207`), which is how `contracts/tools` is reached.

### 5.4 `endpoint/capture-core` hangs the gate — **FAIL (confirmed defect, component)**

See §6.1.

### 5.5 Per-package acceptance run, with explicit timeouts

`tools/verify-all.mjs` has no timeout, so I ran each package myself with one:

```
cd <package>; go test -timeout 90s ./...
```

| Package | Command | Result |
|---|---|---|
| `endpoint/protocol` | `go test -timeout 90s ./...` | **PASS** (`ok`, 0.29s) |
| `endpoint/capture-core` | `go test -timeout 90s ./...` | **FAIL** — `core`, `dedup`, `policy`, `proxy/loopback` all `ok`; `proxy/tlsproxy` hangs (§6.1) |
| `endpoint/capture-spool` | `go test -timeout 90s ./...` | **PASS** (`ok`, 2.7s; includes the in-package crash tests) |
| `endpoint/classifier-host` | `go test -timeout 90s ./...` | **PASS** (root, `classify`, `model` all `ok`, 5.7s) — the 5 compile errors in `parser/parser.go` recorded in L4 are fixed |
| `ingestion/ingest-api` | `go test -timeout 90s ./...` | **PASS** (all internal packages `ok`) |
| `contracts/generated/go` | `go build ./...` | **PASS** (build only, by design) |

Not run by me in this round: `extension` (node), `query/query-api` (node),
`database/tools`, `query/dashboard`, `azure/tools`, `vault/content-vault`. The last three do not exist.
The database suite **was** re-run through `node tools/accept.mjs --skip packages` (43 distinct
assertions, 0 failures). This is why §7 keeps a "not verified" line for each of the others.

---

## 6. Confirmed defects

### 6.1 Self-deadlock in the TLS proxy `Start()` — **CONFIRMED, critical**

`endpoint/capture-core/proxy/tlsproxy/provider.go`: `Start()` locks `p.mu` at line 266 and calls
`p.probe(ctx)` at line 267; `probe()` calls `p.ListenAddr()` at line 294, which locks `p.mu` again at
line 194. `sync.Mutex` is not reentrant, so `Start` blocks forever.

Reproduction:

```
cd endpoint/capture-core
go test -run TestTLS_5_3_InterceptsEligibleDestination -timeout 15s ./proxy/tlsproxy/
```

Raw goroutine stack (trimmed to the chain):

```
sync.(*Mutex).Lock -> provider.go:194 ListenAddr -> provider.go:294 probe -> provider.go:267 Start
  -> provider_test.go:258 TestTLS_5_3_InterceptsEligibleDestination
panic: test timed out after 15s
```

Impact: every `Provider.Start()` call deadlocks the calling goroutine, which is the supervisor's
startup path for TLS interception, not just a test. It also hangs `go test ./...`, and therefore the
whole acceptance run (§5.1).

### 6.2 Native frame `content` encoding — **CONFIRMED**

See §1a.

### 6.3 Latent producer hole, model_detection window fields — **CONFIRMED, latent**

After ADR 0018 the contract forbids `window_end`, `submission_count` and `bytes_total` on
`model_detection`. `endpoint/capture-core/core/envelope.go:120` (`BuildEnvelope`) refuses
content-derived fields for non-prompt kinds (lines 135-137) but copies `WindowStart`, `WindowEnd`,
`SubmissionCount`, `BytesTotal` for **any** kind (lines 154-157). No caller sets them for a detection
today (the only test that sets them is a `usage_rollup`, `pipeline_test.go:608`), so nothing
currently emits an invalid record — but a caller defect would now be refused at ingest instead of at
mint time. A kind-consistency refusal in `BuildEnvelope` would make it unreachable earlier.

---

## 9. Boundary checks (task-14 check 3)

Each answer is code-read evidence with file:line; where a tool or harness already exercises it, that
is cited instead.

| Question | Verdict | Evidence |
|---|---|---|
| Does any component read content at M0? | **No, by construction** | `endpoint/capture-core/core/pipeline.go:506-520` — `readContent` is documented and used as *the single call site* of `ContentReader.Read`, and it refuses when `!mode.ReadsContent()`. `core/envelope.go:132-137` refuses content-derived fields at M0 before an envelope is minted. `endpoint/protocol/classifier.go:184-193` refuses a classify request carrying content in a mode that forbids reading. The extension's `modeReadsContent` (`src/messages.js:82-84`) treats an absent/unknown mode as "do not read". |
| Does the classifier receive tool identity, `user_ref` or destination? | **No** | `endpoint/protocol/classifier.go:22-51` — `ClassifyRequest` has `content`, `mode`, `media_type`, `content_digest`, `release_id`, `budget_ms` and no identity field; `:197-200` names the forbidden set; `:205-221` marshals an empty request at package init and panics if any of those names appears. `core/pipeline.go:548-562` builds the request from bytes, mode, media type and digest only. |
| Does anything write to the spool other than capture-core? | **No — and the extension cannot reach it at all** | The only package that opens the spool is `endpoint/capture-spool` (`spool.go:180` `Open`); consumers hold the `protocol.Store` interface (`endpoint/protocol/spool.go:151`, `core/pipeline.go:56-67`). `extension/src` imports no filesystem module and never names a spool path (grep: the 9 `spool` mentions are all prose/comment or the `spool_unwritable` detail value); `src/queue.js:4-18` states the buffer is in memory and bounded because "the extension cannot read the spool". |
| Does the vault appear on a public route? | **BLOCKED** | `vault/content-vault` does not exist and neither does `azure/`, so there is no vault deployment to inspect. Same verdict as INV-1. |

---

## 7. What is NOT verified

This section is deliberately the one that grows.

- **capture-core's own suite cannot run to completion** (§6.1). Everything below that depends on it is
  unverified: its `dedup` and `policy` packages pass individually, but the package-level `go test ./...`
  verdict does not exist.
- **Byte-level seam round trips not done:** capture-core <-> classifier-host framing and version
  handshake (§1b); capture-core <-> ingest-api batch shape (§1c). Both are read, not exercised.
- **No browser, no real device, no cloud.** Chromium was not installed at round 2; nothing was exercised in a
  real extension host, over a real native-messaging pipe, or against Azure/KMS/Blob. Any claim that
  depends on those is unverified by construction.
- **`query/dashboard`, `vault/content-vault`, `azure` do not exist**, so INV-1 and INV-3 (dashboard)
  are BLOCKED, not pass. Same for the ingest-api <-> query-api <-> dashboard envelope seam.
- **INV-4's ingest side** (append-only in the database) is the DB suite's T17/T18/T19/T20, judged by
  the TALLY line; I re-read the runner but did not re-run the database suite myself in this round.
- **Type-level agreement inside the seam is unchecked by every tool**: `check-seams` compares names,
  `check-vocab` compares enum values. §1a is the first defect of that class; there may be more
  (no tool compares a JSON *value type* on one side with the declared type on the other).
- **JS-side extraction holes**: `check-vocab` only reads single-quoted keys in plain exported object
  literals, so a switch to double quotes or a computed key would silently reduce its coverage (it
  would report `consumer-group-missing`, which does fail — but a partially-parsed group could pass
  with fewer values).
- **Not re-run in this round:** `extension`'s suite and `query/query-api`'s suite
  (node), and `query/dashboard` / `vault/content-vault` / `azure` (absent). A package I did not run
  is not covered by this report, whatever its owner says about it. The database suite was re-run
  (`node tools/accept.mjs --skip packages` -> 43 distinct assertions, 0 failures) but judged, as the
  Lead's log argues, on its TALLY line rather than its exit code.
- **No hostile review of every seam was completed.** I produced two confirmed defects and several
  refuted suspicions for the native seam and the classifier seam; the "two most likely defects per
  seam, CONFIRMED or REFUTED" table for ingest-api/query-api is not finished. The refuted ones so far:
  `parser.Header.Digest` vs `classify/host.go`'s `content_digest` (Go-to-Go struct round trip, no wire
  boundary, not a defect); `StageResult.Duration`'s `duration_ms` JSON name (a `time.Duration`
  marshals as nanoseconds, which the same file warns about for `budget_ms` — **no JSON consumer of
  that field exists yet**, so it is a trap for the first non-Go reader rather than a live defect).
- **`ingestion/ingest-api`'s runtime behaviour** (the write path, idempotency, per-event outcomes) was
  not exercised against a live database by me; its package tests pass, and the database-side
  invariants are the DB suite's.

---

## 8. Variance list (ranked by blast radius)

| # | Disagreement | Citations | Blast radius | Status |
|---|---|---|---|---|
| 1 | Observation `content` is raw text on one side and base64 (`[]byte`) on the other | `extension/src/pipeline.js:137,158` vs `endpoint/protocol/native.go:87` (convention at `attachments/sender.js:187`) | Content identity: every M1+ text observation; digest/dedup break | **confirmed** (§1a) |
| 2 | `model_detection` window fields: contract forbids all four; the DB CHECK forbids only `window_start`, and omits `classifier_version`, `content_excerpt`, `attachments` from the forbidden set | `contracts/event-envelope.schema.json` model_detection branch vs `database/schema.sql:793` | The store's second line of defence is weaker than the contract; a gap in service validation would store a shape the contract refuses | open, owner is database/ |
| 3 | `BuildEnvelope` passes window fields for any kind; the contract now forbids them on detections | `endpoint/capture-core/core/envelope.go:154-157` vs the model_detection branch (ADR 0018) | Latent producer hole; caught at ingest instead of at mint | **confirmed, latent** (§6.3) |
| 4 | Health derives from "a positive observation", but `MarkSuccess` fires on a degraded one | `endpoint/capture-core/core/pipeline.go:498` vs `pipeline.go:228-229` and `core/health.go:5-9` | A row built from `LastSuccess` alone could read healthy for a route whose classifier failed | open, latent (nothing reads it yet) |
| 5 | `verify-all.mjs` cannot discover `contracts/tools/verify.mjs` | `tools/verify-all.mjs:89-105` vs task-1's required file name | A passing package would report MISSING once the run completes | resolved since — `tools/verify-all.mjs:176-182` (§5.3) |
| 6 | `check-vocab.mjs` reports a missing file as `ABSENT` and still exits 0 | `tools/check-vocab.mjs:128-131` | A deleted consumer is a green gate | resolved since — `tools/check-vocab.mjs:128-137` (§5.2) |
| 7 | INV-3b's tool reports "typed error" while counting any throw | `tools/check-invariants.mjs:141` (`catch { errored++ }`) | Overstated evidence; the underlying behaviour is correct (my probe confirms real `QueryError`s) | informational |
| 8 | `duration_ms` is a Go `time.Duration` (nanoseconds on the wire) while the same file warns about exactly that for `budget_ms` | `endpoint/protocol/classifier.go:103` vs `classifier.go:44-46` | Trap for the first non-Go consumer of `stages[].duration_ms`; none exists today | informational |
