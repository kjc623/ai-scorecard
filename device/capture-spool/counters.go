package spool

import (
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// The counter file. dropped_total is a monotonic counter stored separately from the queue,
// so that it survives the deletion that produces it: a counter
// kept in the dropped rows would count itself away.
//
// The design here is one fold, not two ledgers. Every drop and expiry is a tombstone frame
// in a segment, and each segment's tombstones are counted either from the segment itself
// (while it exists) or from this file (after it is unlinked) — never from both. The two are
// separated by watermark: segments with id <= watermark have been folded into these totals
// and no longer exist; segments with id > watermark still exist and are counted from their
// own tombstones.
//
// The fold is ordered: reclaim walks segments front to back and folds a segment's counters
// in the same atomic rename that precedes its unlink, so a crash either happens before the
// fold (segment still present, counted from the segment) or after it (segment already
// accounted, and its id is at or below the watermark).
type countersFile struct {
	Version        int    `json:"v"`
	DroppedTotal   uint64 `json:"dropped_total"`
	ExpiredTotal   uint64 `json:"expired_total"`
	DeliveredTotal uint64 `json:"delivered_total"`
	RejectedTotal  uint64 `json:"rejected_total"`

	// SegmentWatermark is the highest segment id whose tombstones are folded into the
	// totals above. Every segment with a higher id is still on disk and is counted from
	// its own frames.
	SegmentWatermark uint64 `json:"segment_watermark"`

	// Sequence floors survive the deletion of every segment that carried them, so a
	// sequence number is never reused.
	NextFrameSeq   uint64 `json:"next_frame_seq"`
	NextEntrySeq   uint64 `json:"next_entry_seq"`
	NextSegmentSeq uint64 `json:"next_segment_seq"`

	// Drop attribution of folded segments, so "we lost 800 prompt events from the proxy
	// route" survives reclamation.
	Drops    []attribution `json:"drops,omitempty"`
	Expiries []attribution `json:"expiries,omitempty"`

	// Visible data loss, carried forward so the health report can say "spool_reinitialised
	// with N events lost" rather than starting clean.
	CorruptSegments int   `json:"corrupt_segments,omitempty"`
	CorruptBytes    int64 `json:"corrupt_bytes,omitempty"`
	CountersReset   bool  `json:"counters_reset,omitempty"`
}

// attribution is one (kind, route, count) row of what was lost.
type attribution struct {
	Kind  string `json:"kind"`
	Route string `json:"route"`
	Count uint64 `json:"count"`
}

type attrKey struct{ kind, route string }

func newSegmentCounters() segmentCounters {
	return segmentCounters{drops: map[attrKey]uint64{}, expiries: map[attrKey]uint64{}}
}

// segmentCounters counts the tombstones physically present in one segment.
type segmentCounters struct {
	dropped   uint64
	expired   uint64
	delivered uint64
	rejected  uint64
	drops     map[attrKey]uint64
	expiries  map[attrKey]uint64
}

func (c *segmentCounters) add(other segmentCounters) {
	c.dropped += other.dropped
	c.expired += other.expired
	c.delivered += other.delivered
	c.rejected += other.rejected
	mergeAttr(c.drops, other.drops)
	mergeAttr(c.expiries, other.expiries)
}

func mergeAttr(dst, src map[attrKey]uint64) {
	for k, v := range src {
		dst[k] = dst[k] + v
	}
}

func attrSlice(m map[attrKey]uint64) []attribution {
	if len(m) == 0 {
		return nil
	}
	out := make([]attribution, 0, len(m))
	for k, v := range m {
		out = append(out, attribution{Kind: k.kind, Route: k.route, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Route < out[j].Route
	})
	return out
}

func attrMap(s []attribution) map[attrKey]uint64 {
	m := make(map[attrKey]uint64, len(s))
	for _, a := range s {
		m[attrKey{kind: a.Kind, route: a.Route}] = m[attrKey{kind: a.Kind, route: a.Route}] + a.Count
	}
	return m
}

// The counter file is sealed with the same AEAD as the log, in the same frame format. It is
// small but it is not harmless: the drop attribution names kinds and routes, and a spool
// directory another local user can read must expose neither content nor metadata.
// Sealing it also means a modified counter file is detected rather than trusted.
func loadCounters(path string, aead cipher.AEAD) (countersFile, error) {
	var c countersFile
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return countersFile{Version: recordVersion}, nil
		}
		return c, fmt.Errorf("spool: reading counters: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return c, fmt.Errorf("spool: stating counters: %w", err)
	}
	h, pt, next, err := readFrameAt(f, path, 0, aead)
	if err != nil {
		if errors.Is(err, errCleanEnd) || errors.Is(err, ErrTornTail) {
			return c, &CorruptError{Path: path, Offset: 0, Reason: "counter file is incomplete"}
		}
		return c, err
	}
	if h.Type != frameCounters {
		return c, &CorruptError{Path: path, Offset: 0, Reason: "counter file does not carry a counter frame"}
	}
	if next != fi.Size() {
		return c, &CorruptError{Path: path, Offset: next, Reason: "counter file has trailing bytes"}
	}
	if err := json.Unmarshal(pt, &c); err != nil {
		return c, &CorruptError{Path: path, Offset: 0, Reason: "counter file is not valid JSON: " + err.Error()}
	}
	if c.Version != recordVersion {
		return c, &CorruptError{Path: path, Offset: 0, Reason: fmt.Sprintf("counter file version %d is not supported", c.Version)}
	}
	return c, nil
}

// saveCounters replaces the counter file atomically: write to a sibling temporary file,
// flush it, rename over the target. A reader therefore sees either the old file or the new
// one, never a partial write — the same property the segment log gets from a frame.
func saveCounters(path string, c countersFile, aead cipher.AEAD) error {
	c.Version = recordVersion
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("spool: encoding counters: %w", err)
	}
	frame, err := encodeFrame(aead, frameCounters, 0, b)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("spool: writing counters: %w", err)
	}
	if _, err := f.Write(frame); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("spool: writing counters: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("spool: flushing counters: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("spool: closing counters: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("spool: replacing counters: %w", err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir flushes a directory entry where the platform supports it. On Windows a directory
// handle cannot be flushed with os.File.Sync, and the rename above is already ordered by the
// filesystem's own journal; the process-kill crash model this package tests does not depend
// on it. It is best-effort by design, and documented rather than silently assumed.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync()
}
