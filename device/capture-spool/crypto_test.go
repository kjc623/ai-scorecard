package spool

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"
)

// Encryption at rest: a spool directory another local user can read must expose
// neither content nor metadata, and the key must not be in it.
func TestSpoolDirectoryHoldsNoPlaintextAndNoKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "keys", "spool.key")
	key, err := fileKey(keyPath)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	sp := openTest(t, dir, func(c *Config) { c.Key = key })

	appended := appendN(t, sp, 5)
	// Produce a counter file with drop attribution, to prove the metadata is sealed too.
	for i := 0; i < 3; i++ {
		if _, err := sp.Append(testEntry(50 + i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := sp.MarkInFlight([]uint64{appended[0].Seq}); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	if err := sp.Settle(appended[0].Seq, protocol.SpoolDelivered, ""); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	// A tiny bound forces eviction, so the counter file exists and carries attribution.
	if _, err := sp.Append(testEntry(90)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if onDisk := mustRead(t, keyPath); len(onDisk) != KeySize {
		t.Fatalf("key file is %d bytes, want %d", len(onDisk), KeySize)
	}
	if _, err := os.Stat(filepath.Join(dir, "counters.json")); err != nil {
		t.Fatalf("expected a counter file in the spool directory: %v", err)
	}

	secrets := [][]byte{
		[]byte("SECRET-PROMPT-MARKER"),
		[]byte("schema_version"), // envelope structure
		[]byte("proxy.tls"),      // route metadata
		[]byte("prompt"),         // kind metadata
		key,                      // the key itself
	}
	files := walkFiles(t, dir)
	if len(files) < 3 {
		t.Fatalf("expected segments, counters and a lock file, found %v", files)
	}
	for _, f := range files {
		b := mustRead(t, f)
		for _, s := range secrets {
			if containsBytes(b, s) {
				t.Fatalf("%s contains plaintext %q: the spool is not encrypted at rest", f, s)
			}
		}
	}
}

// A modified segment is detected on read and refused, never silently accepted.
func TestTamperedSegmentIsRefused(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir, func(c *Config) { c.SegmentBytes = 400 })
	appendN(t, sp, 6)
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	segs := segmentFiles(t, dir)
	if len(segs) < 3 {
		t.Fatalf("expected %d records to roll segments, got %v", 6, segs)
	}

	// Flip one byte in the second oldest segment: a complete frame that is not the frame
	// that was written.
	victim := segs[1]
	data := mustRead(t, victim)
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(victim, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Open(Config{Dir: dir, Key: testKey(), SyncEvery: -1})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open returned %v, want ErrCorrupt: tampering must be loud", err)
	}

	// The operational policy keeps the device collecting and reports the loss; it does not
	// start clean, and it does not delete the evidence.
	sp2 := openTest(t, dir, func(c *Config) { c.OnCorrupt = CorruptQuarantine })
	st := sp2.Extended()
	if st.Recovery.CorruptSegments != 1 {
		t.Fatalf("CorruptSegments = %d, want 1", st.Recovery.CorruptSegments)
	}
	if st.Recovery.LostEventsKnown {
		t.Fatal("LostEventsKnown is true after a corrupt segment: the spool must not claim a count it cannot know")
	}
	if len(st.Recovery.Quarantined) != 1 {
		t.Fatalf("Quarantined = %v, want one file", st.Recovery.Quarantined)
	}
	if st.Depth != 5 {
		t.Fatalf("Depth = %d, want 5: everything outside the damaged segment is still readable", st.Depth)
	}
	if _, err := os.Stat(st.Recovery.Quarantined[0]); err != nil {
		t.Fatalf("quarantined file is not on disk: %v", err)
	}
}

// The checksum is not the security boundary. A tamperer who fixes the CRC still fails: the
// frame is complete, so it is reported as corruption rather than quietly treated as a torn
// tail.
func TestTamperedTailFrameWithFixedChecksumIsStillRefused(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir)
	appendN(t, sp, 3)
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	segs := segmentFiles(t, dir)
	if len(segs) != 1 {
		t.Fatalf("expected one segment, got %v", segs)
	}
	data := mustRead(t, segs[0])
	offsets := frameOffsets(t, data)
	if len(offsets) != 3 {
		t.Fatalf("found %d frames, want 3", len(offsets))
	}
	last := offsets[len(offsets)-1]

	// Flip a byte inside the last frame's sealed body, then repair that frame's CRC
	// trailer — the checksum is not the security boundary, the AEAD is.
	data[len(data)-21] ^= 0x01
	binary.LittleEndian.PutUint32(data[len(data)-4:], crc32.ChecksumIEEE(data[last:len(data)-4]))
	if err := os.WriteFile(segs[0], data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Open(Config{Dir: dir, Key: testKey(), SyncEvery: -1})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open returned %v, want ErrCorrupt", err)
	}
	var ce *CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *CorruptError", err)
	}
	if ce.Reason != "frame failed authentication" {
		t.Fatalf("reason %q, want the authentication failure: a repaired checksum must not downgrade the verdict", ce.Reason)
	}
}

func TestWrongKeyFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir)
	appendN(t, sp, 3)
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := Open(Config{Dir: dir, Key: otherKey(t), SyncEvery: -1})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open returned %v, want ErrCorrupt: a spool that cannot be decrypted is not an empty spool", err)
	}
}

// Frames carry an authenticated sequence number, so reordering or replaying them is
// detected rather than silently accepted as history.
func TestReorderedAndDuplicatedFramesAreDetected(t *testing.T) {
	t.Run("reordered", func(t *testing.T) {
		dir := t.TempDir()
		sp := openTest(t, dir)
		// Two records with payloads of identical length produce two frames of identical
		// length, so the halves of the file can be swapped cleanly.
		for _, i := range []int{1, 2} {
			if _, err := sp.Append(testEntry(i)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		if err := sp.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		segs := segmentFiles(t, dir)
		data := mustRead(t, segs[0])
		if len(data)%2 != 0 {
			t.Fatalf("frames differ in length (%d bytes); the test needs equal frames", len(data))
		}
		half := len(data) / 2
		swapped := append(append([]byte{}, data[half:]...), data[:half]...)
		if err := os.WriteFile(segs[0], swapped, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Open(Config{Dir: dir, Key: testKey(), SyncEvery: -1}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open returned %v, want ErrCorrupt for reordered frames", err)
		}
	})

	t.Run("duplicated", func(t *testing.T) {
		dir := t.TempDir()
		sp := openTest(t, dir)
		if _, err := sp.Append(testEntry(1)); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := sp.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		segs := segmentFiles(t, dir)
		data := mustRead(t, segs[0])
		replayed := append(append([]byte{}, data...), data...)
		if err := os.WriteFile(segs[0], replayed, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Open(Config{Dir: dir, Key: testKey(), SyncEvery: -1}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open returned %v, want ErrCorrupt for a replayed frame", err)
		}
	})
}

// A physically incomplete trailing frame is what a killed write leaves. It cannot be a
// record, so it is discarded — and it is not counted as delivered or as dropped.
func TestIncompleteTailIsDiscardedNotCounted(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir)
	appendN(t, sp, 3)
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	segs := segmentFiles(t, dir)
	data := mustRead(t, segs[0])
	offsets := frameOffsets(t, data)
	lastFrameBytes := len(data) - offsets[len(offsets)-1]
	const cut = 20
	if err := os.Truncate(segs[0], int64(len(data)-cut)); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	sp2 := openTest(t, dir)
	st := sp2.Extended()
	// The whole incomplete frame is discarded — its header and the part of its body that
	// was written — not just the bytes that were removed from the end of the file.
	if want := int64(lastFrameBytes - cut); st.Recovery.TornBytes != want {
		t.Fatalf("TornBytes = %d, want %d", st.Recovery.TornBytes, want)
	}
	if st.Depth != 2 {
		t.Fatalf("Depth = %d, want 2: the incomplete frame was never a record", st.Depth)
	}
	if st.DroppedTotal != 0 || st.DeliveredTotal != 0 {
		t.Fatalf("a torn frame was counted: dropped %d delivered %d", st.DroppedTotal, st.DeliveredTotal)
	}
	// The truncation is durable: a second open finds a clean tail.
	if err := sp2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sp3 := openTest(t, dir)
	if got := sp3.Extended().Recovery.TornBytes; got != 0 {
		t.Fatalf("TornBytes = %d on the second open, want 0: recovery must truncate, not re-discover", got)
	}
	if got := sp3.Depth(); got != 2 {
		t.Fatalf("Depth = %d on the second open, want 2", got)
	}
}

// The counter file is data too: it is sealed, and a modified one is refused rather than
// trusted. Its loss is visible rather than silent.
func TestCounterFileIsSealedAndTamperIsDetected(t *testing.T) {
	dir := t.TempDir()
	sp := openTest(t, dir, func(c *Config) { c.Bounds = Bounds{MaxEntries: 2} })
	appendN(t, sp, 10)
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	countersPath := filepath.Join(dir, countersFileName)
	raw := mustRead(t, countersPath)
	if containsBytes(raw, []byte("proxy.tls")) || containsBytes(raw, []byte("prompt")) {
		t.Fatal("the counter file exposes drop attribution in plaintext")
	}
	raw[len(raw)/2] ^= 0x01
	if err := os.WriteFile(countersPath, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Open(Config{Dir: dir, Key: testKey(), SyncEvery: -1}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open returned %v, want ErrCorrupt for a modified counter file", err)
	}

	sp2 := openTest(t, dir, func(c *Config) { c.OnCorrupt = CorruptQuarantine })
	st := sp2.Extended()
	if !st.Recovery.CountersReset {
		t.Fatal("CountersReset is false after quarantining the counter file: the loss must be visible")
	}
	if st.Depth != 2 {
		t.Fatalf("Depth = %d, want 2: the queue itself is intact", st.Depth)
	}
}
