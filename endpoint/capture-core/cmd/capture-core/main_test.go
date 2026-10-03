package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/detect"
	"github.com/shadow-ai-capture/device/protocol"
)

// The native-messaging framing is Chromium's, not device/protocol's, and the two must not drift
// into each other. These tests pin the byte layout.
func TestNativeFramingIsLittleEndianLengthPrefixed(t *testing.T) {
	payload := []byte(`{"type":"observation"}`)
	var buf bytes.Buffer
	if err := writeNativeFrame(&buf, payload); err != nil {
		t.Fatalf("writeNativeFrame: %v", err)
	}
	got := buf.Bytes()
	if len(got) != 4+len(payload) {
		t.Fatalf("frame is %d bytes, want %d", len(got), 4+len(payload))
	}
	if n := binary.LittleEndian.Uint32(got[:4]); int(n) != len(payload) {
		t.Fatalf("length prefix = %d, want %d (little-endian)", n, len(payload))
	}
	// The big-endian reading must be wrong, or the test is not proving the byte order.
	if n := binary.BigEndian.Uint32(got[:4]); int(n) == len(payload) && len(payload) > 1 {
		t.Fatalf("the prefix reads as %d in big-endian too; the case does not distinguish the orders", n)
	}
	round, err := readNativeFrame(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("readNativeFrame: %v", err)
	}
	if !bytes.Equal(round, payload) {
		t.Fatalf("round trip changed the payload: %q", round)
	}
}

func TestNativeFramingRefusesOversizeAndEmpty(t *testing.T) {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], maxNativeFrameBytes+1)
	if _, err := readNativeFrame(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("a frame declaring more than the limit was accepted")
	}
	binary.LittleEndian.PutUint32(hdr[:], 0)
	if _, err := readNativeFrame(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("an empty frame was accepted as a message")
	}
	if err := writeNativeFrame(&bytes.Buffer{}, make([]byte, maxNativeFrameBytes+1)); err == nil {
		t.Fatal("writing an oversize frame succeeded")
	}
}

// The conversion from a frame to what the pipeline consumes is the seam device/integration
// simulates. At M0 there must be no reader at all: a pipeline given one has already read the bytes.
func TestToCoreObservationM0HasNoContentReader(t *testing.T) {
	obs := protocol.ObservationMessage{
		ClientID: "c1", Route: protocol.RouteExtWebRequest,
		ToolFingerprint: "genai.web.chat.v1:gemini", SizeBytes: 4096, HasContent: false,
	}
	if err := obs.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	core := toCoreObservation(obs)
	if core.Content != nil {
		t.Fatal("an M0 frame produced a content reader")
	}
	if core.Decision == nil {
		t.Fatal("a prompt observation must carry a policy decision at every mode")
	}
}

func TestToCoreObservationCarriesContentBehindAReader(t *testing.T) {
	obs := protocol.ObservationMessage{
		ClientID: "c2", Route: protocol.RouteExtPageContext,
		ToolFingerprint: "genai.web.chat.v1:claude", SizeBytes: 5, HasContent: true, Content: []byte("hello"),
	}
	core := toCoreObservation(obs)
	if core.Content == nil {
		t.Fatal("a content-bearing frame produced no reader")
	}
	got, err := core.Content.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("reader returned %q", got)
	}
}

// The extractor must decode bytes to text without ever reusing the extension's own digest: the
// pipeline recomputes the canonical digest so two routes agree (docs/02 §4.2).
func TestExtensionExtractorDecodesBytes(t *testing.T) {
	text, atts, err := extensionExtractor{}.Extract([]byte("\xef\xbb\xbfSummarise  this."), "text/plain")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("attachments = %v, want none", atts)
	}
	if strings.HasPrefix(text, "\ufeff") {
		t.Fatal("a leading BOM survived extraction")
	}
	if !strings.Contains(text, "Summarise") {
		t.Fatalf("text = %q", text)
	}
	if _, _, err := (extensionExtractor{}).Extract(nil, ""); err == nil {
		t.Fatal("an empty payload was extracted instead of refused")
	}
}

