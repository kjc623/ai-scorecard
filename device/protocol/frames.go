package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Length-prefixed framing for the local socket between capture-core and classifier-host
// (docs/01-collectors.md §3.4). Unix domain socket under the service's directory on macOS,
// named pipe on Windows; this file is the framing only, so both platforms share one
// implementation and one test.
//
//	+--------+--------+------------------+
//	| ver(1) | len(4) | payload (len)    |
//	+--------+--------+------------------+
//
// ver is the protocol version byte, carried per frame rather than negotiated once, so a
// version mismatch is detected on the frame that has it instead of corrupting the stream.
// len is big-endian and counts payload bytes only.
const (
	// Version is the current framing version. Both sides send it; a mismatch is a
	// degraded handshake, never a crash (docs/01-collectors.md §3.4).
	Version byte = 1

	headerSize = 5

	// MaxFrameBytes bounds a single frame. The classifier is fed one observation or one
	// document at a time, so the ceiling is the document cap, not a network MTU.
	MaxFrameBytes = 64 << 20
)

var (
	// ErrFrameTooLarge is returned instead of allocating an attacker-sized buffer.
	ErrFrameTooLarge = errors.New("protocol: frame exceeds MaxFrameBytes")
	// ErrVersionMismatch is returned when a peer speaks a different framing version.
	ErrVersionMismatch = errors.New("protocol: framing version mismatch")
)

// WriteFrame writes one frame. It is a single Write call so two writers on one pipe cannot
// interleave a header into the middle of another frame's payload.
func WriteFrame(w io.Writer, payload []byte) error {
	return WriteFrameVersion(w, Version, payload)
}

// WriteFrameVersion writes a frame carrying an explicit version, so a test can produce the
// mismatch case and so a future version can answer a peer without lying about its own.
func WriteFrameVersion(w io.Writer, version byte, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(payload))
	}
	buf := make([]byte, headerSize+len(payload))
	buf[0] = version
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame and returns its version and payload.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > MaxFrameBytes {
		return 0, nil, fmt.Errorf("%w: %d bytes declared", ErrFrameTooLarge, n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// ReadFrameChecked reads a frame and rejects a version mismatch before the caller parses
// it. Callers that want to answer a mismatched peer politely use ReadFrame instead.
func ReadFrameChecked(r io.Reader) ([]byte, error) {
	v, payload, err := ReadFrame(r)
	if err != nil {
		return nil, err
	}
	if v != Version {
		return nil, fmt.Errorf("%w: peer speaks %d, this host speaks %d", ErrVersionMismatch, v, Version)
	}
	return payload, nil
}

// HandshakeRequest is the first frame capture-core sends on a new classifier connection.
type HandshakeRequest struct {
	CoreVersion       string `json:"core_version"`
	ProtocolVersion   byte   `json:"protocol_version"`
	ClassifierVersion string `json:"classifier_version,omitempty"`
}

// HandshakeResponse answers a handshake. When OK is false the core falls back to
// rules-only with `confidence: degraded` and never fails the submission.
type HandshakeResponse struct {
	OK                bool   `json:"ok"`
	ClassifierVersion string `json:"classifier_version"`
	Reason            Detail `json:"reason,omitempty"`
}

// Mismatch reports whether a handshake response means the peer cannot serve.
func (h HandshakeResponse) Mismatch() bool { return !h.OK }
