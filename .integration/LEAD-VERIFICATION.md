# Lead verification log

Evidence the Lead reproduced **personally**, as distinct from reports received from component owners.
Every entry: the exact command, what it returned, and what it does and does not prove. This file is
deliberately narrow — it records only checks the Lead ran, so the integration verifier's independent
report (`.integration/REPORT.md`) stays a separate document.

Generated: 2026-10-02 · Build round 2

---

## L1. `device/protocol` — the seam every device component consumes

```
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
cd device/protocol ; go vet ./... ; go test ./...
```

`go vet` clean; `go test` → `ok`, **22 test functions pass**.

Covers: the four wire contracts the component owns — native messaging (extension ↔ capture-core),
length-prefixed framing with a version handshake (capture-core ↔ classifier-host), the spool record
interface, and the `POST /v1/events` batch shapes — plus the rules that make the invariants structural:
M0 content is refused at the door, the classifier request carries no identity field (a test fails if one
is added), the counter and detail vocabularies are closed, and the attachment descriptor serialises the
contract's own `content_digest` name.

Does **not** prove: that any consumer honours these shapes. That is L5's job and the verifier's.

## L2. `ingest.record_event()` adopt path — defect and fix

Found by ingestor in round 1 (raised `submission_exact_key_implies_digest` when an exact observation
arrived on a route of equal or worse fidelity). Reproduced independently by the Lead against the live
server inside a rolled-back transaction (`.tools/tmp/probe-adopt.sql`, discarded after the run):

```
BEGIN; set_config('app.tenant_id','11111111-1111-7111-8111-111111111111',false); …
  PROBE A1  weak m0 ext.web_request                  -> inserted
  PROBE A2  exact m1 SAME ROUTE (equal fidelity)     -> merged     <- used to raise
  PROBE A3  submissions for the probe tool           -> 1          <- no double count
  PROBE A4  adopted row: merge_confidence=high has_exact_key=t has_digest=t
ROLLBACK;
```

**Verdict: fixed.** The equal-rank case — the one the original T26 never covered, and the reason the
defect was invisible — now merges, keeps one submission, adopts the exact key *with* its digest, and
promotes `merge_confidence` off `low`.

## L3. The full database invariant suite, re-run by the Lead

```
powershell -NoProfile -ExecutionPolicy Bypass -File db\tools\run-invariants.ps1
```

```
schema_exit=0  invariants_exit=0
schema.sql  sha256 F0CC80B2F2C117FACC0691ECDC591D42E8845A80484E49666E7DA7F5F1E3AFF0
tests.sql   sha256 E658DAB7478CB5B68BB72FCDEC3B867E92E45F3D3C50BD4C7B1A7ECD3829EEA8
TALLY pass=39 fail=0 distinct_ids=37
```

**37 distinct assertions passed at that point (T1–T37) against PostgreSQL 17.11**, applied by
`db/schema.sql` and asserted as the runtime roles rather than as a superuser — which is the whole
point, since a superuser bypasses row-level security and would prove nothing. T36/T37 are the new
equal-rank adopt assertions from L2.

The suite has since grown to **38 distinct assertions (T1–T38)**, all passing, after the reason-code
alignment added a mapping test. The count is now stated consistently in README.md,
.cockpit/project.json, docs/00-architecture.md, docs/03-data-platform.md and db/invariants.test.sql —
it had been 27 in two documents the earlier correction never reached, which `db/tools/check-schema.mjs`
now detects mechanically by parsing the documents rather than trusting a number written by hand.

Two caveats this log must carry:

- **The runner's exit code was wrong; it has since been fixed.** Its error counter grepped for the
  substring `ERROR`, which matched `ON_ERROR_STOP` in the echoed command line, so a healthy run
  exited 1. The owner anchored every pattern to a real diagnostic and verified both directions with a
  negative control (a failing run reports `error_lines=1`, not 2). `tools/accept.mjs` still judges the
  database gate on the TALLY line rather than the exit code, deliberately: a gate that trusts an exit
  code it has seen lie once is a gate that will lie again.
