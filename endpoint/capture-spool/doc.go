/*
Package spool implements the device-side observation queue described by
docs/01-collectors.md §12: bounded, encrypted at rest, crash-safe, single-writer and
append-only. It is the implementation of protocol.Store, the interface capture-core calls,
and the only durable store on the device.

# The SQLite deviation, stated plainly

docs/01-collectors.md §12 and ADR 0002 name SQLite in WAL mode as the device store. This
package does not use SQLite, and does not claim to. There is no SQLite driver available on
this host: the build is offline (GOPROXY=off; no module downloads), so modernc.org/sqlite
cannot be fetched, and cgo plus a C toolchain is not present either. Rather than stub a
database, the spool is implemented as a single-writer append-only segment log with
application-level AEAD encryption, and the queue-level interface it satisfies is the
swappable part.

The consequence is honest and narrow: the observable contract of §12 — bounded with
drop-oldest, crash-safe, encrypted at rest, counted drops, one writer, one key — is
implemented and tested here; the *engine* underneath it is a segment log rather than SQLite.
A SQLite-backed implementation can satisfy the same interface later without changing any
caller (see "Storage abstraction" below). What SQLite would have supplied for free, and what
this package supplies explicitly instead:

  - Transactions: one frame is one atomic unit (a length-prefixed, CRC-framed, AEAD-sealed
    record). A frame is either entirely present and authentic, or it is the torn tail of a
    killed write and is discarded. There is no partial-record state.
  - WAL replay: recovery reads the frames in order and truncates an incomplete trailing
    frame. Recovery is replay and truncation, never repair, exactly as §12.2's table says.
  - `ORDER BY seq`: a monotonic sequence assigned at append, and an in-memory order index.
  - `state = 'pending' | 'in_flight' | 'delivered' | 'rejected' | 'dropped'`: state
    transitions are appended as tombstones, never written over an existing record, so the
    log stays append-only and a delivered observation is never rewritten (ADR 0004).

# Storage abstraction

The abstraction the rest of the device depends on is protocol.Store:

	Append(Entry) (Entry, error)      Peek(n int) ([]Entry, error)
	MarkInFlight(seqs []uint64) error Settle(seq, state, reason) error
	Stats() SpoolStats                Close() error

A SQLite implementation would satisfy that interface with the table §12 already declares:

	CREATE TABLE spool_event (event_id TEXT PRIMARY KEY, ..., seq INTEGER NOT NULL,
	  envelope BLOB NOT NULL, state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
	  bytes INTEGER NOT NULL, expires_at TEXT NOT NULL);
	CREATE INDEX spool_sendable ON spool_event(state, seq);

and the mapping is one statement per method:

	Append        INSERT INTO spool_event (...) VALUES (...)             (seq from a sequence)
	Peek          SELECT ... WHERE state='pending' ORDER BY seq LIMIT n
	MarkInFlight  UPDATE spool_event SET state='in_flight', attempts=attempts+1 WHERE seq IN (...)
	Settle        UPDATE spool_event SET state=?, reason=? WHERE seq=?   (or DELETE for rejected)
	Stats         SELECT count(*) WHERE state IN ('pending','in_flight');
	              SELECT value FROM spool_counter WHERE name='dropped_total'
	drop-oldest   DELETE FROM spool_event WHERE seq IN (SELECT seq ... WHERE state='pending'
	                ORDER BY seq LIMIT k); UPDATE spool_counter SET value=value+k

Two things the segment log does that the SQL sketch above does not have to, and which are
the parts a SQLite implementation would replace rather than imitate:

  - Reclamation. Delivered and rejected records stay on disk until every record in their
    segment is terminal, at which point the whole segment is unlinked. Nothing is rewritten;
    space is recovered by dropping whole dead files, front to back (see reclaim, below).
  - The counters. `dropped_total` is monotonic and must survive the deletion that produces
    it (§12.2), so the counts of the tombstones living in a segment are folded into a small
    atomically-replaced counter file before that segment is unlinked. The counter file plus
    the tombstones still on disk is the total: a segment's tombstones are counted from
    exactly one of the two, never both, which is what makes the count exact across a crash.

# Invariants this package is built to keep

 1. One writer. Open takes an exclusive lock file in the spool directory. A second Open
    fails with ErrWriterActive; a lock left behind by a killed process is detected as stale
    by process liveness and reclaimed. Append-only with a single writer means a reader can
    never observe a half-written frame as data.
 2. One encryption key. Every frame is sealed with AES-256-GCM under a 32-byte key obtained
    from a KeyProvider. The key is never written into the spool directory; FileKeyProvider
    refuses a path that resolves inside it. Without the key the spool is ciphertext, which
    is the intended outcome when the platform wrapping material is destroyed (§12).
 3. One place the bound is enforced. Append enforces it, synchronously, before it writes
    (§12: "the bound is enforced on write, not on a timer"). Peek, Settle and Stats never
    drop anything.
 4. A drop is counted, never silent (C22). Every eviction appends a tombstone carrying the
    kind and route of what was lost, and DroppedTotal increases by exactly the number
    evicted. DroppedTotal counts *overflow* drops only; retention expiry is counted
    separately in ExtendedStats, because §12.2 requires the two failures to be distinct.
 5. Undelivered observations are never rewritten. A record's payload bytes are written once,
    inside one sealed frame, and are never re-encoded; Peek returns exactly the bytes that
    were appended.
 6. In-flight is not a delivery. A record left in_flight by a killed process returns to
    pending on the next Open, and the recovery count is reported.

# Crash model

The crash the tests exercise is a real process kill: a child process is started, writes
records, and is terminated mid-write (on Windows, TerminateProcess, which is the platform's
uncatchable kill; on POSIX, SIGKILL). Data already handed to the kernel by a completed Write
survives a process kill through the page cache; a torn frame — a frame the process was
killed in the middle of — is truncated at the next Open and is never returned by Peek, never
counted as delivered, and never counted as dropped.

Power loss is a different failure with a different guarantee: the default configuration
fsyncs every frame (Config.SyncEvery == 1), so a frame returned by Append has been flushed
before Append returns. The kill-mid-write test proves the process-kill case, not the
power-loss case; state that distinction rather than blurring it.

# Tamper model

The device is assumed hostile-adjacent: another local user may be able to read the spool
directory. Encryption is application-level (§12), so a segment file is ciphertext and
contains neither payload bytes nor their length distribution. Every frame is authenticated
with the header as additional authenticated data, so a modified byte, a swapped frame, a
truncated body or a replayed frame under a different sequence number fails to open. A
complete frame that fails authentication is reported as corruption (ErrCorrupt) and Open
fails; it is never silently skipped. Only a *physically incomplete* trailing frame — which
cannot have been a valid record — is discarded as a torn tail.
*/
package spool
