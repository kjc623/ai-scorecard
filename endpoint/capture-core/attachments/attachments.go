// Package attachments holds the bytes of files the browser extension transfers ahead of the
// observation that names them, so the agent can classify them on the device. The bytes are held in
// memory only and bounded, verified against their digest, claimed once by the observation, and
// dropped when it does not arrive in time. They are never written anywhere.
package attachments

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

const (
	// MaxBytes bounds one attachment: the largest document the classifier parses. It is below
	// protocol.MaxAttachmentBytes, the transport ceiling, because a classification request carries
	// the bytes base64-encoded in one frame and must stay inside protocol.MaxFrameBytes.
	MaxBytes = 32 << 20
	// MaxPendingBytes bounds everything one browser connection holds at once.
	MaxPendingBytes = 64 << 20
	// HoldFor is how long completed bytes wait for their observation. The extension sends the
	// observation right after its transfers finish.
	HoldFor = 2 * time.Minute
)

var (
	ErrTooLarge        = errors.New("attachments: over the size an attachment may be held at")
	ErrFull            = errors.New("attachments: the channel already holds as many bytes as it may")
	ErrMalformed       = errors.New("attachments: malformed transfer")
	ErrDigestMismatch  = errors.New("attachments: the bytes do not match the declared digest")
	ErrUnknownTransfer = errors.New("attachments: unknown transfer")
)

// Held is one completed transfer: its descriptor, with the digest of the bytes received, and the
// bytes.
type Held struct {
	Descriptor protocol.AttachmentDescriptor
	Bytes      []byte
}

type transfer struct {
	observation string
	descriptor  protocol.AttachmentDescriptor
	buf         bytes.Buffer
	chunks      int
	updated     time.Time
}

type completed struct {
	held Held
	at   time.Time
}

// Holder holds the transfers of one channel. It is safe for concurrent use.
type Holder struct {
	now func() time.Time

	mu      sync.Mutex
	open    map[string]*transfer
	done    map[string][]completed // by observation id
	pending int64
}

// New returns an empty holder.
func New(now func() time.Time) *Holder {
	if now == nil {
		now = time.Now
	}
	return &Holder{now: now, open: map[string]*transfer{}, done: map[string][]completed{}}
}

// Open accepts a manifest before any byte moves.
func (h *Holder) Open(m protocol.AttachmentManifest) error {
	switch {
	case m.TransferID == "" || m.Observation == "" || m.Descriptor.Name == "":
		return fmt.Errorf("%w: a manifest needs a transfer_id, an observation_id and a descriptor name", ErrMalformed)
	case m.Descriptor.SizeBytes < 0:
		return fmt.Errorf("%w: attachment %q declares a negative size", ErrMalformed, m.Descriptor.Name)
	case m.Descriptor.SizeBytes > MaxBytes:
		return fmt.Errorf("%w: attachment %q is %d bytes, over %d", ErrTooLarge, m.Descriptor.Name, m.Descriptor.SizeBytes, MaxBytes)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	if _, exists := h.open[m.TransferID]; exists {
		return fmt.Errorf("%w: transfer %q is already open", ErrMalformed, m.TransferID)
	}
	if h.pending+m.Descriptor.SizeBytes > MaxPendingBytes {
		return ErrFull
	}
	h.open[m.TransferID] = &transfer{observation: m.Observation, descriptor: m.Descriptor, updated: h.now()}
	return nil
}

// Append adds one chunk. Chunks are contiguous and zero-based: a chunk out of sequence is refused
// and the transfer waits for the expected one; bytes past the declared size end the transfer.
func (h *Holder) Append(c protocol.AttachmentChunk) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	t, ok := h.open[c.TransferID]
	if !ok {
		return fmt.Errorf("%w %q; the manifest must be accepted first", ErrUnknownTransfer, c.TransferID)
	}
	if c.Seq != t.chunks {
		return fmt.Errorf("%w: chunk %d arrived where %d was expected", ErrMalformed, c.Seq, t.chunks)
	}
	size := int64(t.buf.Len() + len(c.Data))
	if size > MaxBytes || size > t.descriptor.SizeBytes {
		h.dropLocked(c.TransferID)
		return fmt.Errorf("%w: transfer %q exceeded its declared size", ErrTooLarge, c.TransferID)
	}
	if h.pending+int64(len(c.Data)) > MaxPendingBytes {
		h.dropLocked(c.TransferID)
		return ErrFull
	}
	t.buf.Write(c.Data)
	h.pending += int64(len(c.Data))
	t.chunks++
	t.updated = h.now()
	return nil
}

// Complete closes a transfer. A transfer that reports a read error, is short of its declared size,
// or does not match a declared digest is dropped; otherwise its bytes wait for the observation.
func (h *Holder) Complete(c protocol.AttachmentComplete) (Held, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	t, ok := h.open[c.TransferID]
	if !ok {
		return Held{}, fmt.Errorf("%w %q", ErrUnknownTransfer, c.TransferID)
	}
	delete(h.open, c.TransferID)
	if c.Err != "" || int64(t.buf.Len()) != t.descriptor.SizeBytes || c.Chunks != t.chunks {
		h.pending -= int64(t.buf.Len())
		if c.Err != "" {
			return Held{}, fmt.Errorf("attachments: the extension could not read %q: %s", t.descriptor.Name, c.Err)
		}
		return Held{}, fmt.Errorf("%w: transfer %q ended with %d bytes in %d chunks", ErrMalformed, c.TransferID, t.buf.Len(), t.chunks)
	}
	sum := sha256.Sum256(t.buf.Bytes())
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if t.descriptor.ContentDigest != "" && t.descriptor.ContentDigest != digest {
		h.pending -= int64(t.buf.Len())
		return Held{}, ErrDigestMismatch
	}
	d := t.descriptor
	d.ContentDigest = digest
	held := Held{Descriptor: d, Bytes: t.buf.Bytes()}
	h.done[t.observation] = append(h.done[t.observation], completed{held: held, at: h.now()})
	return held, nil
}

// Claim hands over the completed attachments of one observation that the observation itself
// describes, matched by digest, and forgets every transfer for it. An attachment the observation
// does not name with the same digest is dropped unclassified.
func (h *Holder) Claim(observation string, descriptors []protocol.AttachmentDescriptor) []Held {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	held := h.done[observation]
	delete(h.done, observation)
	named := map[string]bool{}
	for _, d := range descriptors {
		if d.ContentDigest != "" {
			named[d.ContentDigest] = true
		}
	}
	var out []Held
	for _, c := range held {
		h.pending -= int64(len(c.held.Bytes))
		if named[c.held.Descriptor.ContentDigest] {
			out = append(out, c.held)
		}
	}
	return out
}

// Pending is the number of bytes held, open or completed.
func (h *Holder) Pending() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pending
}

func (h *Holder) dropLocked(id string) {
	if t, ok := h.open[id]; ok {
		h.pending -= int64(t.buf.Len())
		delete(h.open, id)
	}
}

// expireLocked drops open transfers that stalled and completed ones whose observation did not
// arrive within HoldFor.
func (h *Holder) expireLocked() {
	cut := h.now().Add(-HoldFor)
	for id, t := range h.open {
		if t.updated.Before(cut) {
			h.dropLocked(id)
		}
	}
	for obs, list := range h.done {
		kept := list[:0]
		for _, c := range list {
			if c.at.Before(cut) {
				h.pending -= int64(len(c.held.Bytes))
				continue
			}
			kept = append(kept, c)
		}
		if len(kept) == 0 {
			delete(h.done, obs)
		} else {
			h.done[obs] = kept
		}
	}
}
