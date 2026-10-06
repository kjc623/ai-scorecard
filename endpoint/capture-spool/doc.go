// Package spool is the device's observation queue: bounded, encrypted at rest, crash-safe,
// single-writer and append-only. It implements protocol.Store, which capture-core calls.
//
// The spool is a log of segment files. Every frame is length-prefixed, CRC-checked and sealed
// with AES-256-GCM under the caller's key, with the frame header as additional data, so a
// modified byte, a swapped frame or a replayed frame fails to open. State changes (in flight,
// delivered, rejected, dropped) are appended as tombstones, never written over a record, so a
// spooled observation is never rewritten and Peek returns exactly the bytes that were appended.
//
// Invariants:
//
//  1. One writer. Open takes an operating-system lock on a file in the spool directory
//     (LockFileEx on Windows, flock elsewhere); the kernel releases it when the holder dies, so
//     there is no stale lock to detect. A second Open fails with ErrWriterActive.
//  2. One key. The caller supplies it and keeps it outside the spool directory; without it the
//     spool is ciphertext.
//  3. One place the bound is enforced. Append enforces it before it writes, evicting the oldest
//     pending records. Peek, Settle and Stats never drop anything.
//  4. A drop is counted, never silent. Every eviction appends a tombstone carrying the kind and
//     route of what was lost. DroppedTotal counts overflow drops only; retention expiry is
//     counted separately in ExtendedStats.
//  5. In flight is not delivered. A record a killed process left in flight returns to pending on
//     the next Open, and the count is reported in Recovery.
//
// Recovery reads the frames in order and truncates an incomplete trailing frame, which was a
// write killed mid-way and never a record. A complete frame that fails authentication is
// corruption: Open refuses it (CorruptFail) or quarantines the segment and reports the loss
// (CorruptQuarantine). Space is reclaimed by unlinking a segment once every record in it is
// terminal; the counts its tombstones carry are first folded into an atomically replaced counter
// file, so the totals survive the deletion exactly once.
//
// Durability: by default every frame is fsynced before Append returns. The crash tests kill a
// real child process mid-write (TerminateProcess on Windows, SIGKILL elsewhere) and assert that
// no torn record is counted as delivered, pending or dropped.
package spool
