package spool

import (
	"errors"
	"fmt"
)

var (
	// ErrWriterActive is returned by Open when another live process holds the spool's
	// writer lock. One writer, one spool (§3.4, §12).
	ErrWriterActive = errors.New("spool: another process holds the writer lock")

	// ErrClosed is returned by any operation on a closed spool.
	ErrClosed = errors.New("spool: closed")

	// ErrPoisoned is returned once a write has failed in a way that could have left a
	// partial frame in the log and the damage could not be truncated away. The spool
	// refuses further appends rather than appending after a torn frame, which would make
	// every later frame unreadable.
	ErrPoisoned = errors.New("spool: log is poisoned by a failed write")

	// ErrCorrupt is the category of every "this frame is complete but not authentic"
	// failure: a modified segment, a wrong key, a reordered or truncated body.
	ErrCorrupt = errors.New("spool: corrupt segment")

	// ErrTornTail marks a physically incomplete trailing frame. It is not corruption: a
	// frame the writer was killed in the middle of was never a record.
	ErrTornTail = errors.New("spool: incomplete trailing frame")
)

// CorruptError is returned when a frame is complete on disk but is not the frame that was
// written. It is deliberately loud: tampering with a segment is detected on read, never
// silently accepted, and the offset and reason are reported so an operator can tell a bad
// sector from a modified file.
type CorruptError struct {
	Path   string
	Offset int64
	Reason string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("spool: corrupt segment %s at offset %d: %s", e.Path, e.Offset, e.Reason)
}

// Unwrap makes errors.Is(err, ErrCorrupt) true for every corruption.
func (e *CorruptError) Unwrap() error { return ErrCorrupt }

// EntryError is returned when an entry cannot be accepted at the door: a defect upstream is
// refused rather than spooled, because a spooled record that can never be delivered is
// indistinguishable from data loss.
type EntryError struct {
	Seq    uint64
	Reason string
}

func (e *EntryError) Error() string {
	if e.Seq != 0 {
		return fmt.Sprintf("spool: refusing entry seq %d: %s", e.Seq, e.Reason)
	}
	return "spool: refusing entry: " + e.Reason
}
