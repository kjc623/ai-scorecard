package spool

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// recordVersion is the version of the *frame plaintext* encoding, independent of the frame
// header version. It is written into every record so a future layout change is detectable
// rather than silently misparsed.
const recordVersion = 1

// dataMeta is the indexable part of a spooled observation. It is a mirror of protocol.Entry
// minus the payload: the payload bytes follow it verbatim, so the exact bytes the device
// minted are what is stored and what Peek returns (ADR 0004: an observation is immutable).
//
// The spool never parses Payload. It has no field for prompt text, no decoder for the
// envelope, and no code path that could rewrite one.
type dataMeta struct {
	Version           int    `json:"v"`
	Seq               uint64 `json:"seq"`
	ClientID          string `json:"client_id,omitempty"`
	Kind              string `json:"kind"`
	Route             string `json:"route"`
	CollectionMode    string `json:"collection_mode"`
	ToolFingerprint   string `json:"tool_fingerprint"`
	OccurredAt        int64  `json:"occurred_at"`
	MonotonicOffsetMS int64  `json:"monotonic_offset_ms"`
	DedupKey          string `json:"dedup_key"`
	SizeBytes         int64  `json:"size_bytes"`
	ExpiresAt         int64  `json:"expires_at,omitempty"`
	AppendedAt        int64  `json:"appended_at"`
	Attempts          int    `json:"attempts,omitempty"`
}

// encodeDataFrame lays out metaLen(4, LE) || meta JSON || payload bytes. The payload is not
// re-encoded: base64 or a JSON wrapper would inflate it and would be a transformation of
// bytes the device minted.
func encodeDataFrame(meta dataMeta, payload []byte) ([]byte, error) {
	mj, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("spool: encoding spool metadata: %w", err)
	}
	if len(mj) > 64<<10 {
		return nil, fmt.Errorf("spool: spool metadata of %d bytes is implausible", len(mj))
	}
	buf := make([]byte, 4+len(mj)+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(mj)))
	copy(buf[4:], mj)
	copy(buf[4+len(mj):], payload)
	return buf, nil
}

func decodeDataFrame(pt []byte) (dataMeta, []byte, error) {
	var meta dataMeta
	if len(pt) < 4 {
		return meta, nil, fmt.Errorf("spool: data frame shorter than its length prefix")
	}
	n := int(binary.LittleEndian.Uint32(pt[0:4]))
	if n < 0 || 4+n > len(pt) {
		return meta, nil, fmt.Errorf("spool: data frame metadata length %d does not fit the frame", n)
	}
	if err := json.Unmarshal(pt[4:4+n], &meta); err != nil {
		return meta, nil, fmt.Errorf("spool: decoding spool metadata: %w", err)
	}
	if meta.Version != recordVersion {
		return meta, nil, fmt.Errorf("spool: spool record version %d is not supported", meta.Version)
	}
	payload := pt[4+n:]
	if int64(len(payload)) != meta.SizeBytes {
		return meta, nil, fmt.Errorf("spool: record declares %d payload bytes but carries %d", meta.SizeBytes, len(payload))
	}
	return meta, payload, nil
}

// Transition operations. A state change is appended, never written over a record, which is
// what keeps the log append-only and a delivered observation unrewritten.
const (
	opClaim   = "claim"   // pending -> in_flight, attempts+1
	opRelease = "release" // in_flight -> pending (retryable failure)
	opSettle  = "settle"  // -> delivered | rejected | pending
	opDrop    = "drop"    // evicted by the bound; counted as a drop
	opExpire  = "expire"  // past its retention deadline; counted separately from a drop
)

// Drop causes. §12.2 counts an overflow drop and a retention expiry separately, because
// they are different failures with different fixes.
const (
	causeBound  = "bound"
	causeExpiry = "expiry"
)

type transition struct {
	Op        string `json:"op"`
	Seq       uint64 `json:"seq"`
	State     string `json:"state,omitempty"` // settle target
	Reason    string `json:"reason,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
	Kind      string `json:"kind,omitempty"`  // drop/expire attribution
	Route     string `json:"route,omitempty"` // drop/expire attribution
	Cause     string `json:"cause,omitempty"`
	At        int64  `json:"at,omitempty"`
}

type controlRecord struct {
	Version     int          `json:"v"`
	Transitions []transition `json:"transitions"`
}

func encodeControlFrame(ctl controlRecord) ([]byte, error) {
	ctl.Version = recordVersion
	b, err := json.Marshal(ctl)
	if err != nil {
		return nil, fmt.Errorf("spool: encoding control record: %w", err)
	}
	return b, nil
}

func decodeControlFrame(pt []byte) (controlRecord, error) {
	var ctl controlRecord
	if err := json.Unmarshal(pt, &ctl); err != nil {
		return ctl, fmt.Errorf("spool: decoding control record: %w", err)
	}
	if ctl.Version != recordVersion {
		return ctl, fmt.Errorf("spool: control record version %d is not supported", ctl.Version)
	}
	if len(ctl.Transitions) == 0 {
		return ctl, fmt.Errorf("spool: control record carries no transitions")
	}
	return ctl, nil
}

// entryMeta is the in-memory index row. It carries no payload: Peek reads the payload back
// from the segment at off, so a 25 MB spool does not become a 25 MB heap index and a
// spooled payload is never held twice.
type entryMeta struct {
	seq             uint64
	state           protocol.SpoolState
	attempts        int
	sizeBytes       int64
	kind            protocol.Kind
	route           protocol.Route
	collectionMode  protocol.CollectionMode
	toolFingerprint string
	clientID        string
	dedupKey        string
	occurredAt      time.Time
	monotonicMS     int64
	expiresAt       time.Time
	appendedAt      time.Time
	lastError       string
	rejectReason    string
	segID           uint64
	off             int64
	frameLen        int64
}

// toEntry rebuilds the protocol record. Payload is returned byte-for-byte as appended.
func (m *entryMeta) toEntry(payload []byte) protocol.Entry {
	return protocol.Entry{
		Seq:               m.seq,
		ClientID:          m.clientID,
		Kind:              m.kind,
		Route:             m.route,
		CollectionMode:    m.collectionMode,
		ToolFingerprint:   m.toolFingerprint,
		OccurredAt:        m.occurredAt,
		MonotonicOffsetMS: m.monotonicMS,
		DedupKey:          m.dedupKey,
		Payload:           payload,
		SizeBytes:         m.sizeBytes,
		ExpiresAt:         m.expiresAt,
		State:             m.state,
		Attempts:          m.attempts,
		LastError:         m.lastError,
		RejectReason:      m.rejectReason,
	}
}

// indexable reports whether an entry occupies the queue: pending and in-flight records are
// the undelivered set that depth counts and that only drop-oldest may remove.
func indexable(st protocol.SpoolState) bool {
	return st == protocol.SpoolPending || st == protocol.SpoolInFlight
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func timeFromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
