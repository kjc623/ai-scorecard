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


## L4. End-to-end acceptance harness

```
node tools/verify-all.mjs
```

Round-2 state: `device/protocol` PASS (22 tests), `device/capture-spool` PASS (11 tests),
`device/capture-core` and `device/classifier-host` compile with partially-covered packages,
`apps/capture-extension` FAIL (3 of 66 tests — in-progress work by its owner),
`services/ingest-api` FAIL (module path error, `replace => ../protocol` resolving to
`services/protocol`; reported to the owner with the one-line fix).

The harness treats a package with no test files, and a module with no packages, as `NO-TESTS` rather than
`PASS`, so "green" cannot mean "nothing ran".

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

## What is NOT verified at this point

- **No independent integration verification has run.** `.integration/REPORT.md` does not exist yet: the
  team-member cap (8) has been full since round 1, so the verifier task is created and unowned. Until it
  runs, every cross-component claim above is the Lead's own, which is exactly the conflict the verifier
  exists to remove.
- **No component has been exercised in a browser, on a real device, or against a real cloud.** Chromium is
  not installed; there is no Azure subscription; there is no cloud KMS. Anything touching those is
  unverified by construction, and each component's report is expected to say so.
- **`apps/dashboard`, `services/content-vault` and `infra` have no owner yet** — the same cap.
- **The classifier's dual-target equivalence and the canonicalisation normaliser are in flight.** Neither
  result is quoted here until it lands with raw output.