- **Version substitution.** The deployment target is PostgreSQL 16 (ADR 0002); 17.11 is what is
  locally available with no network. Every construct used has a minimum version ≤13, which is an
  argument, not a run, and it is recorded as such rather than being presented as verification.

## L6. The six invariants, checked mechanically (round 2)

```
node tools/check-invariants.mjs
```

Verdict at the time of writing: **INV-2, INV-3b, INV-4, INV-5, INV-6 PASS; INV-1 and INV-3 BLOCKED**
(their components do not exist yet — reported as blocked, never as pass).

Two of these checks were wrong on first writing and the corrections are worth recording, because both
were the same mistake: a check that looks for a pattern instead of the property.

- **INV-5 first read the DDL** and counted 2 `ENABLE ROW LEVEL SECURITY` statements, because most of
  this schema's policies come from a `DO` block looping over a table list with `EXECUTE format(...)`.
  It now asks the live catalog: **31 tenant-scoped tables with RLS both enabled and forced, 31
  policies**, and the 6 tables without it are all global reference data with no tenant column
  (`ref.collector`, `ref.data_class`, `ref.route_fidelity`, `ref.rule`, `ref.retention_class`,
  `ref.classifier_release`) — which is the distinction that makes the check mean something, since a
  policy on those would compare a column that does not exist.
- **INV-3b first flagged `services/query-api/src/compile.js:121`** (`select.push(\`__ord_${t.by}\`)`)
  as SQL interpolation. A static check cannot tell an allow-listed identifier from an injection, so
  the check now drives the compiler with five hostile values across three query shapes in a child
  process: **15 of 15 rejected with a typed error, 0 reached SQL text**. The finding stands as a line
  to watch, not as a defect.

## L7. Cross-component vocabulary and seam checks (round 2)

```
node tools/check-vocab.mjs      # 8 vocabularies, protocol vs extension, diffed by value: no drift
node tools/check-seams.mjs      # 6 components, field names against the contract: no findings
node tools/accept.mjs           # 6 gates: packages, contract drift, seams, vocab, invariants, database
```

The vocabulary check exists because of a defect class this project has already produced once: two
components spelling the same closed enum differently compiles on both sides and fails only on the
rejection path. The most recent instance was between the ingest wire codes and the database
quarantine CHECK, where three were pure renames.

## L8. A defect class worth naming: the test that agrees with the bug

The strongest evidence produced this round is not a pass; it is a catch. The database owner's first
rewrite of the reason-code CHECK silently dropped `revoked_device`, and the behavioural assertion
passed anyway — because the same misunderstanding was in the code and in the test's expected list. A
test written from the same misunderstanding as the code confirms the misunderstanding. What caught it
was a static check comparing the CHECK against two independent artefacts: the wire enum parsed out of
`device/protocol/batch.go` and the codes `store.QuarantineReason` can actually emit.

The generalisation, which is why it belongs in this log rather than in a commit message: **a
component's own suite can only ever confirm its author's model of the world.** Every check in
`tools/` that matters compares a component against something outside it — the contract, the live
catalog, another component's enum. That is also the instruction I gave the verifier.


## L4. End-to-end acceptance harness (round 2, final)

```
node tools/accept.mjs
```

Nine of thirteen packages pass, zero fail, with the three absences named rather than skipped:
`contracts/tools` (12 tests), `apps/capture-extension`, `services/query-api` (150 tests),
`device/protocol` (22), `device/capture-core` (111), `device/capture-spool` (33),
`device/classifier-host` (88), `services/ingest-api` (156), `contracts/generated` (build is the
assertion, reported `BLDOK` so it is not miscounted as "no suite"). Missing: `apps/dashboard`,
`db/tools`, `infra/tools`. `services/content-vault` is no longer missing — it has a module and no
suite yet, reported `NOTS`.

