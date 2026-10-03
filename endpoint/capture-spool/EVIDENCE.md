# E3 — Device Spool: evidence

Task: `task-8` (endpoint/capture-spool, docs/01-collectors.md §12, ADR 0002).
Owner: spool-dev. Write scope: `endpoint/capture-spool/` only.

Everything below was run on this host (Windows, Go 1.27.0, `GOPROXY=off`). Nothing in this
package was verified against SQLite, and nothing here claims to be.

---

## 1. The SQLite deviation, stated plainly

docs/01-collectors.md §12 and ADR 0002 name **SQLite in WAL mode** as the device store.
**This package does not use SQLite and does not claim to.** No SQLite driver was available
when this was built and run: the host had no network access then, `modernc.org/sqlite`
cannot be fetched with `GOPROXY=off` (which the builds still set), and cgo plus a C toolchain
is not present either (`go test -race` fails with `-race requires cgo`, which shows the same
absence).

The spool is therefore a **single-writer append-only segment log** with application-level
AEAD encryption per record, in the spool directory. The queue-level interface it satisfies —
`protocol.Store`, owned by `endpoint/protocol` — is the part a SQLite-backed implementation
could satisfy later without changing a single caller. §12's observable contract is
implemented and tested; the engine underneath it is not SQLite.

What SQLite would have supplied, and what this package supplies instead:

| SQLite | Here |
|---|---|
| Transaction per row | One frame is one atomic unit: `header(32) ‖ AES-256-GCM body ‖ crc32(4)`. Whole frame or nothing. |
| WAL replay | Recovery reads frames in order and truncates an incomplete **trailing** frame. Replay and truncation, never repair (§12.2). |
| `ORDER BY seq` | Monotonic entry sequence assigned at append, plus an in-order index. |
| `state` column | Tombstone frames appended to the log; an existing frame is never rewritten. |
| `DELETE` + counter | Drop/expiry tombstones; the counter is folded into a sealed counter file before a segment is unlinked. |

## 2. Files

| File | What it holds |
|---|---|
| `doc.go` | Package documentation: the deviation above, the abstraction, the invariants, the crash and tamper models. |
| `frame.go` | The frame format, AES-256-GCM open/seal, torn-tail vs corruption distinction. |
| `segment.go` | One append-only segment file: append handle, read handle, per-segment liveness and tombstone counters. |
| `entry.go` | The frame plaintext (`metaLen ‖ meta JSON ‖ payload bytes` verbatim), transitions, and the in-memory index row. |
| `spool.go` | `Open`/replay/recovery, `Append`, bound enforcement, drop-oldest, reclamation, the counter fold, the OS writer lock acquisition. |
| `ops.go` | `Peek`, `MarkInFlight`, `Settle`, `Expire`, `Stats`, `Extended`, `Close`, drop attribution. |
| `counters.go` | The sealed, atomically-replaced counter file and the fold/watermark rule. |
| `keys.go` | `KeyProvider`, in-memory and file providers, the DPAPI/Keychain seam. |
| `lock.go`, `lock_windows.go` (`LockFileEx`), `lock_flock.go`, `lock_other.go` | The single-writer lock. |
| `errors.go` | `ErrCorrupt`, `ErrTornTail`, `ErrWriterActive`, `ErrPoisoned`, `ErrClosed`, `CorruptError`, `EntryError`. |
| `*_test.go` | 34 test functions (plus subtests, and one `TestMain`) — see §5. |

## 3. The storage abstraction, and why this one

`protocol.Store` is the abstraction:

```go
Append(Entry) (Entry, error)      Peek(n int) ([]Entry, error)
MarkInFlight(seqs []uint64) error Settle(seq, state, reason) error
Stats() SpoolStats                Close() error
```

`var _ protocol.Store = (*Spool)(nil)` in `spool.go`; `TestDrainCycleThroughTheStoreInterface`
drives a full drain through the *interface*, not the concrete type, so the seam is exercised
rather than asserted.

Why this shape:

- **It is the interface the consumer already declared.** `capture-core` calls `protocol.Store`
  (task-7). Adding a second, spool-owned interface would have been a second source of truth
  for the same queue — exactly what ADR 0010 exists to prevent one layer up.
- **Every method maps to one statement over §12's `spool_event` table**, so a SQLite
  implementation is a swap, not a redesign: `Append`→`INSERT`, `Peek`→
  `SELECT … WHERE state='pending' ORDER BY seq LIMIT n`, `MarkInFlight`→`UPDATE … SET
  state='in_flight', attempts=attempts+1`, `Settle`→`UPDATE … SET state=?`, `Stats`→
  `SELECT count(*)` + `SELECT value FROM spool_counter`, drop-oldest→`DELETE … ORDER BY seq
  LIMIT k` plus the counter update.
