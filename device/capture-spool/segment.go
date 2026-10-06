package spool

import (
	"fmt"
	"os"
)

// A segment is one append-only file. Frames are appended to the active segment until it
// reaches Config.SegmentBytes, then a new one is opened. Nothing in a segment is ever
// rewritten; a segment is either read or, once every record in it is terminal, unlinked.
//
// Why segments rather than one file: reclaiming space from an append-only log requires
// dropping whole files. Front-to-back reclamation also makes the counter fold exact, because
// a segment's tombstones are either entirely on disk or entirely folded into the counter
// file — never half of each.
type segment struct {
	id   uint64
	path string
	size int64

	// file is the append handle. It is non-nil only for the active segment.
	file *os.File
	// reader is a lazily opened read handle for a segment that is no longer active.
	reader *os.File

	// pending and inFlight are the undelivered records in this segment: the records that
	// keep it from being reclaimed.
	pending  int
	inFlight int

	// counters counts the tombstones physically present in this segment.
	counters segmentCounters

	// dataFrames is the number of observation frames, for reporting only.
	dataFrames int
}

func (s *segment) live() int { return s.pending + s.inFlight }

// readHandle returns a handle that can be used with ReadAt. The active segment's append
// handle is opened O_RDWR precisely so that reading a just-written record needs no second
// handle and cannot race with the write.
func (s *segment) readHandle() (*os.File, error) {
	if s.file != nil {
		return s.file, nil
	}
	if s.reader != nil {
		return s.reader, nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("spool: opening segment %s for reading: %w", s.path, err)
	}
	s.reader = f
	return f, nil
}

func (s *segment) close() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if s.reader != nil {
		_ = s.reader.Close()
		s.reader = nil
	}
}