Every structural gate passes: contract codegen drift, seam field check, vocabulary drift, the six
invariants, and the database suite at **43 assertions, 45 PASS notices, 0 failures**.

**Four harness defects were found and fixed this round**, and every one of them was a way for a
green result to mean nothing:

- **No per-package timeout.** `capture-core`'s TLS provider deadlocked on a non-reentrant mutex, so
  `go test ./...` never returned and `node tools/accept.mjs` hung instead of reporting. A gate that
  cannot fail is worse than one that fails; every package now has a hard budget and `TIMEOUT` is a
  distinct status.
- **Temp directories outside the workspace.** The file sandbox denies them, so `TestDiscoverWalksUp`
  failed with "Access is denied" — a sandbox boundary wearing the costume of a component defect.
  Runners now get `TMP`/`TEMP` inside the workspace, but *not* inside the repo, because a component
  that walks up from its temp dir looking for a repository marker would otherwise find this one and
  change its own verdict.
- **`contracts/tools` reported MISSING while holding 12 passing tests**, because its suite is
  `verify.mjs` re-exported from `index.js` and discovery looked only for `*.test.*`. A suite does not
  have to be named after the runner.
- **A module with one test-less package was reported as having no suite at all.** `go test ./...`
  prints `?  cmd/...  [no test files]` next to eight passing packages; the check now requires that
  *nothing* in the module has tests before it says NO-TESTS.

There is a fifth, still open: the `packages` gate fails while three declared components do not exist,
which is the honest outcome — "the directory is not there yet" is the state this harness exists to
make visible — but it means a green `accept.mjs` is not available until dashboard, infra and the
vault's suite land.

## L5. Seam check against the contract (round 1; superseded by L7)

```
node tools/check-seams.mjs
```

Derives the field vocabulary from `contracts/event-envelope.schema.json` (27 core fields, 13 required, 3
kinds, 4 modes, 7 routes; M0 forbids 6 content-derived fields) and checks every component that declares
envelope-shaped fields. Current verdict: clean for `device/protocol`, `device/capture-core`,
`device/capture-spool`; three components declare none and carry a documented reason — classifier-host
(bytes → labels, no identity by design §3.3), capture-extension (emits observations; capture-core mints the
envelope §3.4), ingest-api (validates against the schema at runtime rather than declaring types).

Does **not** prove: nesting, optionality or types across a seam. Behavioural fixtures are the verifier's.

---

## L9. The cross-component device harness (round 3)

```
node tools/verify-all.mjs      # device/integration is one of its packages
cd device/integration && go test ./... -count=1
```

Every device component had its own suite, and every one of those suites tested the component
**against its own fakes**. None could show the pieces compose, and that gap was not theoretical: the
extension sent observation content as raw text while `device/protocol` declared it `[]byte`, so text
failed to decode outright and text that happened to *be* valid base64 decoded silently to different
bytes than the user typed, while the digest covered the original. Both sides' suites were green —
each was testing its own assumption.

`device/integration/` imports capture-core, capture-spool and protocol and is the only place they
are wired together. The golden frames are generated by
`apps/capture-extension/tools/emit-frames.mjs` using **the extension's own** `observationBody()` and
`frame()`, so what the harness reads is what the extension produces, not a fixture that agrees with
the Go types by construction.

**Verified by negative control, because a passing test proves nothing until it has failed.** A golden
frame was mutated to send its content raw, the way the pre-fix extension did:

| Mutation | Result |
|---|---|
| ASCII payload sent raw | `content is not valid base64 (illegal base64 data at input byte 9)` |
| A payload that *is* valid base64, sent raw | `content decodes to "hello world", but this case's payload is "aGVsbG8gd29ybGQ="` |

The second row is the one that matters: a "does it decode?" test cannot see it. Frames were restored
and the suite re-run green in both cases.