- **`Extended()` is additive, not part of the seam.** The health report needs more than
  `protocol.SpoolStats` carries (a separate expiry count, drop attribution by kind and route,
  recovery facts, the over-bound count). Those live on the concrete type so the protocol
  interface stays as the Lead defined it.
- **No `Update` and no iterator.** `Append` is the only way in; the payload is never parsed.
  Confirmed mechanically: every use of `Payload` in non-test source is a copy, a length, or a
  slice — never a decode (`payload-uses.txt`).

What is deliberately *not* abstracted: the segment file layout and the counter fold. They are
the implementation, they are documented in `doc.go`, and abstracting them would have produced
an interface with exactly one implementation and no second caller.

## 4. Invariants, and where each is enforced

| Invariant | Enforced in | Test |
|---|---|---|
| One writer | OS lock (`LockFileEx` / `flock`), released by the kernel when the holder dies | `TestSecondOpenInTheSameProcessIsRefused`, `TestKilledHolderReleasesTheWriterLock` |
| One encryption key, never in the spool dir | `KeyProvider`; `FileKeyProvider` refuses a path inside the spool dir | `TestSpoolDirectoryHoldsNoPlaintextAndNoKey`, `TestFileKeyProviderRefusesAKeyInsideTheSpool` |
| One place the bound is enforced | `Append` → `enforceBoundLocked`, before the write; `Peek`/`Settle`/`Stats` never drop | `TestBoundEvictsOldestExactlyAndCounts` |
| A drop is counted, never silent | Tombstone per eviction; `DroppedTotal` monotonic | `TestBoundEvictsOldestExactlyAndCounts`, `TestDropCounterIsMonotonicAcrossReopenAndReclamation` |
| In-flight/delivered are never evicted | `collectVictims` selects pending only | `TestBoundNeverEvictsInFlightRecords` |
| Observation immutable, append-only | frame written once; recovery truncates, never repairs | `TestDeliveredRecordsAreNeverRewritten`, `TestPayloadIsStoredAndReturnedByteForByte` |
| Encryption at rest | AES-256-GCM, header as AAD, payload and metadata sealed (counter file too) | `TestSpoolDirectoryHoldsNoPlaintextAndNoKey` |
| Tamper is detected, not accepted | AEAD open failure → `CorruptError`; a complete frame is never silently discarded | `TestTamperedSegmentIsRefused`, `TestTamperedTailFrameWithFixedChecksumIsStillRefused`, `TestWrongKeyFailsLoudly`, `TestReorderedAndDuplicatedFramesAreDetected` |
| Crash safety | replay + torn-tail truncation; in-flight→pending on open | §6 below |
| Counted separately: expiry vs overflow | `ExpiredTotal` ≠ `DroppedTotal` | `TestExpireIsCountedSeparatelyFromDrops` |

## 5. Exact commands and raw output

Build prefix (execution policy blocks dot-sourcing `.tools\env.ps1`), run in
`endpoint/capture-spool`:

```powershell
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
```

```
=== gofmt -l . ===
=== go build ./... ===
build exit=0
=== go vet ./... ===
vet exit=0
=== GOOS=darwin go build ./... ===
darwin exit=0
=== GOOS=linux go build ./... ===
linux exit=0
```

(`gofmt -l .` printed nothing: no file needs formatting. The macOS and Linux builds cover
`lock_flock.go`, which cannot be executed on this host. Raw output of the block above:
`build-output.txt`.)

```
> go test -v -count=1 ./...
```

Full raw output: `test-output.txt` (99 lines, reproduced below). The suite was also run twice
in one process (`go test -count=2 ./...` → `ok … 4.752s`) to check that the five
killed-process tests are not timing-flaky.

