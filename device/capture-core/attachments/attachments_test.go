package attachments

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func manifest(transfer, observation string, size int64) protocol.AttachmentManifest {
	return protocol.AttachmentManifest{TransferID: transfer, Observation: observation,
		Descriptor: protocol.AttachmentDescriptor{Name: transfer + ".txt", MediaType: "text/plain", SizeBytes: size}}
}

// sendWhole runs one whole transfer of data in a single chunk.
func sendWhole(t *testing.T, h *Holder, id, observation string, data []byte) Held {
	t.Helper()
	if err := h.Open(manifest(id, observation, int64(len(data)))); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := h.Append(protocol.AttachmentChunk{TransferID: id, Seq: 0, Data: data}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	held, err := h.Complete(protocol.AttachmentComplete{TransferID: id, Chunks: 1})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return held
}

func TestOpenRefusesBeforeAnyByteMoves(t *testing.T) {
	h := New(nil)
	for name, tc := range map[string]struct {
		m    protocol.AttachmentManifest
		want error
	}{
		"no transfer":    {manifest("", "o", 1), ErrMalformed},
		"no observation": {manifest("t", "", 1), ErrMalformed},
		"negative size":  {manifest("t", "o", -1), ErrMalformed},
		"over the cap":   {manifest("t", "o", MaxBytes+1), ErrTooLarge},
	} {
		if err := h.Open(tc.m); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if err := h.Open(manifest("t", "o", 1)); err != nil {
		t.Fatal(err)
	}
	if err := h.Open(manifest("t", "o", 1)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a second manifest for an open transfer answered %v", err)
	}
}

func TestOpenBoundsWhatOneChannelHolds(t *testing.T) {
	h := New(nil)
	big := make([]byte, MaxBytes)
	for i := 0; i < MaxPendingBytes/MaxBytes; i++ {
		sendWhole(t, h, string(rune('a'+i)), "o", big)
	}
	if err := h.Open(manifest("next", "o", 1)); !errors.Is(err, ErrFull) {
		t.Fatalf("a manifest past the channel's bound answered %v", err)
	}
	h.Claim("o", nil)
	if h.Pending() != 0 {
		t.Fatalf("pending = %d after the observation claimed everything", h.Pending())
	}
	if err := h.Open(manifest("next", "o", 1)); err != nil {
		t.Fatalf("the bound did not free: %v", err)
	}
}

func TestAppendKeepsChunksContiguousAndWithinTheDeclaredSize(t *testing.T) {
	h := New(nil)
	if err := h.Append(protocol.AttachmentChunk{TransferID: "t", Data: []byte("x")}); !errors.Is(err, ErrUnknownTransfer) {
		t.Fatalf("a chunk before its manifest answered %v", err)
	}
	if err := h.Open(manifest("t", "o", 4)); err != nil {
		t.Fatal(err)
	}
	if err := h.Append(protocol.AttachmentChunk{TransferID: "t", Seq: 1, Data: []byte("ab")}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a gap answered %v", err)
	}
	if err := h.Append(protocol.AttachmentChunk{TransferID: "t", Seq: 0, Data: []byte("ab")}); err != nil {
		t.Fatalf("the expected chunk after a refused gap: %v", err)
	}
	if err := h.Append(protocol.AttachmentChunk{TransferID: "t", Seq: 1, Data: []byte("cde")}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("bytes past the declared size answered %v", err)
	}
	if h.Pending() != 0 {
		t.Fatalf("pending = %d after an oversize transfer was dropped", h.Pending())
	}
	if _, err := h.Complete(protocol.AttachmentComplete{TransferID: "t", Chunks: 1}); !errors.Is(err, ErrUnknownTransfer) {
		t.Fatalf("completing a dropped transfer answered %v", err)
	}
}

func TestCompleteDropsWhatCannotBeClassified(t *testing.T) {
	h := New(nil)
	open := func(id string, d protocol.AttachmentDescriptor, data []byte) {
		t.Helper()
		if err := h.Open(protocol.AttachmentManifest{TransferID: id, Observation: "o", Descriptor: d}); err != nil {
			t.Fatal(err)
		}
		if err := h.Append(protocol.AttachmentChunk{TransferID: id, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	open("read-error", protocol.AttachmentDescriptor{Name: "a", SizeBytes: 3}, []byte("abc"))
	if _, err := h.Complete(protocol.AttachmentComplete{TransferID: "read-error", Chunks: 1, Err: "file vanished"}); err == nil {
		t.Fatal("a transfer the extension could not read was held")
	}
	open("short", protocol.AttachmentDescriptor{Name: "b", SizeBytes: 5}, []byte("abc"))
	if _, err := h.Complete(protocol.AttachmentComplete{TransferID: "short", Chunks: 1}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a short transfer answered %v", err)
	}
	open("digest", protocol.AttachmentDescriptor{Name: "c", SizeBytes: 3, ContentDigest: digestOf([]byte("xyz"))}, []byte("abc"))
	if _, err := h.Complete(protocol.AttachmentComplete{TransferID: "digest", Chunks: 1}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("bytes that do not match the declared digest answered %v", err)
	}
	if h.Pending() != 0 {
		t.Fatalf("pending = %d after every transfer was dropped", h.Pending())
	}
	if held := h.Claim("o", nil); len(held) != 0 {
		t.Fatalf("dropped transfers were claimable: %+v", held)
	}
}

func TestClaimHandsOverOnlyWhatTheObservationNames(t *testing.T) {
	h := New(nil)
	doc := []byte("charge card 4111 1111 1111 1111")
	held := sendWhole(t, h, "t1", "o1", doc)
	if held.Descriptor.ContentDigest != digestOf(doc) {
		t.Fatalf("held digest = %q, want the digest of the bytes received", held.Descriptor.ContentDigest)
	}
	sendWhole(t, h, "t2", "o1", []byte("not named by the observation"))
	sendWhole(t, h, "t3", "o2", []byte("another observation"))

	got := h.Claim("o1", []protocol.AttachmentDescriptor{{Name: "t1.txt", ContentDigest: digestOf(doc)}})
	if len(got) != 1 || string(got[0].Bytes) != string(doc) || got[0].Descriptor.MediaType != "text/plain" {
		t.Fatalf("claim = %+v, want the one named attachment", got)
	}
	if again := h.Claim("o1", []protocol.AttachmentDescriptor{{ContentDigest: digestOf(doc)}}); len(again) != 0 {
		t.Fatal("an observation claimed its attachments twice")
	}
	if want := int64(len("another observation")); h.Pending() != want {
		t.Fatalf("pending = %d, want only the other observation's %d", h.Pending(), want)
	}
}

func TestBytesWhoseObservationDoesNotArriveExpire(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := New(c.now)
	doc := []byte("held for an observation that never comes")
	sendWhole(t, h, "t1", "o1", doc)
	if err := h.Open(manifest("stalled", "o2", 10)); err != nil {
		t.Fatal(err)
	}
	if err := h.Append(protocol.AttachmentChunk{TransferID: "stalled", Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}

	c.t = c.t.Add(HoldFor + time.Second)
	if held := h.Claim("o1", []protocol.AttachmentDescriptor{{ContentDigest: digestOf(doc)}}); len(held) != 0 {
		t.Fatal("bytes were classified after their hold expired")
	}
	if h.Pending() != 0 {
		t.Fatalf("pending = %d after every hold expired", h.Pending())
	}
	if err := h.Append(protocol.AttachmentChunk{TransferID: "stalled", Seq: 1, Data: []byte("d")}); !errors.Is(err, ErrUnknownTransfer) {
		t.Fatalf("a stalled transfer was still open: %v", err)
	}
}

// The largest attachment held still fits, base64-encoded, in one classification frame, and is
// within the transport ceiling the extension checks.
func TestTheCapFitsOneClassificationFrame(t *testing.T) {
	if MaxBytes > protocol.MaxAttachmentBytes {
		t.Fatalf("MaxBytes %d is over the transport ceiling %d", MaxBytes, protocol.MaxAttachmentBytes)
	}
	if n := base64.StdEncoding.EncodedLen(MaxBytes) + 4096; n > protocol.MaxFrameBytes {
		t.Fatalf("a classification request for %d bytes needs a %d-byte frame, over %d", MaxBytes, n, protocol.MaxFrameBytes)
	}
}