The device path test proves four things beyond the seam: the spooled payload is a contract-shaped
envelope with no `received_at`; M0 calls neither the content reader nor the classifier (counted, not
asserted); a refused spool write produces an attributable outcome rather than a silent drop; and a
missing canonicaliser degrades the record and never produces a Tier-T key over a digest the device
cannot prove.

## L10. Acceptance after round 3

```
packages: 14   pass: 11   fail: 0   missing: 2   no-tests: 1   timeouts: 0
```

Passing: `contracts/tools`, `apps/capture-extension`, `apps/dashboard`, `services/query-api`,
`device/protocol`, `device/integration`, `device/capture-core`, `device/capture-spool`,
`device/classifier-host`, `services/ingest-api`; `contracts/generated` reports `BLDOK` (a build is
its assertion). Missing: `db/tools` (its checker is a script, not a suite — a harness gap, mine) and
`infra/tools`. `services/content-vault` has a module and no suite yet.

## L11. All seven invariants pass, none blocked (round 3)

```
node tools/check-invariants.mjs      # invariants: 7   pass: 7   fail: 0   partial: 0   blocked: 0
```

Two of these closed this round, and neither closed by assertion:

- **INV-1 (content crosses only on a per-event grant)** was PARTIAL with a note that a component
  should not be the only witness to the invariant it implements. It is now driven by a Lead-owned
  external suite, `services/content-vault/vaultinvariants/`, against the real service: a fabricated
  grant is refused with a closed reason; a grant for event A does not produce event B; another
  principal cannot redeem it; an expired grant is refused; a second redemption is refused; and the
  granted path reports **unavailability with a reason rather than an empty success** — an empty
  success being the failure mode a caller would read as "there was nothing to see".
- **INV-3 (the browser never speaks SQL)** was BLOCKED; the dashboard landed and the check now reads
  23 files with no SQL statement and no database driver. **INV-5** moved from a DDL text count to the
  live catalog (31 tenant-scoped tables with RLS enabled *and* forced, 31 policies; the six without
  it are all global reference data with no tenant column).

## L12. The canonicalisation contract now exists (round 3)

`device/canon` implements `sac-canon-1` step C3 (NFC) in pure Go with no external dependency, tables
generated from Node's ICU because the UCD cannot be downloaded. Its evidence is the strongest
verification pattern in this repository: **four independent equivalence checks**, not one.

- Live Node over a 3,376-case corpus, byte for byte: agree.
- All **1,112,064** code points: NFC digests agree.
- All **929,296** ordered non-starter pairs — the one table derived by observation rather than
  transcribed: agree.
- 65,536 pairs from a stride sample: agree.

And, in the pattern this round has established: **the tests can fail.** Three mutations were applied
and reverted — blocking rule removed → 3 tests fail; quick check disabled → 8 fail; combining classes
zeroed → 3 fail.

Two limits it states rather than hides: browser ICU is NOT verified (Chrome and Edge ship their own
ICU builds; the oracle script is the mechanism, but no browser is installed here), and canonically
indecomposable text — CJK, emoji, most of the SMP — is covered only as *already NFC*, because it is
NFC-invariant.

**This closes the gap the dedup tier ruling depended on.** Until now the device had no C3, so it
emitted the weak dedup key with `confidence: degraded`; that remains the correct behaviour when no
normaliser is installed, and the installer now exists.

## L13. A defect in my own acceptance harness, found by a component owner (round 3)

`tools/verify-all.mjs:231` built its Node command line as
`['--test-timeout', T, ...cmdArgs.slice(1)]`, which **dropped the `--test` flag**: the remaining paths
became `process.argv`, Node ran the first file as the entry module, and every multi-file Node package
was reported PASS after running one file. The numbers were the tell — and nobody read them:

| Package | Reported | Actually |
|---|---|---|
| `apps/capture-extension` | PASS (17 tests, 0.1s) | 193 tests, 32s |
| `apps/dashboard` | PASS (0.1s) | 100 tests |
| `services/query-api` | PASS (0.1s) | 150 tests |