```
=== RUN   TestBoundEvictsOldestExactlyAndCounts
--- PASS: TestBoundEvictsOldestExactlyAndCounts (0.01s)
=== RUN   TestBoundNeverEvictsInFlightRecords
--- PASS: TestBoundNeverEvictsInFlightRecords (0.01s)
=== RUN   TestOverBoundIsCountedNotSilent
--- PASS: TestOverBoundIsCountedNotSilent (0.01s)
=== RUN   TestDropCounterIsMonotonicAcrossReopenAndReclamation
--- PASS: TestDropCounterIsMonotonicAcrossReopenAndReclamation (0.78s)
=== RUN   TestByteBoundIsEnforcedOnWrite
--- PASS: TestByteBoundIsEnforcedOnWrite (0.48s)
=== RUN   TestConcurrentCallersAreSerialised
--- PASS: TestConcurrentCallersAreSerialised (0.04s)
=== RUN   TestCrashKillMidWriteLeavesNoTornRecord
=== RUN   TestCrashKillMidWriteLeavesNoTornRecord/half
=== RUN   TestCrashKillMidWriteLeavesNoTornRecord/header
--- PASS: TestCrashKillMidWriteLeavesNoTornRecord (0.24s)
    --- PASS: TestCrashKillMidWriteLeavesNoTornRecord/half (0.18s)
    --- PASS: TestCrashKillMidWriteLeavesNoTornRecord/header (0.06s)
=== RUN   TestCrashBetweenTheCounterFoldAndTheUnlink
    crash_test.go:373: the child reported at the crash point: appended=7|reported=4|depth=3|pending=3|inflight=0|1:sz1966:live0:drop1,2:sz1866:live3:drop3
--- PASS: TestCrashBetweenTheCounterFoldAndTheUnlink (0.06s)
=== RUN   TestCrashAfterMarkInFlightReturnsRecordsToPending
--- PASS: TestCrashAfterMarkInFlightReturnsRecordsToPending (0.04s)
=== RUN   TestCrashWithTheTombstoneWrittenCountsTheDropExactlyOnce
--- PASS: TestCrashWithTheTombstoneWrittenCountsTheDropExactlyOnce (0.05s)
=== RUN   TestSpoolDirectoryHoldsNoPlaintextAndNoKey
--- PASS: TestSpoolDirectoryHoldsNoPlaintextAndNoKey (0.02s)
=== RUN   TestFileKeyProviderRefusesAKeyInsideTheSpool
--- PASS: TestFileKeyProviderRefusesAKeyInsideTheSpool (0.00s)
=== RUN   TestTamperedSegmentIsRefused
--- PASS: TestTamperedSegmentIsRefused (0.05s)
=== RUN   TestTamperedTailFrameWithFixedChecksumIsStillRefused
--- PASS: TestTamperedTailFrameWithFixedChecksumIsStillRefused (0.02s)
=== RUN   TestWrongKeyFailsLoudly
--- PASS: TestWrongKeyFailsLoudly (0.01s)
=== RUN   TestReorderedAndDuplicatedFramesAreDetected
=== RUN   TestReorderedAndDuplicatedFramesAreDetected/reordered
=== RUN   TestReorderedAndDuplicatedFramesAreDetected/duplicated
--- PASS: TestReorderedAndDuplicatedFramesAreDetected (0.05s)
    --- PASS: TestReorderedAndDuplicatedFramesAreDetected/reordered (0.03s)
    --- PASS: TestReorderedAndDuplicatedFramesAreDetected/duplicated (0.02s)
=== RUN   TestIncompleteTailIsDiscardedNotCounted
--- PASS: TestIncompleteTailIsDiscardedNotCounted (0.04s)
=== RUN   TestCounterFileIsSealedAndTamperIsDetected
--- PASS: TestCounterFileIsSealedAndTamperIsDetected (0.03s)
=== RUN   TestSecondOpenInTheSameProcessIsRefused
--- PASS: TestSecondOpenInTheSameProcessIsRefused (0.02s)
=== RUN   TestKilledHolderReleasesTheWriterLock
--- PASS: TestKilledHolderReleasesTheWriterLock (0.04s)
=== RUN   TestOpenRequiresADirectoryAndAKey
--- PASS: TestOpenRequiresADirectoryAndAKey (0.00s)
=== RUN   TestAppendAssignsSequenceAndPeekReturnsIt
--- PASS: TestAppendAssignsSequenceAndPeekReturnsIt (0.01s)
=== RUN   TestPayloadIsStoredAndReturnedByteForByte
--- PASS: TestPayloadIsStoredAndReturnedByteForByte (0.02s)
=== RUN   TestAppendRefusesDefectsAtTheDoor
=== RUN   TestAppendRefusesDefectsAtTheDoor/no_payload
=== RUN   TestAppendRefusesDefectsAtTheDoor/kind_outside_the_registry
=== RUN   TestAppendRefusesDefectsAtTheDoor/mode_outside_the_closed_set
=== RUN   TestAppendRefusesDefectsAtTheDoor/no_dedup_key
=== RUN   TestAppendRefusesDefectsAtTheDoor/already_in_flight
=== RUN   TestAppendRefusesDefectsAtTheDoor/already_delivered
--- PASS: TestAppendRefusesDefectsAtTheDoor (0.01s)
    --- PASS: TestAppendRefusesDefectsAtTheDoor/no_payload (0.00s)
    --- PASS: TestAppendRefusesDefectsAtTheDoor/kind_outside_the_registry (0.00s)
    --- PASS: TestAppendRefusesDefectsAtTheDoor/mode_outside_the_closed_set (0.00s)
    --- PASS: TestAppendRefusesDefectsAtTheDoor/no_dedup_key (0.00s)
    --- PASS: TestAppendRefusesDefectsAtTheDoor/already_in_flight (0.00s)
    --- PASS: TestAppendRefusesDefectsAtTheDoor/already_delivered (0.00s)
=== RUN   TestEmptyStateDefaultsToPending
--- PASS: TestEmptyStateDefaultsToPending (0.01s)
=== RUN   TestMarkInFlightIncrementsAttemptsAndIsIdempotent
--- PASS: TestMarkInFlightIncrementsAttemptsAndIsIdempotent (0.01s)
=== RUN   TestSettleFollowsProtocolOutcomeMapping
=== RUN   TestSettleFollowsProtocolOutcomeMapping/accepted
=== RUN   TestSettleFollowsProtocolOutcomeMapping/duplicate
=== RUN   TestSettleFollowsProtocolOutcomeMapping/rejected_terminal
=== RUN   TestSettleFollowsProtocolOutcomeMapping/rejected_retryable
--- PASS: TestSettleFollowsProtocolOutcomeMapping (0.02s)
    --- PASS: TestSettleFollowsProtocolOutcomeMapping/accepted (0.01s)
    --- PASS: TestSettleFollowsProtocolOutcomeMapping/duplicate (0.01s)
    --- PASS: TestSettleFollowsProtocolOutcomeMapping/rejected_terminal (0.01s)
    --- PASS: TestSettleFollowsProtocolOutcomeMapping/rejected_retryable (0.01s)
=== RUN   TestSettleRejectedRequiresAClosedReasonCode
--- PASS: TestSettleRejectedRequiresAClosedReasonCode (0.01s)
=== RUN   TestReopenKeepsSequenceOrderAndDepth
--- PASS: TestReopenKeepsSequenceOrderAndDepth (0.02s)
=== RUN   TestExpireIsCountedSeparatelyFromDrops
--- PASS: TestExpireIsCountedSeparatelyFromDrops (0.01s)
=== RUN   TestDeliveredRecordsAreNeverRewritten
--- PASS: TestDeliveredRecordsAreNeverRewritten (0.02s)
=== RUN   TestDrainCycleThroughTheStoreInterface
--- PASS: TestDrainCycleThroughTheStoreInterface (0.01s)
=== RUN   TestStatsExposeDepthAndDropCounter
--- PASS: TestStatsExposeDepthAndDropCounter (0.01s)
=== RUN   TestCloseIsIdempotentAndOperationsAfterCloseFail
--- PASS: TestCloseIsIdempotentAndOperationsAfterCloseFail (0.01s)
PASS
ok  	github.com/shadow-ai-capture/device/capture-spool	2.355s
```

