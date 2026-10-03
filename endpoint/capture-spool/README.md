# capture-spool — the device's only durable store

This is `protocol.Store`: the bounded, encrypted-at-rest, crash-safe observation queue of
[docs/01-collectors.md §12](../../docs/01-collectors.md). It exists because the device is offline most
of the time and observations must survive a crash, a kill and a hostile-adjacent local user.

Two failures shape the whole design: an observation lost silently (C22 forbids it — a drop is
counted, attributed and reported), and a spool that a second writer or a replayed frame can corrupt.
Everything below follows from those.

## The SQLite deviation, stated plainly

§12 and [ADR 0002](../../docs/adr/0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md)
name SQLite in WAL mode. **This package does not use SQLite and does not claim to.** No SQLite driver
is fetchable on this offline host and there is no C toolchain for cgo. The spool is a single-writer
append-only segment log with application-level AEAD per record; the swappable part is the queue-level
interface, and a SQLite implementation could satisfy `protocol.Store` later without changing a caller.

What SQLite would have supplied for free, supplied explicitly here:

| SQLite | Here |
|---|---|
| transaction per row | one frame is one atomic unit — `header(32) ‖ AES-256-GCM body ‖ crc32(4)`; whole frame or nothing |
| WAL replay | recovery reads frames in order and truncates an incomplete **trailing** frame; replay and truncation, never repair |
| `ORDER BY seq` | a monotonic sequence assigned at append, plus an in-memory order index |
| `state` column | tombstone frames appended to the log; an existing frame is never rewritten |
| `DELETE` + counter | drop and expiry tombstones; counters folded into a sealed counter file before a segment is unlinked |

## Invariants it is built to keep

1. **One writer.** `Open` takes an exclusive lock file in the spool directory; a second `Open` fails
   with `ErrWriterActive`, and a lock left by a killed process is detected as stale by process
   liveness and reclaimed.
2. **One encryption key.** Every frame is sealed AES-256-GCM under a 32-byte key from a `KeyProvider`.
   The key is never written inside the spool directory: `FileKeyProvider` refuses a path that resolves
   inside it.
3. **One place the bound is enforced.** `Append` enforces it synchronously before writing. `Peek`,
   `Settle` and `Stats` never drop anything.
4. **A drop is counted, never silent.** Every eviction appends a tombstone carrying the kind and route
   of what was lost. Overflow drops and retention expiry are counted separately, because §12.2
   requires the two failures to be distinct.
5. **Undelivered observations are never rewritten.** `Peek` returns exactly the bytes that were
   appended; the spool never parses an envelope, so it has no opinion about the contract.
6. **In-flight is not a delivery.** A record left `in_flight` by a killed process returns to `pending`
   on the next `Open`, and the recovery count is reported.

## Crash and tamper models

The crash the tests exercise is a real process kill: a child process writes records and is terminated
mid-write (Windows `TerminateProcess`, POSIX `SIGKILL`). A torn trailing frame is truncated at the
next `Open` — never returned by `Peek`, never counted as delivered, never counted as dropped. With the
default `Config.SyncEvery == 1` each frame is fsynced before `Append` returns, so a process kill and a
power loss are different guarantees, and the package states which one it proves.

Every frame is authenticated with its header as additional authenticated data, so a modified byte, a
swapped frame, a truncated body or a frame replayed under a different sequence number fails to open.
A complete frame that fails authentication is reported as `ErrCorrupt` and `Open` fails; only a
*physically incomplete* trailing frame, which cannot have been a valid record, is discarded.

## Evidence

[EVIDENCE.md](EVIDENCE.md) carries the full run. Headlines: 33 test functions plus subtests as recorded
there (the tree now has 34 `Test*` functions); a real kill-mid-write crash suite including a crash
between the counter fold and the segment unlink (the drop is counted exactly once); a bound that
evicts oldest exactly and never evicts in-flight records; and a crypto suite covering
plaintext-absence, wrong key, reordering, duplication, tamper and counter-file tamper.
`TestDrainCycleThroughTheStoreInterface` drives a full drain through the interface rather than the
concrete type, so the seam `capture-core` uses is exercised rather than asserted.

`Extended()` — the separate expiry count, drop attribution by kind and route, recovery facts and the
over-bound count — lives on the concrete type on purpose, so `protocol.Store` stays exactly as the
Lead defined it.

## What it deliberately does not do

- **No parsing, no validation, no rewriting of payloads.** The spool stores opaque bytes; anything
  that needs to understand an envelope belongs to `capture-core`.
- **No delivery.** Draining to `ingest-api` is the core's job; this package never opens a socket.
- **No key management.** `KeyProvider` is the seam where DPAPI/Keychain sealing belongs; the shipped
  file provider protects the key with filesystem ACLs only.
- **No SQLite, and no claim of it.** When a driver is available, the interface is the contract to
  satisfy — not the segment-log internals.