443 tests were being replaced by 3, and the gate said PASS. This is the fifth harness defect this
project has produced and by far the worst, because every other one failed loudly. It was found by the
extension's owner reading his own test count against the harness output — which is the argument for
every agent being told its own numbers rather than a verdict.

Fixed by inserting the flag instead of substituting it, plus an assertion that the runner flag survived
construction, so the same class of mistake cannot be silent. Re-run after the fix: the three Node
packages execute their full suites and still pass.

## L14. Cost-model contradiction (round 3, unresolved by design)

`infra/COST-FINDING.md` records a contradiction the infrastructure checker found by recomputing
docs/05 §11 from its own unit prices, which I then verified against the document:
**§11.1's table prices Front Door's $330 base "per profile per region", while §11.2 charges it per
tenant and §11.6 calls it "40% of the tenant"**. §11.3's list of shared regional costs does not include
it. If the unit-price row is right, the base belongs in the shared allocation and the per-tenant figure
is ≈$498 at 100 tenants — back inside the $300–700 band that master §1.4 records as corrected away.

This changes the headline number of the cost model by roughly 1.7×, and the resolution is a product
decision (how much edge isolation each tenant gets), so it is recorded rather than decided. A second,
smaller finding is an unpriced line item: §11.1 says HSM-backed keys cost extra but gives no rate, while
§11.2 charges ~30 keys at about $2 in total.

## L15. Seven gates, all green — and the endpoint runs as a process (round 6)

```
node tools/accept.mjs
  [PASS] packages    16 packages, 0 fail, 0 missing, 0 timeouts
  [PASS] contract    codegen drift
  [PASS] seams       field names against the contract
  [PASS] vocab       enumerated vocabularies
  [PASS] invariants  7 rows, 0 fail, 0 blocked, 0 partial
  [PASS] endpoint    capture-core --selftest: 21 assertions, 0 failures
  [PASS] db          46 assertions on a real PostgreSQL server
  VERDICT: every gate passed. This is the acceptance run.
```

The `endpoint` gate is new and it is the one that changes what "green" means here. Every other gate
tests a component or a seam; this one builds `device/capture-core/cmd/capture-core` and runs
`--selftest`, which drives the **assembled** agent: the literal §3.5 startup and shutdown order
(loopback released first, the port free afterwards), six real extension frames through the real
framing, the native-messaging host as **two separate child processes** (one with the classifier host
up, one with it down, the second proving the rules-only fallback), the real spool, M0 carrying no
content-derived field, and every coverage row validating as `protocol.HealthReport`.

It mints its own signed bundle, opens its own spool, uses ephemeral ports and removes its own work
directory, so it is safe to run from a gate. I reproduced it independently: build exit 0, selftest
exit 0, 21 assertions.

## L16. A correction to the record, and what it is worth

I diagnosed the failing child-process check as "the fixture uses a Unix socket, which does not exist
on Windows". **That was wrong**, and the owner said so with a stack trace: the child exited 2 from a
panic in the shutdown path — a typed-nil `*loopback.Broker` assigned to `Supervisor.Loopback` passed
the `!= nil` test, the `Releaser` assertion succeeded, and `Release` dereferenced a nil receiver.
AF_UNIX works on this host and always did; the classifier WARN I saw was §3.4's degradation working
correctly, and I read it as the cause when it was a symptom.

The fix is better than the one I asked for: a typed-nil guard, per-step panic containment in the
shutdown column, and two regression tests. The lesson is the one this project keeps re-teaching — a
plausible mechanism that fits the visible output is not a diagnosis. I had the WARN line and built a
story around it; the owner had the stack trace.

## L17. Security findings this round