## 6. The kill-mid-write crash test, and how the kill is real

`TestCrashKillMidWriteLeavesNoTornRecord` (subtests `half` and `header`):

1. The test binary is re-executed as a **real child process** (`os/exec`, env
   `CAPTURE_SPOOL_CHILD_*`); `TestMain` intercepts before the test framework starts.
2. The child opens the spool, appends 5 complete records, then installs a write
   interposition that writes **part** of the next frame (half of it, or 8 bytes of its
   header), writes a marker file, and blocks forever.
3. The parent waits for the marker and kills the process, then reopens the spool.

A signal cannot be delivered into another process at a chosen instruction, so the child is
told where to stop and is then killed there. On a real device this is exactly a service that
dies mid-`write`.

Platform honesty: **on Windows there is no SIGKILL.** `Process.Kill()` is
`TerminateProcess`, the platform's uncatchable termination — no deferred function runs, no
buffer is flushed. On POSIX the same code path is `SIGKILL`. The test asserts the child did
*not* exit through its own error path (exit code 2), so a child that failed on its own can
never be mistaken for a killed one.

What the test asserts after reopening:

- `Recovery.TornBytes > 0` — a torn frame really was on disk; the scenario is not vacuous.
- `Depth == 5` and `Peek` returns the 5 complete records with their exact payloads, and not
  the record whose write was interrupted.
