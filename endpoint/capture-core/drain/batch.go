package drain

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// BatchSchemaVersion is the /v1/events document version, independent of the API major version.
const BatchSchemaVersion = "1.0"

// builtBatch is one assembled batch, ready to POST: a fresh batch_id, the gzip body, and the
// entries (and their sequence numbers) that went into it.
type builtBatch struct {
	id      string
	body    []byte
	entries []protocol.Entry
	seqs    []uint64
}

// buildBatch selects up to protocol.MaxBatchEvents entries, oldest first, and packs them into a
// batch whose decompressed size stays under protocol.MaxDecompressedBytes and whose gzip body stays
// under protocol.MaxRequestBodyBytes. Entries handed in are already known to be at most
// protocol.MaxEnvelopeBytes each (oversize envelopes are rejected before this is called).
func buildBatch(entries []protocol.Entry, now time.Time) (*builtBatch, error) {
	if len(entries) == 0 {
		return nil, errors.New("drain: no entries to batch")
	}
	if len(entries) > protocol.MaxBatchEvents {
		entries = entries[:protocol.MaxBatchEvents]
	}

	// The decompressed cap is the decompression-bomb guard the server enforces on read.
	var total int64
	cut := len(entries)
	for i, e := range entries {
		total += int64(len(e.Payload))
		if total > protocol.MaxDecompressedBytes {
			cut = i
			break
		}
	}
	entries = entries[:cut]
	if len(entries) == 0 {
		return nil, errors.New("drain: a single entry exceeds the decompressed cap")
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}

	// The compressed cap: while the gzip body is over the limit (incompressible envelopes), drop
	// trailing entries. A single envelope is at most MaxEnvelopeBytes, so the loop terminates.
	for {
		body, err := gzipBatch(&protocol.EventBatch{
			SchemaVersion: BatchSchemaVersion,
			BatchID:       id,
			DeviceSentAt:  now.UTC(),
			EventCount:    len(entries),
			Events:        payloads(entries),
		})
		if err != nil {
			return nil, err
		}
		if len(body) <= protocol.MaxRequestBodyBytes {
			seqs := make([]uint64, len(entries))
			for i, e := range entries {
				seqs[i] = e.Seq
			}
			return &builtBatch{id: id, body: body, entries: entries, seqs: seqs}, nil
		}
		if len(entries) == 1 {
			return nil, fmt.Errorf("drain: a single envelope produces a %d-byte compressed body, over the %d-byte cap", len(body), protocol.MaxRequestBodyBytes)
		}
		entries = entries[:len(entries)-1]
	}
}

func payloads(entries []protocol.Entry) []json.RawMessage {
	out := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		out[i] = e.Payload
	}
	return out
}

// envelopeIdentity extracts the tenant_id/device_id an already-minted envelope carries, so the drain
// can refuse to deliver a record minted under a different identity than the current credential.
func envelopeIdentity(payload []byte) (tenantID, deviceID string) {
	var env struct {
		TenantID string `json:"tenant_id"`
		DeviceID string `json:"device_id"`
	}
	_ = json.Unmarshal(payload, &env)
	return env.TenantID, env.DeviceID
}

func gzipBatch(b *protocol.EventBatch) ([]byte, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("drain: encoding batch: %w", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return nil, fmt.Errorf("drain: compressing batch: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("drain: closing gzip: %w", err)
	}
	return buf.Bytes(), nil
}