- **A false security finding, fixed.** The loopback broker reported `tampered` permanently after a
  port conflict ended, even though it had re-bound and was serving. `tampered` is the only state that
  raises a security finding, so every ordinary port conflict would have raised a permanent alert on a
  working device. Now `tampered` is present-tense while the conflict history is sticky, separately,
  on the coverage row — the option I asked for, with the reasoning recorded at the method.
- **A cross-component digest inconsistency, being fixed.** The classifier host's release loader
  compares digests with `strings.EqualFold` while the database column is lowercase-only by CHECK, so a
  hand-written uppercase manifest digest would be accepted by the loader and refused by the store.
  Third instance of this class in the project, after the content-encoding break and the reason-code
  vocabulary conflict.

---

## What is NOT verified at this point

- **No endpoint has ever run as an endpoint.** There is no `capture-core` service binary, no
  native-messaging host binary, no browser, and no installed root CA. `device/integration` wires the
  libraries together in one process; it starts no service, binds no port, and installs nothing. That
  is the largest single gap between this repository and a deployable product.
- **The browser half is the least-verified surface in the project.** The extension's own suite
  reports green, and the native-messaging seam is now driven from the outside by `device/integration`
  with negative controls — but `chrome.*` behaviour, native-messaging host registration, the inline
  warn/block path and attachment capture from a live page have never executed. Chromium is not
  installed. The extension owner still has `worker.test.mjs` cases open and a README outstanding.
- **`infra` is mid-flight.** Its own suite is failing on a self-check about the not-verified
  statement, which is what a component's suite is for. Until it is green, the `packages` gate cannot
  be green, and the deployment remains the one artefact nobody has attempted to render runnable.
- **`db/tools` has no suite** for the acceptance harness to run. Its checker is invoked by the
  database gate itself, so the work is checked — but it is reported MISSING by the package harness,
  and a component that cannot be run by the acceptance command is one step away from being skipped.
- **No browser, no device, no cloud.** Chromium is not installed; there is no Azure subscription, no
  cloud KMS, no real system-proxy or trust-store interaction. Four device subsystems sit behind
  interfaces with fakes: `SystemProxy`, trust-store install/remove, DPAPI/Keychain sealing, and the
  Windows named-pipe *server* side. Each is named in its component's own report and none is claimed
  as working.
- **Two of the seven collection routes had no provider** until mid-round; `proc.detect` now exists
  (9 tests) and `cli.shim` does not. One route out of seven is unimplemented, and its §3.5 ordering
  slot is covered only by a recording fake.
- **The `content` encoding seam was broken** when the verifier found it: the extension sent raw text
  where `device/protocol` declares `[]byte` (base64). Text payloads failed to decode outright, and
  text that happened to be valid base64 decoded *silently to different bytes than the user typed*
  while the digest was computed over the original — a content-identity break. The fix is in flight on
  the extension side; it is not verified here until the round-trip test runs.
- **PostgreSQL 16 is the deployment target and 17.11 is what was tested.** Every construct used has a
  minimum version ≤13, which is an argument, not a run, and no PG16 image exists on this host.
- **`go test -race` cannot run** (needs cgo; no gcc), so concurrency is covered by concurrent tests
  rather than by the race detector, in both the spool and the classifier host.
- **The NFC normaliser does not exist yet** (task-15). Until it does, C3 is unimplemented, and the
  ruled behaviour is that a device without it emits the *weak* dedup key with `confidence: degraded`
  — an honest visible undercount rather than a silent one. `services/ingest-api`'s canonicaliser is
  likewise identity-based and off the request path.
- **The database's `database/sql` plumbing is unexecuted**: no PostgreSQL wire driver exists offline,
  so only the statement text (run against the live schema) and the stored procedure's semantics are
  proven, not the Go code around them.
- **`.integration/REPORT.md` is the independent verifier's report, not the Lead's.** Where the two
  disagree, the disagreement is a finding; there is at least one such case this round (the verifier's
  account of whether `capture-core` completes, which the deadlock explains and which the owner has
  since confirmed was his bug and his fix).
