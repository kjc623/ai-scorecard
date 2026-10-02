package spool

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// testEntry builds a valid observation. The payload looks like an envelope because it is
// opaque to the spool; nothing here is parsed by the package under test.
func testEntry(i int) protocol.Entry {
	return protocol.Entry{
		ClientID:          fmt.Sprintf("client-%d", i),
		Kind:              protocol.KindPrompt,
		Route:             protocol.RouteProxyTLS,
		CollectionMode:    protocol.ModeM1,
		ToolFingerprint:   fmt.Sprintf("tool-%d", i%3),
		OccurredAt:        time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second),
		MonotonicOffsetMS: int64(i),
		DedupKey:          fmt.Sprintf("sha256:%064x", i),
		Payload:           []byte(fmt.Sprintf(`{"schema_version":"1.0","event_id":"e-%d","text":"SECRET-PROMPT-MARKER-%d"}`, i, i)),
		State:             protocol.SpoolPending,
	}
}

// testKey is a fixed key so a test can close and reopen a spool. It is a test key and is
// never used outside tests; the at-rest test below uses a file key instead.
func testKey(t *testing.T) KeyProvider {
	t.Helper()
	kp, err := NewMemoryKeyProvider([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	return kp
}

// otherKey is a different 32-byte key, for proving that a spool opened with the wrong key
// fails loudly rather than returning plausible records.
func otherKey(t *testing.T) KeyProvider {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	kp, err := NewMemoryKeyProvider(k)
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	return kp
}

func openTest(t *testing.T, dir string, tweak ...func(*Config)) *Spool {
	t.Helper()
	cfg := Config{
		Dir:       dir,
		Keys:      testKey(t),
		SyncEvery: -1, // tests crash on process kill, not power loss; see doc.go
	}
	for _, f := range tweak {
		f(&cfg)
	}
	sp, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func appendN(t *testing.T, sp *Spool, n int) []protocol.Entry {
	t.Helper()
	out := make([]protocol.Entry, 0, n)
	for i := 0; i < n; i++ {
		e, err := sp.Append(testEntry(i))
		if err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
		out = append(out, e)
	}
	return out
}

// walkFiles returns every regular file under root, for the at-rest assertions.
func walkFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, segmentsDirName))
	if err != nil {
		t.Fatalf("read segments dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if _, ok := parseSegmentName(e.Name()); ok {
			out = append(out, filepath.Join(dir, segmentsDirName, e.Name()))
		}
	}
	return out
}

// frameOffsets walks a segment file and returns the offset of every complete frame, using
// the same header layout the package writes. It exists so a test can tamper with a specific
// frame instead of guessing at byte positions.
func frameOffsets(t *testing.T, data []byte) []int {
	t.Helper()
	var out []int
	off := 0
	for off+frameHeaderSize <= len(data) {
		if !isMagic(data[off:]) {
			t.Fatalf("no frame magic at offset %d", off)
		}
		bodyLen := int(binary.LittleEndian.Uint32(data[off+16 : off+20]))
		next := off + frameHeaderSize + bodyLen + frameTrailerSize
		if next > len(data) {
			t.Fatalf("frame at %d claims to end at %d, past the %d-byte file", off, next, len(data))
		}
		out = append(out, off)
		off = next
	}
	return out
}
