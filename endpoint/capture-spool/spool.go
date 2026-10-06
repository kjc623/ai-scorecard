package spool

import (
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

const (
	// DefaultSegmentBytes is the roll size of one segment file. It is small enough that
	// reclamation frees space in useful units and large enough that a day of observations
	// (an order of 10 KB) never rolls a segment.
	DefaultSegmentBytes = 1 << 20

	segmentsDirName        = "segments"
	quarantineSuffix       = ".bad"
	countersFileName       = "counters.json"
	segmentSuffix          = ".seg"
	maxTransitionsPerFrame = 512
	tombstoneEstimate      = 128
)

// DefaultBounds is ~25 MB or ~25,000 rows, whichever comes first: two to three orders of
// magnitude above expected single-day volume, so an ordinary outage never approaches it.
func DefaultBounds() Bounds {
	return Bounds{MaxBytes: 25 << 20, MaxEntries: 25000}
}

// Bounds is the spool bound. Both are enforced on write, and either may be zero to
// mean "no bound of this kind".
type Bounds struct {
	MaxBytes   int64 // on-disk bytes retained by the spool
	MaxEntries int   // undelivered records retained
}

// CorruptPolicy is what Open does when a complete frame fails authentication, or when the
// counter file cannot be parsed.
type CorruptPolicy int

const (
	// CorruptFail (default) refuses to open. Tampering is loud.
	CorruptFail CorruptPolicy = iota
	// CorruptQuarantine renames the damaged file aside, continues with what is readable,
	// and reports the loss in Recovery. It is for the operational path: an unreadable spool is
	// reported as data loss with a count, without stopping collection and without starting
	// clean silently.
	CorruptQuarantine
)

// Config configures a spool.
type Config struct {
	// Dir is the spool directory. It is owned by the spool: the spool is the single writer
	// of every file in it.
	Dir string

	// Key is the 32-byte AES-256 spool key. The caller keeps it outside the spool directory:
	// a key beside its ciphertext is not encryption at rest.
	Key []byte

	// Bounds defaults to DefaultBounds when both fields are zero.
	Bounds Bounds

	// SegmentBytes defaults to DefaultSegmentBytes.
	SegmentBytes int64

	// SyncEvery controls fsync. 0 means the default of 1 (every frame is flushed before
	// Append or MarkInFlight returns, which is what makes a returned write durable);
	// a negative value disables fsync and is for tests only, where the crash under test is
	// a process kill rather than a power loss.
	SyncEvery int

	// OnCorrupt defaults to CorruptFail.
	OnCorrupt CorruptPolicy

	// Now is the clock, injected for tests. It defaults to time.Now.
	Now func() time.Time
}

// Recovery reports what Open had to do to make the log readable: what it truncated, what it
// quarantined, and how many in-flight records went back to pending. It is data-loss
// reporting, and it is deliberately explicit: an unreadable spool is visible with a count,
// never silent.
type Recovery struct {
	// TornBytes is how many bytes of an incomplete trailing frame were discarded. A torn
	// frame was never a record, so this is not data loss; it is the write that was killed.
	TornBytes    int64
	TornSegments int

	// CorruptSegments counts segments that were complete but not authentic. Their exact
	// event count is not knowable from the bytes that remain, which is why the spool
	// reports the segment and the byte count and does not invent a number: see
	// LostEventsKnown.
	CorruptSegments int
	CorruptBytes    int64
	Quarantined     []string

	// InFlightResetToPending counts records that a kill left in flight. In-flight is not a
	// delivery, so they are delivered again.
	InFlightResetToPending int

	// OrphanTransitions counts tombstones whose record had already been reclaimed. They are
	// still counted — a tombstone carries its own counter effect — so this is a diagnostic,
	// not a loss.
	OrphanTransitions uint64

	// CountersReset is set when the counter file itself was unreadable and had to be
	// quarantined. The totals then start from zero, which is visible data loss of the
	// counters (never of the queue).
	CountersReset bool

	// LostEventsKnown is false when a corrupt segment makes the number of lost events
	// unknowable. The spool reports bytes and the segment path instead of a fabricated
	// count.
	LostEventsKnown bool
}

// Spool is the device spool. It implements protocol.Store.
type Spool struct {
	cfg  Config
	aead cipher.AEAD

	lock       *writerLock
	segsDir    string
	countersIn string

	mu       sync.Mutex
	closed   bool
	poisoned error

	segments []*segment // ascending id
	active   *segment   // the last element of segments, or nil before the first roll

	entries map[uint64]*entryMeta
	order   []uint64 // entry sequence numbers, ascending

	pending  int
	inFlight int
	terminal int // delivered + rejected + dropped, retained until their segment is reclaimed

	diskBytes int64
	overBound uint64

	nextFrameSeq   uint64
	nextEntrySeq   uint64
	nextSegmentSeq uint64

	persisted      countersFile
	foldedDrops    map[attrKey]uint64
	foldedExpiries map[attrKey]uint64

	framesSinceSync int

	recovery Recovery
}

// compile-time proof that the spool satisfies the interface capture-core calls.
var _ protocol.Store = (*Spool)(nil)

// Open recovers the spool in cfg.Dir and takes the single writer lock.
//
// Recovery reads every frame in order, discards an incomplete trailing frame, and returns
// every record a previous process left in flight to pending. It never rewrites a record and
// never repairs a frame: recovery is replay and truncation.
func Open(cfg Config) (*Spool, error) {
	if cfg.Dir == "" {
		return nil, errors.New("spool: Config.Dir is required")
	}
	if len(cfg.Key) != KeySize {
		return nil, fmt.Errorf("spool: Config.Key must be %d bytes, got %d", KeySize, len(cfg.Key))
	}
	if cfg.Bounds.MaxBytes == 0 && cfg.Bounds.MaxEntries == 0 {
		cfg.Bounds = DefaultBounds()
	}
	if cfg.SegmentBytes <= 0 {
		cfg.SegmentBytes = DefaultSegmentBytes
	}
	if cfg.SyncEvery == 0 {
		cfg.SyncEvery = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	s := &Spool{
		cfg:            cfg,
		entries:        map[uint64]*entryMeta{},
		foldedDrops:    map[attrKey]uint64{},
		foldedExpiries: map[attrKey]uint64{},
	}
	s.segsDir = filepath.Join(cfg.Dir, segmentsDirName)
	s.countersIn = filepath.Join(cfg.Dir, countersFileName)

	if err := os.MkdirAll(s.segsDir, 0o700); err != nil {
		return nil, fmt.Errorf("spool: creating %s: %w", s.segsDir, err)
	}

	lock, err := acquireWriterLock(filepath.Join(cfg.Dir, lockFileName))
	if err != nil {
		return nil, err
	}
	s.lock = lock

	aead, err := newAEAD(cfg.Key)
	if err != nil {
		s.lock.release()
		return nil, err
	}
	s.aead = aead

	counters, err := loadCounters(s.countersIn, aead)
	if err != nil {
		if !errors.Is(err, ErrCorrupt) {
			s.lock.release()
			return nil, err
		}
		if cfg.OnCorrupt != CorruptQuarantine {
			s.lock.release()
			return nil, fmt.Errorf("%w (set Config.OnCorrupt = CorruptQuarantine to quarantine the counter file and continue, reporting the loss)", err)
		}
		if _, qerr := quarantine(s.countersIn); qerr != nil {
			s.lock.release()
			return nil, qerr
		}
		s.recovery.CountersReset = true
		s.recovery.LostEventsKnown = false
		counters = countersFile{Version: recordVersion}
	}
	s.persisted = counters
	s.foldedDrops = attrMap(counters.Drops)
	s.foldedExpiries = attrMap(counters.Expiries)
	s.nextFrameSeq = counters.NextFrameSeq
	s.nextEntrySeq = counters.NextEntrySeq
	s.nextSegmentSeq = counters.NextSegmentSeq
	s.recovery.CorruptSegments = counters.CorruptSegments
	s.recovery.CorruptBytes = counters.CorruptBytes
	s.recovery.CountersReset = s.recovery.CountersReset || counters.CountersReset
	if s.recovery.CorruptSegments == 0 {
		s.recovery.LostEventsKnown = true
	}

	if err := s.replay(); err != nil {
		s.lock.release()
		return nil, err
	}

	// In-flight is not a delivery. A record a killed process left in flight goes back to
	// pending and is sent again.
	for _, seq := range s.order {
		m := s.entries[seq]
		if m != nil && m.state == protocol.SpoolInFlight {
			s.setState(m, protocol.SpoolPending)
			s.recovery.InFlightResetToPending++
		}
	}

	if err := s.openActive(); err != nil {
		s.lock.release()
		return nil, err
	}
	if err := s.saveCounters(); err != nil {
		s.lock.release()
		return nil, err
	}
	return s, nil
}

// replay walks every segment in id order. It is the whole of crash recovery.
func (s *Spool) replay() error {
	names, err := os.ReadDir(s.segsDir)
	if err != nil {
		return fmt.Errorf("spool: listing %s: %w", s.segsDir, err)
	}
	var ids []uint64
	maxQuarantined := uint64(0)
	for _, e := range names {
		name := e.Name()
		if id, ok := parseSegmentName(name); ok {
			ids = append(ids, id)
			continue
		}
		if id, ok := parseQuarantinedName(name); ok && id > maxQuarantined {
			maxQuarantined = id
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var lastFrameSeq uint64
	first := true
	for i, id := range ids {
		path := filepath.Join(s.segsDir, segmentName(id))
		seg := &segment{id: id, path: path, counters: newSegmentCounters()}
		last := i == len(ids)-1
		outcome, corruptBytes, err := s.replaySegment(seg, path, last, &lastFrameSeq, &first)
		if err != nil {
			return err
		}
		if outcome == segQuarantine {
			// Quarantine runs here, after replaySegment has closed its handle: Windows
			// refuses to rename a file that is still open.
			dest, err := quarantine(path)
			if err != nil {
				return err
			}
			s.recovery.CorruptSegments++
			s.recovery.CorruptBytes += corruptBytes
			s.recovery.Quarantined = append(s.recovery.Quarantined, dest)
			s.recovery.LostEventsKnown = false
			continue
		}
		if id >= s.nextSegmentSeq {
			s.nextSegmentSeq = id + 1
		}
		s.segments = append(s.segments, seg)
		s.diskBytes += seg.size
	}
	if maxQuarantined >= s.nextSegmentSeq {
		s.nextSegmentSeq = maxQuarantined + 1
	}
	return nil
}

// segmentOutcome is what replay decided about one segment.
type segmentOutcome int

const (
	segKeep segmentOutcome = iota
	// segQuarantine means the segment was damaged and Config.OnCorrupt asked for it to be
	// set aside. The rename happens after this function has closed its handle.
	segQuarantine
)

// replaySegment reads one segment. Recovery is replay and truncation, never repair.
func (s *Spool) replaySegment(seg *segment, path string, isTail bool, lastFrameSeq *uint64, first *bool) (segmentOutcome, int64, error) {
	// O_RDWR because a torn tail is truncated away, and a read-only handle cannot truncate
	// on Windows.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return segKeep, 0, fmt.Errorf("spool: opening segment %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return segKeep, 0, fmt.Errorf("spool: stating segment %s: %w", path, err)
	}
	size := fi.Size()

	off := int64(0)
	torn := false
	for {
		h, pt, next, err := readFrameAt(f, path, off, s.aead)
		switch {
		case errors.Is(err, errCleanEnd):
		case errors.Is(err, ErrTornTail):
			// A physically incomplete trailing frame is what a killed write leaves. It
			// cannot be a record, so it is discarded — but only at the very tail of the
			// log. An incomplete frame anywhere else means a segment was damaged.
			if !isTail {
				return segKeep, 0, &CorruptError{Path: path, Offset: off, Reason: "incomplete trailing frame in a segment that is not the tail of the log"}
			}
			torn = true
		case err != nil:
			if s.cfg.OnCorrupt == CorruptQuarantine && errors.Is(err, ErrCorrupt) {
				// The damaged segment is set aside, not deleted, and the loss is reported.
				// The rename itself happens in the caller, once this handle is closed.
				return segQuarantine, size, nil
			}
			return segKeep, 0, err
		default:
			if !*first && h.Seq <= *lastFrameSeq {
				// Frames are sequence-numbered and the number is authenticated. A frame
				// that is out of order is a reordered, duplicated or replayed frame.
				return segKeep, 0, &CorruptError{Path: path, Offset: off, Reason: fmt.Sprintf("frame sequence %d does not follow %d", h.Seq, *lastFrameSeq)}
			}
			*first = false
			*lastFrameSeq = h.Seq
			if h.Seq >= s.nextFrameSeq {
				s.nextFrameSeq = h.Seq + 1
			}
			frameLen := int64(len(pt)) + frameHeaderSize + frameTrailerSize
			switch h.Type {
			case frameData:
				if err := s.applyDataFrame(seg, off, frameLen, pt); err != nil {
					return segKeep, 0, err
				}
			case frameControl:
				if err := s.applyControl(seg, pt); err != nil {
					return segKeep, 0, err
				}
			default:
				// A counter frame belongs in the counter file, not in the log.
				return segKeep, 0, &CorruptError{Path: path, Offset: off, Reason: "counter frame inside a segment"}
			}
		}
		if errors.Is(err, errCleanEnd) || errors.Is(err, ErrTornTail) {
			break
		}
		off = next
	}

	if torn || off < size {
		// Truncate the incomplete tail so the next append does not follow a partial frame.
		if err := f.Truncate(off); err != nil {
			return segKeep, 0, fmt.Errorf("spool: truncating the incomplete tail of %s: %w", path, err)
		}
		_ = f.Sync()
		s.recovery.TornBytes += size - off
		s.recovery.TornSegments++
	}
	seg.size = off
	return segKeep, 0, nil
}

func (s *Spool) applyDataFrame(seg *segment, off, frameLen int64, pt []byte) error {
	meta, payload, err := decodeDataFrame(pt)
	if err != nil {
		return &CorruptError{Path: seg.path, Offset: off, Reason: err.Error()}
	}
	if _, dup := s.entries[meta.Seq]; dup {
		return &CorruptError{Path: seg.path, Offset: off, Reason: fmt.Sprintf("duplicate spool sequence %d", meta.Seq)}
	}
	if n := len(s.order); n > 0 && meta.Seq <= s.order[n-1] {
		return &CorruptError{Path: seg.path, Offset: off, Reason: fmt.Sprintf("spool sequence %d does not follow %d", meta.Seq, s.order[n-1])}
	}
	m := &entryMeta{
		seq:             meta.Seq,
		state:           protocol.SpoolPending,
		attempts:        meta.Attempts,
		sizeBytes:       meta.SizeBytes,
		kind:            protocol.Kind(meta.Kind),
		route:           protocol.Route(meta.Route),
		collectionMode:  protocol.CollectionMode(meta.CollectionMode),
		toolFingerprint: meta.ToolFingerprint,
		clientID:        meta.ClientID,
		dedupKey:        meta.DedupKey,
		occurredAt:      timeFromUnixNano(meta.OccurredAt),
		monotonicMS:     meta.MonotonicOffsetMS,
		expiresAt:       timeFromUnixNano(meta.ExpiresAt),
		appendedAt:      timeFromUnixNano(meta.AppendedAt),
		segID:           seg.id,
		off:             off,
		frameLen:        frameLen,
	}
	_ = payload // the payload is read back on demand; the index holds no content
	s.entries[m.seq] = m
	s.order = append(s.order, m.seq)
	s.pending++
	seg.pending++
	seg.dataFrames++
	if m.seq >= s.nextEntrySeq {
		s.nextEntrySeq = m.seq + 1
	}
	return nil
}

// applyControl applies a batch of tombstones. The counter effect of a tombstone is applied
// from the tombstone alone, never inferred from the record's state, so a tombstone whose
// record was already reclaimed is still counted exactly once.
func (s *Spool) applyControl(seg *segment, pt []byte) error {
	ctl, err := decodeControlFrame(pt)
	if err != nil {
		return &CorruptError{Path: seg.path, Offset: 0, Reason: err.Error()}
	}
	for _, t := range ctl.Transitions {
		if err := s.applyTransition(seg, t); err != nil {
			return err
		}
	}
	return nil
}

func (s *Spool) applyTransition(seg *segment, t transition) error {
	switch t.Op {
	case opClaim:
		m := s.lookup(t.Seq)
		if m == nil {
			s.recovery.OrphanTransitions++
			return nil
		}
		if m.state == protocol.SpoolPending {
			s.setState(m, protocol.SpoolInFlight)
		}
		m.attempts = t.Attempts
	case opRelease:
		m := s.lookup(t.Seq)
		if m == nil {
			s.recovery.OrphanTransitions++
			return nil
		}
		if m.state == protocol.SpoolInFlight {
			s.setState(m, protocol.SpoolPending)
		}
		m.lastError = t.LastError
	case opSettle:
		state := protocol.SpoolState(t.State)
		switch state {
		case protocol.SpoolDelivered:
			seg.counters.delivered++
		case protocol.SpoolRejected:
			seg.counters.rejected++
		case protocol.SpoolPending:
			// a retryable rejection returns the record to the queue; it is not a settlement
		default:
			return &CorruptError{Path: seg.path, Reason: fmt.Sprintf("settle transition carries unknown state %q", t.State)}
		}
		m := s.lookup(t.Seq)
		if m == nil {
			s.recovery.OrphanTransitions++
			return nil
		}
		s.setState(m, state)
		m.rejectReason = t.Reason
		m.lastError = t.LastError
	case opDrop:
		if t.Cause != causeBound {
			return &CorruptError{Path: seg.path, Reason: fmt.Sprintf("drop transition carries unknown cause %q", t.Cause)}
		}
		seg.counters.dropped++
		seg.counters.drops[attrKey{kind: t.Kind, route: t.Route}]++
		s.applyLost(t.Seq, protocol.SpoolDropped)
	case opExpire:
		if t.Cause != causeExpiry {
			return &CorruptError{Path: seg.path, Reason: fmt.Sprintf("expire transition carries unknown cause %q", t.Cause)}
		}
		seg.counters.expired++
		seg.counters.expiries[attrKey{kind: t.Kind, route: t.Route}]++
		s.applyLost(t.Seq, protocol.SpoolDropped)
	default:
		return &CorruptError{Path: seg.path, Reason: fmt.Sprintf("unknown transition %q", t.Op)}
	}
	return nil
}

func (s *Spool) applyLost(seq uint64, state protocol.SpoolState) {
	m := s.lookup(seq)
	if m == nil {
		s.recovery.OrphanTransitions++
		return
	}
	s.setState(m, state)
}

func (s *Spool) lookup(seq uint64) *entryMeta { return s.entries[seq] }

// setState moves a record between queue states and keeps the depth counters, the per-segment
// liveness counts and the record's own state consistent. It is the only place a state
// changes, in recovery and at runtime alike.
func (s *Spool) setState(m *entryMeta, next protocol.SpoolState) {
	if m.state == next {
		return
	}
	seg := s.segmentByID(m.segID)
	leave := func(st protocol.SpoolState) {
		switch st {
		case protocol.SpoolPending:
			s.pending--
			if seg != nil {
				seg.pending--
			}
		case protocol.SpoolInFlight:
			s.inFlight--
			if seg != nil {
				seg.inFlight--
			}
		case "":
			// a record being created
		default:
			s.terminal--
		}
	}
	enter := func(st protocol.SpoolState) {
		switch st {
		case protocol.SpoolPending:
			s.pending++
			if seg != nil {
				seg.pending++
			}
		case protocol.SpoolInFlight:
			s.inFlight++
			if seg != nil {
				seg.inFlight++
			}
		default:
			s.terminal++
		}
	}
	leave(m.state)
	enter(next)
	m.state = next
}

func (s *Spool) segmentByID(id uint64) *segment {
	for _, seg := range s.segments {
		if seg.id == id {
			return seg
		}
	}
	return nil
}

// openActive reuses the newest segment as the append target, so a restart does not litter
// the log with one-segment-per-restart.
func (s *Spool) openActive() error {
	if n := len(s.segments); n > 0 {
		seg := s.segments[n-1]
		f, err := os.OpenFile(seg.path, os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("spool: reopening segment %s for append: %w", seg.path, err)
		}
		seg.file = f
		s.active = seg
		return nil
	}
	return s.roll()
}

func (s *Spool) roll() error {
	if s.nextSegmentSeq == 0 {
		s.nextSegmentSeq = 1
	}
	id := s.nextSegmentSeq
	s.nextSegmentSeq++
	path := filepath.Join(s.segsDir, segmentName(id))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("spool: creating segment %s: %w", path, err)
	}
	seg := &segment{id: id, path: path, file: f, counters: newSegmentCounters()}
	s.segments = append(s.segments, seg)
	s.active = seg
	syncDir(s.segsDir)
	return nil
}

func (s *Spool) rollIfNeededLocked(incoming int64) error {
	if s.active == nil {
		return s.roll()
	}
	if s.active.size > 0 && s.active.size+incoming > s.cfg.SegmentBytes {
		return s.roll()
	}
	return nil
}

// testReclaimBarrier, when non-nil, is called with the mutex held at the instant between
// folding a segment's counters into the counter file and unlinking that segment. That
// instant is the one window in which the counter design could double-count, so the crash test
// stops a real process exactly there and reopens the spool. It is nil in every production
// path.
var testReclaimBarrier func(seg *segment, droppedTotal uint64)

// reclaim unlinks whole segments whose records are all terminal, front to back, folding each
// segment's tombstones into the counter file first. Front-to-back order is not cosmetic: it
// is what keeps the counter exact, because a segment's tombstones are counted either from
// the segment or from the counter file, and the watermark says which.
func (s *Spool) reclaim() {
	for i := 0; i < len(s.segments); {
		seg := s.segments[i]
		if seg == s.active || seg.live() > 0 {
			// Nothing older than this can be unlinked without breaking the ordering
			// invariant, and the active segment is never unlinked.
			return
		}
		if err := s.foldSegment(seg); err != nil {
			return // keep the segment rather than lose its counters
		}
		if testReclaimBarrier != nil {
			dropped, _, _, _ := s.totalsLocked()
			testReclaimBarrier(seg, dropped)
		}
		s.forgetSegment(seg)
		seg.close()
		if err := os.Remove(seg.path); err != nil {
			return
		}
		s.diskBytes -= seg.size
		s.segments = append(s.segments[:i], s.segments[i+1:]...)
	}
}

// foldSegment moves a segment's counters into the counter file. It must complete before the
// segment is unlinked; a crash in between leaves the segment present with an id at or below
// the watermark, so its counters are not counted twice.
func (s *Spool) foldSegment(seg *segment) error {
	if seg.id <= s.persisted.SegmentWatermark {
		return nil
	}
	s.persisted.DroppedTotal += seg.counters.dropped
	s.persisted.ExpiredTotal += seg.counters.expired
	s.persisted.DeliveredTotal += seg.counters.delivered
	s.persisted.RejectedTotal += seg.counters.rejected
	mergeAttr(s.foldedDrops, seg.counters.drops)
	mergeAttr(s.foldedExpiries, seg.counters.expiries)
	s.persisted.SegmentWatermark = seg.id
	return s.saveCounters()
}

// saveCounters writes the counter file. It is called on every fold and on Close; it is not
// on the append path, so the spool does not pay a rename per observation.
func (s *Spool) saveCounters() error {
	c := s.persisted
	if s.nextFrameSeq > c.NextFrameSeq {
		c.NextFrameSeq = s.nextFrameSeq
	}
	if s.nextEntrySeq > c.NextEntrySeq {
		c.NextEntrySeq = s.nextEntrySeq
	}
	if s.nextSegmentSeq > c.NextSegmentSeq {
		c.NextSegmentSeq = s.nextSegmentSeq
	}
	c.Drops = attrSlice(s.foldedDrops)
	c.Expiries = attrSlice(s.foldedExpiries)
	c.CorruptSegments = s.recovery.CorruptSegments
	c.CorruptBytes = s.recovery.CorruptBytes
	c.CountersReset = s.recovery.CountersReset
	s.persisted = c
	return saveCounters(s.countersIn, c, s.aead)
}

// writeFrameLocked appends one complete frame. A short write is treated as a failure and the
// partial bytes are truncated away; if that also fails, the spool is poisoned, because
// appending after a torn frame would make every later frame unreadable.
func (s *Spool) writeFrameLocked(seg *segment, frame []byte) error {
	off := seg.size
	n, err := writeSegmentFrame(seg.file, frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if terr := seg.file.Truncate(off); terr != nil {
			s.poisoned = fmt.Errorf("write to %s failed (%v) and the partial frame could not be truncated (%v)", seg.path, err, terr)
		}
		return fmt.Errorf("spool: writing %s: %w", seg.path, err)
	}
	seg.size += int64(len(frame))
	s.diskBytes += int64(len(frame))
	s.framesSinceSync++
	if s.cfg.SyncEvery > 0 && s.framesSinceSync >= s.cfg.SyncEvery {
		if err := seg.file.Sync(); err != nil {
			return fmt.Errorf("spool: flushing %s: %w", seg.path, err)
		}
		s.framesSinceSync = 0
	}
	return nil
}

func (s *Spool) appendControlLocked(ctl controlRecord) error {
	pt, err := encodeControlFrame(ctl)
	if err != nil {
		return err
	}
	frameSeq := s.nextFrameSeq
	frame, err := encodeFrame(s.aead, frameControl, frameSeq, pt)
	if err != nil {
		return err
	}
	if err := s.rollIfNeededLocked(int64(len(frame))); err != nil {
		return err
	}
	if err := s.writeFrameLocked(s.active, frame); err != nil {
		return err
	}
	s.nextFrameSeq = frameSeq + 1
	// Apply only after the tombstone is durable: a drop that is not on disk must not be
	// counted, and a record that is not durably settled must stay in the queue.
	for _, t := range ctl.Transitions {
		if err := s.applyTransition(s.active, t); err != nil {
			return err
		}
	}
	return nil
}

func (s *Spool) checkWritable() error {
	if s.closed {
		return ErrClosed
	}
	if s.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrPoisoned, s.poisoned)
	}
	return nil
}

