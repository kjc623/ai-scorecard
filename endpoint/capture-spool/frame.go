package spool

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// The on-disk frame. Every frame is one indivisible unit:
//
//	header (32 bytes)           body (bodyLen)                trailer (4)
//	+------+------+-----+----+  +--------------------------+  +-----------+
//	| DSP1 | ver  | typ |key |  | AES-256-GCM(nonce, plain) |  | crc32(LE) |
//	| seq(8)     | bodyLen(4)|  |                          |  |           |
//	| nonce(12)               |  |                          |  |           |
//	+------+------+-----+----+  +--------------------------+  +-----------+
//
// The header is the additional authenticated data, so seq, type and bodyLen cannot be
// modified without failing authentication. The trailer is a plain CRC: it distinguishes "a
// frame the writer was killed in the middle of" from "a frame someone modified", and the
// AEAD — not the CRC — is what makes modification detectable in the first place.
const (
	frameHeaderSize  = 32
	frameTrailerSize = 4
	frameVersionV1   = 1

	// maxFrameBody bounds a sealed body. An envelope is capped at 256 KiB by the batch
	// contract; this is the ceiling that stops a corrupted length field from allocating
	// an attacker-sized buffer.
	maxFrameBody = 8 << 20

	aeadTagSize = 16
)

// frameMagic identifies the frame format. A file that does not start with it is not one of
// ours, which is a corruption rather than a torn tail.
var frameMagic = [4]byte{'D', 'S', 'P', '1'}

type frameType uint8

const (
	frameData     frameType = 1
	frameControl  frameType = 2
	frameCounters frameType = 3
)

// valid reports whether a frame type belongs to the log or to the counter file.
func (t frameType) valid() bool {
	return t == frameData || t == frameControl || t == frameCounters
}

// testWriteHalf replaces the single Write of a frame when it is non-nil. It is nil in every
// production path; the crash test sets it in a *child test process* so that a real process
// can be killed with a known half-written frame on disk.
var testWriteHalf func(f *os.File, frame []byte) (int, error)

// writeSegmentFrame appends one frame with a single Write call, so a kill leaves either the
// whole frame or a prefix of it.
func writeSegmentFrame(f *os.File, frame []byte) (int, error) {
	if testWriteHalf != nil {
		return testWriteHalf(f, frame)
	}
	return f.Write(frame)
}

// errCleanEnd is the internal end-of-log signal: the byte after the last complete frame.
var errCleanEnd = errors.New("spool: end of log")

type frameHeader struct {
	Version byte
	Type    frameType
	KeyID   byte
	Seq     uint64
	BodyLen uint32
	Nonce   [12]byte
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("spool: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("spool: AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("spool: GCM: %w", err)
	}
	return aead, nil
}

// encodeFrame builds one complete frame: header, sealed body, CRC trailer. It is assembled
// in one buffer and written with one Write call, so a killed process leaves either the whole
// frame or a prefix of it — never a valid header followed by another frame's body.
func encodeFrame(aead cipher.AEAD, ft frameType, seq uint64, plaintext []byte) ([]byte, error) {
	bodyLen := len(plaintext) + aead.Overhead()
	if bodyLen > maxFrameBody {
		return nil, fmt.Errorf("spool: frame body of %d bytes exceeds the %d-byte cap", bodyLen, maxFrameBody)
	}
	h := frameHeader{
		Version: frameVersionV1,
		Type:    ft,
		KeyID:   0,
		Seq:     seq,
		BodyLen: uint32(bodyLen),
	}
	if _, err := rand.Read(h.Nonce[:]); err != nil {
		return nil, fmt.Errorf("spool: nonce: %w", err)
	}
	header := h.marshal()
	frame := make([]byte, 0, frameHeaderSize+bodyLen+frameTrailerSize)
	frame = append(frame, header...)
	frame = aead.Seal(frame, h.Nonce[:], plaintext, header)
	frame = binary.LittleEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	return frame, nil
}

func (h frameHeader) marshal() []byte {
	b := make([]byte, frameHeaderSize)
	copy(b[0:4], frameMagic[:])
	b[4] = h.Version
	b[5] = byte(h.Type)
	b[6] = h.KeyID
	b[7] = 0 // reserved, must be zero so a future flag cannot be forged silently
	binary.LittleEndian.PutUint64(b[8:16], h.Seq)
	binary.LittleEndian.PutUint32(b[16:20], h.BodyLen)
	copy(b[20:32], h.Nonce[:])
	return b
}

// readFrameAt reads and opens the frame at off. It returns errCleanEnd at the end of the
// log, ErrTornTail for a physically incomplete trailing frame, and *CorruptError for a frame
// that is complete but is not what was written.
func readFrameAt(f *os.File, path string, off int64, aead cipher.AEAD) (frameHeader, []byte, int64, error) {
	var h frameHeader

	header := make([]byte, frameHeaderSize)
	n, err := f.ReadAt(header, off)
	if err != nil {
		if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && n == 0 {
			return h, nil, off, errCleanEnd
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return h, nil, off, ErrTornTail
		}
		return h, nil, off, fmt.Errorf("spool: reading %s at %d: %w", path, off, err)
	}
	if n < frameHeaderSize {
		return h, nil, off, ErrTornTail
	}
	if !isMagic(header) {
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: "bad frame magic"}
	}
	if header[4] != frameVersionV1 {
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: fmt.Sprintf("unsupported frame version %d", header[4])}
	}
	if header[7] != 0 {
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: "reserved header byte is not zero"}
	}
	ft := frameType(header[5])
	if !ft.valid() {
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: fmt.Sprintf("unknown frame type %d", header[5])}
	}
	h = frameHeader{
		Version: header[4],
		Type:    ft,
		KeyID:   header[6],
		Seq:     binary.LittleEndian.Uint64(header[8:16]),
		BodyLen: binary.LittleEndian.Uint32(header[16:20]),
	}
	copy(h.Nonce[:], header[20:32])

	if h.BodyLen < aeadTagSize {
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: fmt.Sprintf("body length %d is smaller than a tag", h.BodyLen)}
	}
	if h.BodyLen > maxFrameBody {
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: fmt.Sprintf("body length %d exceeds the cap", h.BodyLen)}
	}

	rest := make([]byte, int(h.BodyLen)+frameTrailerSize)
	n, err = f.ReadAt(rest, off+frameHeaderSize)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return h, nil, off, ErrTornTail
		}
		return h, nil, off, fmt.Errorf("spool: reading %s at %d: %w", path, off+frameHeaderSize, err)
	}
	if n < len(rest) {
		return h, nil, off, ErrTornTail
	}

	body := rest[:h.BodyLen]
	want := binary.LittleEndian.Uint32(rest[h.BodyLen:])
	if got := crc32.ChecksumIEEE(append(append([]byte{}, header...), body...)); got != want {
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: "frame checksum mismatch"}
	}

	plaintext, err := aead.Open(nil, h.Nonce[:], body, header)
	if err != nil {
		// A complete frame that will not open is tampering, a wrong key, or a frame
		// swapped in from elsewhere. It is never quietly skipped.
		return h, nil, off, &CorruptError{Path: path, Offset: off, Reason: "frame failed authentication"}
	}
	return h, plaintext, off + frameHeaderSize + int64(h.BodyLen) + frameTrailerSize, nil
}

func isMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == frameMagic[0] && b[1] == frameMagic[1] && b[2] == frameMagic[2] && b[3] == frameMagic[3]
}
