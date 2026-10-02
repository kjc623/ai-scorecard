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

**37 distinct assertions pass (T1–T37) against PostgreSQL 17.11**, applied by `db/schema.sql` and asserted
as the runtime roles rather than as a superuser — which is the whole point, since a superuser bypasses
row-level security and would prove nothing. T36/T37 are the new equal-rank adopt assertions from L2.

Two caveats this log must carry, because the runner's own exit code contradicts them:

- **The runner exits 1 on a healthy run.** Its error counter greps for the substring `ERROR`, which
  matches `ON_ERROR_STOP` in the echoed command line, giving `error_lines=2` with zero real errors. Reported
  to the owner (db-builder); until it is anchored, trust the tally, not the exit code.
- **Version substitution.** The deployment target is PostgreSQL 16 (ADR 0002); 17.11 is what is locally
  available with no network. No assertion depends on a 16-versus-17 difference as far as the suite shows,
  but that has not been independently proven — it is a substitution, not the target.

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

## L5. Seam check against the contract

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