// Append writes one observation. It is the only way in: there is deliberately no Update that
// could rewrite a spooled observation (protocol.Store).
//
// The bound is enforced here, before the write, and nowhere else. At the bound the oldest
// pending observations are evicted and counted; an in-flight or already-delivered record is
// never evicted, and the new observation is never silently discarded in favour of them.
func (s *Spool) Append(e protocol.Entry) (protocol.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWritable(); err != nil {
		return e, err
	}

	// The record enters the queue as pending. An empty state is taken as pending; any other
	// state is a caller defect and is refused, because Append is not a way to rewrite
	// delivery state.
	if e.State == "" {
		e.State = protocol.SpoolPending
	}
	if err := e.Validate(); err != nil {
		return e, &EntryError{Reason: err.Error()}
	}
	if e.State != protocol.SpoolPending {
		return e, &EntryError{Reason: fmt.Sprintf("only a pending observation may be appended, got state %q", e.State)}
	}
	if e.SizeBytes <= 0 {
		e.SizeBytes = int64(len(e.Payload))
	}

	seq := s.nextEntrySeq
	now := s.cfg.Now()
	meta := dataMeta{
		Version:           recordVersion,
		Seq:               seq,
		ClientID:          e.ClientID,
		Kind:              string(e.Kind),
		Route:             string(e.Route),
		CollectionMode:    string(e.CollectionMode),
		ToolFingerprint:   e.ToolFingerprint,
		OccurredAt:        unixNano(e.OccurredAt),
		MonotonicOffsetMS: e.MonotonicOffsetMS,
		DedupKey:          e.DedupKey,
		SizeBytes:         e.SizeBytes,
		ExpiresAt:         unixNano(e.ExpiresAt),
		AppendedAt:        unixNano(now),
	}
	pt, err := encodeDataFrame(meta, e.Payload)
	if err != nil {
		return e, err
	}
	frameBytes := int64(len(pt) + frameHeaderSize + aeadTagSize + frameTrailerSize)

	s.reclaim()
	s.enforceBoundLocked(frameBytes)

	frameSeq := s.nextFrameSeq
	s.nextFrameSeq = frameSeq + 1
	frame, err := encodeFrame(s.aead, frameData, frameSeq, pt)
	if err != nil {
		return e, err
	}
	if err := s.rollIfNeededLocked(int64(len(frame))); err != nil {
		return e, err
	}
	seg := s.active
	off := seg.size
	if err := s.writeFrameLocked(seg, frame); err != nil {
		return e, err
	}
	s.nextEntrySeq = seq + 1

	m := &entryMeta{
		seq:             seq,
		attempts:        e.Attempts,
		sizeBytes:       e.SizeBytes,
		kind:            e.Kind,
		route:           e.Route,
		collectionMode:  e.CollectionMode,
		toolFingerprint: e.ToolFingerprint,
		clientID:        e.ClientID,
		dedupKey:        e.DedupKey,
		occurredAt:      e.OccurredAt,
		monotonicMS:     e.MonotonicOffsetMS,
		expiresAt:       e.ExpiresAt,
		appendedAt:      now,
		segID:           seg.id,
		off:             off,
		frameLen:        int64(len(frame)),
	}
	s.entries[seq] = m
	s.order = append(s.order, seq)
	s.setState(m, protocol.SpoolPending)

	e.Seq = seq
	e.State = protocol.SpoolPending
	return e, nil
}

