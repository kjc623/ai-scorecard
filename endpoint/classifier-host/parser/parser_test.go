package parser_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/classifier-host/internal/importcheck"
	"github.com/shadow-ai-capture/device/classifier-host/parser"
	"github.com/shadow-ai-capture/device/protocol"
)

func writeZip(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPlainTextIsExtracted(t *testing.T) {
	res := parser.Parse("text/plain", []byte("hello  world"), parser.DefaultLimits())
	if !res.OK() || res.Text != "hello world" {
		t.Fatalf("plain text: %+v", res)
	}
	if res.BytesIn != 12 || res.BytesOut != len(res.Text) {
		t.Errorf("accounting: %+v", res)
	}
}

// TestDecompressionBombIsBounded is §10's first hostile document: a small archive that expands
// hundreds of times over must become a status code, not an allocation.
func TestDecompressionBombIsBounded(t *testing.T) {
	// 128 MiB of zeros compresses to a few hundred kilobytes; the parser must never materialise it.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zeros := make([]byte, 1<<20)
	for i := 0; i < 128; i++ {
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz.Len() > 8<<20 {
		t.Fatalf("the bomb fixture is %d bytes compressed; that is not a bomb", gz.Len())
	}
	res := parser.Parse("application/gzip", gz.Bytes(), parser.DefaultLimits())
	if res.Status != parser.StatusOutputCap {
		t.Fatalf("a decompression bomb produced %+v", res)
	}
	if !res.Truncated || len(res.Text) > parser.DefaultLimits().MaxOutputBytes {
		t.Errorf("the output was not bounded: %d bytes truncated=%v", len(res.Text), res.Truncated)
	}
}

// TestHugeDeclaredMemberIsRefusedBeforeReading is §10's "huge declared size": the declared
// uncompressed size is checked before the member is opened, so a 20 MB member cannot make the
// parser allocate 20 MB to discover it is over the cap.
func TestHugeDeclaredMemberIsRefusedBeforeReading(t *testing.T) {
	limits := parser.DefaultLimits()
	limits.MaxEntryBytes = 1 << 20
	body := writeZip(t, map[string][]byte{"big.txt": make([]byte, 8<<20)})
	res := parser.Parse("application/zip", body, limits)
	if res.Status != parser.StatusInputOverCap {
		t.Fatalf("a member declaring 8 MiB against a 1 MiB cap produced %+v", res)
	}
	if !strings.Contains(res.Err, "declares") {
		t.Errorf("the refusal does not name the declared size: %q", res.Err)
	}
}

// TestDeepNestingIsRefused is §10's "deeply nested" document.
func TestDeepNestingIsRefused(t *testing.T) {
	depth := 5000
	body := strings.Repeat("[", depth) + `"x"` + strings.Repeat("]", depth)
	res := parser.Parse("application/json", []byte(body), parser.DefaultLimits())
	if res.Status != parser.StatusDepthExceeded {
		t.Fatalf("a %d-deep body produced %+v", depth, res)
	}
	if res.Text != "" {
		t.Errorf("a refused body still produced text: %q", res.Text)
	}
}

func TestUnsupportedAndUndecodableFormats(t *testing.T) {
	if res := parser.Parse("application/pdf", []byte("%PDF-1.7 ..."), parser.DefaultLimits()); res.Status != parser.StatusUnsupported {
		t.Errorf("PDF produced %+v", res)
	}
	if res := parser.Parse("application/octet-stream", []byte{0x00, 0x01, 0x02}, parser.DefaultLimits()); res.Status != parser.StatusUndecodable {
		t.Errorf("undecodable bytes produced %+v", res)
	}
	if res := parser.Parse("application/gzip", []byte("not gzip at all"), parser.DefaultLimits()); res.Status != parser.StatusMalformed {
		t.Errorf("a malformed container produced %+v", res)
	}
	if res := parser.Parse("application/zip", []byte("not a zip"), parser.DefaultLimits()); res.Status != parser.StatusMalformed {
		t.Errorf("a malformed archive produced %+v", res)
	}
}

func TestDeclaredSizeOverCapIsRefused(t *testing.T) {
	limits := parser.DefaultLimits()
	limits.MaxDeclaredBytes = 16
	res := parser.Parse("text/plain", []byte(strings.Repeat("x", 17)), limits)
	if res.Status != parser.StatusInputOverCap {
		t.Fatalf("an over-cap document produced %+v", res)
	}
}

// TestDocumentContainerTextIsExtracted exercises the real docx path: a zip whose word/document.xml
// holds the text.
func TestDocumentContainerTextIsExtracted(t *testing.T) {
	docx := writeZip(t, map[string][]byte{
		"[Content_Types].xml":   []byte(`<Types/>`),
		"word/document.xml":     []byte(`<w:document><w:body><w:p>card 4111 1111 1111 1111</w:p></w:body></w:document>`),
		"word/media/image1.png": []byte{0x89, 'P', 'N', 'G', 0x00, 0xff},
	})
	res := parser.Parse("application/vnd.openxmlformats-officedocument.wordprocessingml.document", docx, parser.DefaultLimits())
	if !res.OK() {
		t.Fatalf("docx produced %+v", res)
	}
	if !strings.Contains(res.Text, "4111 1111 1111 1111") {
		t.Errorf("docx text was not extracted: %q", res.Text)
	}
	if len(res.Offsets) == 0 {
		t.Error("§10's Output row asks for offsets, and none were produced")
	}
}

func TestZipMemberSelectionSkipsBinaries(t *testing.T) {
	body := writeZip(t, map[string][]byte{
		"notes.txt":  []byte("hello"),
		"blob.bin":   {0xff, 0xfe, 0xfd},
		"empty.txt":  {},
		"data.json":  []byte(`{"a":"b"}`),
		"image.jpeg": {0xff, 0xd8, 0xff},
	})
	res := parser.Parse("application/zip", body, parser.DefaultLimits())
	if !res.OK() {
		t.Fatalf("zip produced %+v", res)
	}
	if !strings.Contains(res.Text, "hello") || !strings.Contains(res.Text, "a b") {
		t.Errorf("text members were not extracted: %q", res.Text)
	}
}

// TestOneRequestOneResult is §10's "one document, one process" at the wire level: the child reads
// exactly one framed request and writes exactly one framed result.
func TestRequestAndResultFraming(t *testing.T) {
	doc := []byte("4111 1111 1111 1111")
	payload, err := parser.EncodeRequest(parser.Header{MediaType: "text/plain", DeclaredSize: int64(len(doc))}, doc)
	if err != nil {
		t.Fatal(err)
	}
	h, got, err := parser.DecodeRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if h.MediaType != "text/plain" || h.DeclaredSize != int64(len(doc)) || !bytes.Equal(got, doc) {
		t.Fatalf("round trip lost data: %+v %q", h, got)
	}

	var stdout bytes.Buffer
	var stdin bytes.Buffer
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: int64(len(doc))}, doc); err != nil {
		t.Fatal(err)
	}
	if code := parser.Execute(&stdin, &stdout, parser.DefaultLimits(), nil); code != 0 {
		t.Fatalf("child exit code %d", code)
	}
	res, err := parser.ReadResult(&stdout)
	if err != nil {
		t.Fatalf("the child's result frame is unreadable: %v", err)
	}
	if res.Status != parser.StatusOK || res.Text != string(doc) {
		t.Errorf("child result: %+v", res)
	}
	if res.PeakAllocBytes <= 0 {
		t.Error("the child did not report its own heap footprint, informational as it is")
	}
}