- `DroppedTotal == 0`, `DeliveredTotal == 0`, `RejectedTotal == 0` — the torn record was
  counted as nothing at all.
- A subsequent `Append` succeeds, and a second reopen shows `TornBytes == 0` and depth 6:
  recovery **truncated** the torn tail rather than leaving it for the next append to follow.

Four further killed-process tests, all passing:

- `TestCrashAfterMarkInFlightReturnsRecordsToPending` — killed after `MarkInFlight`; on
  reopen `Recovery.InFlightResetToPending == 5`, every record is pending again with
  `Attempts == 1`, and it can be handed to a delivery attempt again.
- `TestCrashWithTheTombstoneWrittenCountsTheDropExactlyOnce` — killed with the whole drop
  tombstone written but not applied; the drop is counted once on reopen, and not again on the
  second open (§12.2's "crash mid-drop" row).
- `TestCrashBetweenTheCounterFoldAndTheUnlink` — killed inside the only window where the
  counter design could double count (see §7).
- `TestKilledHolderReleasesTheWriterLock` — killed while holding the writer lock; the parent
  opens the same spool immediately afterwards, with no stale-lock handling, because the OS
  released the lock when the process died.

## 7. A real defect the crash test found, and the regression test for it

`TestCrashBetweenTheCounterFoldAndTheUnlink` kills a process **between the counter fold and
the segment unlink**. The counter file is replaced first and the segment unlinked second, so
a crash in between leaves a segment on disk whose id is at or below the watermark. The first
version of the counter total summed *every* segment still on disk, which counted that
segment's tombstones twice.

The test was strengthened twice after it failed to detect the bug:

1. An early version compared the spool's own before/after numbers — which are inflated by the
   same bug, so it agreed with itself. It was replaced with an **independent ground truth**:
   the child reports how many records it appended and how many it still holds, so the drop
   count is `appended − pending` and comes from arithmetic rather than from the spool.
2. An early segment layout gave the folded segment zero tombstones, so a double count of zero
   is invisible. The scenario now uses a bound of 4 and 2 KiB segments, so the folded segment
   genuinely carries a tombstone.

With the fix removed, the test fails with:

```
--- FAIL: TestCrashBetweenTheCounterFoldAndTheUnlink
    DroppedTotal = 5 after reopening, want 4 (7 appended - 3 still held): the counters of a
    folded-but-not-unlinked segment were counted twice
```

With the fix in place (segments at or below the watermark are excluded from the live sum,
because their counters are already in the file), it passes. That toggle was run in both
directions on this host.

## 8. What is NOT verified

- **SQLite.** Not used, not available at the time of this run, not tested. §1.
- **Power loss.** The crash tests kill a process. Data already handed to the kernel survives a
  process kill through the page cache; surviving a power cut needs the `fsync` the default
  configuration issues per frame (`Config.SyncEvery: 1`) — that is implemented and reviewed
  but **not** tested, because this host cannot cut power mid-write. A power-loss test needs a
  VM with disk-write fault injection.
- **DPAPI / Keychain wrapping.** §12's ASSUMPTION A1 is a seam (`FuncKeyProvider`), not an
  implementation: no DPAPI or Keychain call is shipped, because neither can be exercised on
  this host and shipping an untested platform call while reporting
  `EncryptionKeySealed: true` would be a claim rather than a fact. `Sealed()` reports what is
  true, so a device using the file or memory provider honestly reports `false`.
- **Real disk-full, permission-denied and read-only-volume behaviour.** A failed write is
  handled (truncate the partial frame, otherwise poison the log and refuse further appends)
  and that path is unit-testable, but it was not exercised against a genuinely full volume.
- **The race detector.** `go test -race` cannot run: `-race requires cgo`, and there is no C
  toolchain on this host. `TestConcurrentCallersAreSerialised` (8 writers + 4 readers
  concurrently, exact counts asserted) is the closest available check, not a substitute.
- **macOS and Linux execution.** Both targets compile (`GOOS=darwin`, `GOOS=linux`), but only
  the Windows `LockFileEx` path was executed. `lock_flock.go` is reviewed, not run.
- **Long-run behaviour at the 25 MB default bound.** The bound is exercised at small
  thresholds (4 KB, 4 records, 200+ appends); the default is a configuration value, not a
  measured load.