// enforceBoundLocked is the one place the bound is enforced. It selects the oldest
// pending observations, then writes their tombstones, then applies them: a drop that is not
// durable must not be counted, and a record that is not durably dropped must stay pending.
//
// It never evicts an in-flight or already-delivered record. If the bound cannot be met by
// evicting pending records — for instance while a batch is in flight and pins the oldest
// segments — the observation is still accepted and the overage is counted, because the
// device must not stop observing because it cannot store.
func (s *Spool) enforceBoundLocked(incoming int64) {
	bounds := s.cfg.Bounds
	if bounds.MaxEntries <= 0 && bounds.MaxBytes <= 0 {
		return
	}
	victims := s.collectVictims(incoming)

	if len(victims) > 0 {
		now := unixNano(s.cfg.Now())
		for start := 0; start < len(victims); start += maxTransitionsPerFrame {
			end := start + maxTransitionsPerFrame
			if end > len(victims) {
				end = len(victims)
			}
			transitions := make([]transition, 0, end-start)
			for _, m := range victims[start:end] {
				transitions = append(transitions, transition{
					Op:    opDrop,
					Seq:   m.seq,
					Kind:  string(m.kind),
					Route: string(m.route),
					Cause: causeBound,
					At:    now,
				})
			}
			if err := s.appendControlLocked(controlRecord{Transitions: transitions}); err != nil {
				// The eviction did not happen; the records stay pending and the next append
				// will try again. No count is invented for a drop that was not written.
				return
			}
		}
		s.reclaim()
	}
	// Still over after evicting everything evictable — because the rest is in flight or
	// already terminal — is a fact the health report must see, not a reason to discard the
	// new observation.
	if bounds.MaxEntries > 0 && s.pending+s.inFlight+1 > bounds.MaxEntries {
		s.overBound++
	} else if bounds.MaxBytes > 0 && s.diskBytes+incoming > bounds.MaxBytes {
		s.overBound++
	}
}