func TestDeclaredSizeMustMatchTheBytesReceived(t *testing.T) {
	var stdin, stdout bytes.Buffer
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: 4096}, []byte("short")); err != nil {
		t.Fatal(err)
	}
	if code := parser.Execute(&stdin, &stdout, parser.DefaultLimits(), nil); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	res, err := parser.ReadResult(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != parser.StatusInputShort {
		t.Errorf("a lying declared_size produced %+v", res)
	}
}

func TestReadFrameVersionIsChecked(t *testing.T) {
	var buf bytes.Buffer
	if err := protocol.WriteFrameVersion(&buf, protocol.Version+1, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ReadResult(&buf); !errors.Is(err, protocol.ErrVersionMismatch) {
		t.Errorf("a mismatched child frame returned %v", err)
	}
}

// TestParserChildCannotReachAnything is the structural half of §10's Privileges row: the child that
// parses an attacker-chosen document cannot name os, net, exec or syscall, so "no filesystem path
// to key material, verifiable by inspection" is a property of the import graph as well as of the
// platform sandbox.
func TestParserChildCannotReachAnything(t *testing.T) {
	dir := parserDir(t)
	importcheck.AssertClosed(t, dir,
		"archive/zip", "bytes", "compress/gzip", "encoding/binary", "encoding/json", "io", "runtime", "strings",
		"github.com/shadow-ai-capture/device/classifier-host/norm",
		"github.com/shadow-ai-capture/device/protocol")
}

func TestTheChildRefusesAMalformedRequestHeader(t *testing.T) {
	var stdout bytes.Buffer
	body := []byte{0x00, 0x00, 0x00, 0xff} // header length longer than the payload
	if err := protocol.WriteFrame(&stdout, body); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := parser.Execute(&stdout, &out, parser.DefaultLimits(), nil); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	res, err := parser.ReadResult(&out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != parser.StatusInputShort {
		t.Errorf("a malformed header produced %+v", res)
	}
}

func TestChildLimitsComeFromTheParentRequest(t *testing.T) {
	doc := []byte("4111 1111 1111 1111")
	var stdin, stdout bytes.Buffer
	limits := parser.DefaultLimits()
	limits.MaxOutputBytes = 4
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: int64(len(doc)), Limits: &limits}, doc); err != nil {
		t.Fatal(err)
	}
	if code := parser.Execute(&stdin, &stdout, parser.DefaultLimits(), nil); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	res, err := parser.ReadResult(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != parser.StatusOutputCap || len(res.Text) > 4 {
		t.Errorf("the parent's child-side cap was not applied: %+v", res)
	}
}

func parserDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Dir(file)
}

var _ = json.Marshal