// tasklist's CSV is the only process source available on this host; the parser is the part that can
// be tested everywhere, so it is tested rather than the command.
func TestParseTasklistCSV(t *testing.T) {
	const sample = `"System Idle Process","0","Services","0","8 K"
"ollama.exe","4242","Console","1","1,234,567 K"
"not-a-process","x","Console","1","1 K"
`
	procs, err := parseTasklistCSV(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(procs) != 2 {
		t.Fatalf("parsed %d processes, want 2 (a non-numeric PID is skipped, not guessed)", len(procs))
	}
	if procs[1].PID != 4242 || procs[1].ImagePath != "ollama.exe" {
		t.Fatalf("second process = %+v", procs[1])
	}
	var out []detect.ProcessInfo = procs
	if len(out) != 2 {
		t.Fatal("the enumerator must return detect.ProcessInfo values")
	}
}

// A frame whose body is not an observation must be answered with a typed refusal, never a panic and
// never silence: the extension is waiting.
func TestNativeHostRefusesMalformedInput(t *testing.T) {
	host := &nativeHost{cfg: Config{AttachmentCap: 1024}, transfers: map[string]*attachmentTransfer{}}
	for _, payload := range []string{
		`not json`,
		`{}`,
		`{"type":"unknown_type","version":1}`,
		`{"type":"observation","version":1,"body":{"route":"proxy.tls","tool_fingerprint":"x"}}`,
		`{"type":"attachment_chunk","version":1,"body":{"transfer_id":"nope","seq":0,"data":""}}`,
	} {
		resp := host.Handle(context.Background(), []byte(payload))
		var msg protocol.NativeMessage
		if err := json.Unmarshal(resp, &msg); err != nil {
			t.Fatalf("payload %q produced an unparseable response: %v", payload, err)
		}
		if msg.Type != protocol.TypeRefusal {
			t.Fatalf("payload %q produced %q, want a refusal", payload, msg.Type)
		}
	}
}

// A manifest whose attachment is over the cap is refused BEFORE any byte moves, and a chunk without
// an accepted manifest is refused rather than assembled into a partial file.
func TestNativeHostAttachmentLadder(t *testing.T) {
	host := &nativeHost{cfg: Config{AttachmentCap: 10}, transfers: map[string]*attachmentTransfer{}}

	over, _ := json.Marshal(mustMessage(protocol.TypeAttachmentManifest, "m1", protocol.AttachmentManifest{
		TransferID: "t1", Descriptor: protocol.AttachmentDescriptor{Name: "big.bin", SizeBytes: 11},
	}))
	if resp := host.Handle(context.Background(), over); !strings.Contains(string(resp), string(protocol.RefusalAttachmentTooLarge)) {
		t.Fatalf("oversized manifest response = %s", resp)
	}

	ok, _ := json.Marshal(mustMessage(protocol.TypeAttachmentManifest, "m2", protocol.AttachmentManifest{
		TransferID: "t2", Descriptor: protocol.AttachmentDescriptor{Name: "small.txt", SizeBytes: 4},
	}))
	if resp := host.Handle(context.Background(), ok); !strings.Contains(string(resp), string(protocol.TypeAck)) {
		t.Fatalf("acceptable manifest response = %s", resp)
	}

	gap, _ := json.Marshal(mustMessage(protocol.TypeAttachmentChunk, "m3", protocol.AttachmentChunk{TransferID: "t2", Seq: 3, Data: []byte("ab")}))
	if resp := host.Handle(context.Background(), gap); !strings.Contains(string(resp), string(protocol.RefusalMalformed)) {
		t.Fatalf("out-of-order chunk response = %s", resp)
	}

	unknown, _ := json.Marshal(mustMessage(protocol.TypeAttachmentChunk, "m4", protocol.AttachmentChunk{TransferID: "nope", Seq: 0, Data: []byte("ab")}))
	if resp := host.Handle(context.Background(), unknown); !strings.Contains(string(resp), string(protocol.RefusalMalformed)) {
		t.Fatalf("chunk without a manifest response = %s", resp)
	}
}

// The address parser is the one place a transport is chosen. A non-loopback TCP classifier address
// is refused: the classifier channel is local, and a remote one would be a different trust decision.
func TestClassifierAddressParsing(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"unix:/tmp/classifier.sock", "unix", false},
		{"pipe:\\\\.\\pipe\\sac-classifier", "pipe", false},
		{"tcp:127.0.0.1:9000", "tcp", false},
		{"", "", true},
		{"http://example.invalid", "", true},
	}
	for _, c := range cases {
		got, err := classifierAddress(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("classifierAddress(%q) accepted an unsupported form", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("classifierAddress(%q): %v", c.in, err)
			continue
		}
		if got.Network != c.want {
			t.Errorf("classifierAddress(%q).Network = %q, want %q", c.in, got.Network, c.want)
		}
	}
}

// A dry run resolves and validates everything and starts nothing — the check an operator runs before
// a change window.
func TestDryRunStartsNothing(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.TenantID = "11111111-1111-4111-8111-111111111111"
	cfg.DeviceID = "22222222-2222-4222-8222-222222222222"
	cfg.SpoolDir = t.TempDir()
	cfg.SpoolKey = cfg.SpoolDir + "-key"
	svc, err := newService(context.Background(), cfg, newLogger(cfg, runMode{}))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("dry-run start: %v", err)
	}
	if len(svc.sup.Order()) != 0 {
		t.Fatalf("a dry run recorded startup steps: %v", svc.sup.Order())
	}
}