// collectVictims selects the oldest pending records to evict, without changing any state.
// Selection is pure so that the tombstones can be written and made durable before anything
// is applied: a drop that is not on disk must not be counted.
func (s *Spool) collectVictims(incoming int64) []*entryMeta {
	bounds := s.cfg.Bounds
	var victims []*entryMeta
	perSeg := map[uint64]int{}

	// projectedDisk is what the spool would occupy if the selected victims were dropped and
	// every segment that thereby became fully terminal were reclaimed.
	projectedDisk := func() int64 {
		total := incoming + int64(len(victims))*tombstoneEstimate
		for _, seg := range s.segments {
			if seg.live()-perSeg[seg.id] > 0 {
				total += seg.size
			}
		}
		return total
	}

	cursor := 0
	nextVictim := func() *entryMeta {
		for cursor < len(s.order) {
			m := s.entries[s.order[cursor]]
			cursor++
			if m != nil && m.state == protocol.SpoolPending {
				return m
			}
		}
		return nil
	}

	for len(victims) <= s.pending {
		needCount := bounds.MaxEntries > 0 && s.pending+s.inFlight+1-len(victims) > bounds.MaxEntries
		needBytes := bounds.MaxBytes > 0 && projectedDisk() > bounds.MaxBytes
		if !needCount && !needBytes {
			break
		}
		m := nextVictim()
		if m == nil {
			break
		}
		victims = append(victims, m)
		perSeg[m.segID]++
	}
	return victims
}

func segmentName(id uint64) string {
	return fmt.Sprintf("%016d%s", id, segmentSuffix)
}

func parseSegmentName(name string) (uint64, bool) {
	if !strings.HasSuffix(name, segmentSuffix) {
		return 0, false
	}
	id, err := strconv.ParseUint(strings.TrimSuffix(name, segmentSuffix), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func parseQuarantinedName(name string) (uint64, bool) {
	if !strings.HasSuffix(name, segmentSuffix+quarantineSuffix) {
		return 0, false
	}
	id, err := strconv.ParseUint(strings.TrimSuffix(name, segmentSuffix+quarantineSuffix), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// quarantine renames a damaged file aside, keeping it for a human rather than deleting it.
// A quarantined segment is evidence; a deleted one is only a story. It returns the path the
// file now lives at, which is what a report should name.
func quarantine(path string) (string, error) {
	dest := path + quarantineSuffix
	for i := 1; ; i++ {
		if _, err := os.Stat(dest); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dest = fmt.Sprintf("%s%s.%d", path, quarantineSuffix, i)
	}
	if err := os.Rename(path, dest); err != nil {
		return "", fmt.Errorf("spool: quarantining %s: %w", path, err)
	}
	syncDir(filepath.Dir(path))
	return dest, nil
}
