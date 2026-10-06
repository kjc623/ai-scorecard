package parser_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
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

func gzipBomb(t *testing.T, mib int) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zeros := make([]byte, 1<<20)
	for i := 0; i < mib; i++ {
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

func TestPlainTextIsExtracted(t *testing.T) {
	if res := parser.Parse("text/plain", []byte("hello  world"), parser.DefaultLimits()); res.Status != parser.StatusOK || res.Text != "hello world" {
		t.Fatalf("plain text: %+v", res)
	}
}

func TestDecompressionBombIsBounded(t *testing.T) {
	bomb := gzipBomb(t, 128)
	if len(bomb) > 8<<20 {
		t.Fatalf("the fixture is %d bytes compressed, which is not a bomb", len(bomb))
	}
	res := parser.Parse("application/gzip", bomb, parser.DefaultLimits())
	if res.Status != parser.StatusOutputCap || !res.Truncated || len(res.Text) > parser.DefaultLimits().MaxOutputBytes {
		t.Fatalf("a decompression bomb produced status %s, truncated %v, %d bytes", res.Status, res.Truncated, len(res.Text))
	}
}

func TestOversizedMemberIsRefusedBeforeItIsRead(t *testing.T) {
	lim := parser.DefaultLimits()
	lim.MaxMemberBytes = 1 << 20
	res := parser.Parse("application/zip", writeZip(t, map[string][]byte{"big.txt": make([]byte, 8<<20)}), lim)
	if res.Status != parser.StatusInputOverCap || !strings.Contains(res.Err, "declares") {
		t.Fatalf("an 8 MiB member against a 1 MiB limit: %+v", res)
	}
}

func TestTooManyMembersAreRefused(t *testing.T) {
	lim := parser.DefaultLimits()
	lim.MaxMembers = 2
	res := parser.Parse("application/zip", writeZip(t, map[string][]byte{"a.txt": nil, "b.txt": nil, "c.txt": nil}), lim)
	if res.Status != parser.StatusInputOverCap {
		t.Fatalf("three members against a limit of two: %+v", res)
	}
}

func TestDeepNestingIsRefused(t *testing.T) {
	body := strings.Repeat("[", 5000) + `"x"` + strings.Repeat("]", 5000)
	if res := parser.Parse("application/json", []byte(body), parser.DefaultLimits()); res.Status != parser.StatusDepthExceeded || res.Text != "" {
		t.Fatalf("a 5000-deep body: %+v", res)
	}
}

func TestUnsupportedUndecodableAndMalformed(t *testing.T) {
	cases := []struct {
		mediaType string
		body      []byte
		want      parser.Status
	}{
		{"application/pdf", []byte("%PDF-1.7"), parser.StatusUnsupported},
		{"application/octet-stream", []byte{0, 1, 2}, parser.StatusUndecodable},
		{"application/gzip", []byte("not gzip"), parser.StatusMalformed},
		{"application/zip", []byte("not a zip"), parser.StatusMalformed},
	}
	for _, tc := range cases {
		if res := parser.Parse(tc.mediaType, tc.body, parser.DefaultLimits()); res.Status != tc.want {
			t.Errorf("%s: %+v, want %s", tc.mediaType, res, tc.want)
		}
	}
}

func TestDocumentOverTheSizeLimitIsRefused(t *testing.T) {
	lim := parser.DefaultLimits()
	lim.MaxDocumentBytes = 16
	if res := parser.Parse("text/plain", []byte(strings.Repeat("x", 17)), lim); res.Status != parser.StatusInputOverCap {
		t.Fatalf("an over-limit document: %+v", res)
	}
}

func TestOfficeDocumentTextIsExtracted(t *testing.T) {
	docx := writeZip(t, map[string][]byte{
		"[Content_Types].xml":   []byte(`<Types/>`),
		"word/document.xml":     []byte(`<w:document><w:body><w:p>card 4111 1111 1111 1111</w:p></w:body></w:document>`),
		"word/media/image1.png": {0x89, 'P', 'N', 'G', 0x00, 0xff},
	})
	res := parser.Parse("application/vnd.openxmlformats-officedocument.wordprocessingml.document", docx, parser.DefaultLimits())
	if res.Status != parser.StatusOK || res.Text != "card 4111 1111 1111 1111" {
		t.Fatalf("docx: %+v", res)
	}
}

func TestZipTextMembersAreReadAndBinariesSkipped(t *testing.T) {
	res := parser.Parse("application/zip", writeZip(t, map[string][]byte{
		"notes.txt":  []byte("hello"),
		"blob.bin":   {0xff, 0xfe, 0xfd},
		"data.json":  []byte(`{"a":"b"}`),
		"image.jpeg": {0xff, 0xd8, 0xff},
		"bad.txt":    {0xff, 0x00},
	}), parser.DefaultLimits())
	if res.Status != parser.StatusOK || !strings.Contains(res.Text, "hello") || !strings.Contains(res.Text, "a b") {
		t.Fatalf("zip: %+v", res)
	}
}

func execute(t *testing.T, stdin *bytes.Buffer) parser.Result {
	t.Helper()
	var stdout bytes.Buffer
	if code := parser.Execute(stdin, &stdout, parser.DefaultLimits()); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	res, err := parser.ReadResult(&stdout)
	if err != nil {
		t.Fatalf("the result frame is unreadable: %v", err)
	}
	if _, err := parser.ReadResult(&stdout); err == nil {
		t.Fatal("the child wrote a second result")
	}
	return res
}

func TestExecuteReadsOneDocumentAndWritesOneResult(t *testing.T) {
	var stdin bytes.Buffer
	doc := []byte("first document")
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: int64(len(doc))}, doc); err != nil {
		t.Fatal(err)
	}
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: 7}, []byte("second!")); err != nil {
		t.Fatal(err)
	}
	if res := execute(t, &stdin); res.Status != parser.StatusOK || res.Text != string(doc) {
		t.Fatalf("result: %+v", res)
	}
}

func TestExecuteRefusesAMismatchedDeclaredSize(t *testing.T) {
	var stdin bytes.Buffer
	if err := parser.WriteRequest(&stdin, parser.Header{MediaType: "text/plain", DeclaredSize: 4096}, []byte("short")); err != nil {
		t.Fatal(err)
	}
	if res := execute(t, &stdin); res.Status != parser.StatusInputShort {
		t.Errorf("a wrong declared_size: %+v", res)
	}
}

func TestExecuteRefusesAMalformedHeader(t *testing.T) {
	var stdin bytes.Buffer
	if err := protocol.WriteFrame(&stdin, []byte{0, 0, 0, 0xff}); err != nil {
		t.Fatal(err)
	}
	if res := execute(t, &stdin); res.Status != parser.StatusInputShort {
		t.Errorf("a malformed header: %+v", res)
	}
}

func TestExecuteWithoutARequestExitsWithoutWriting(t *testing.T) {
	var stdout bytes.Buffer
	if code := parser.Execute(&bytes.Buffer{}, &stdout, parser.DefaultLimits()); code != 2 || stdout.Len() != 0 {
		t.Fatalf("exit code %d, %d bytes written", code, stdout.Len())
	}
}

func TestResultFrameVersionIsChecked(t *testing.T) {
	var buf bytes.Buffer
	if err := protocol.WriteFrameVersion(&buf, protocol.Version+1, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ReadResult(&buf); !errors.Is(err, protocol.ErrVersionMismatch) {
		t.Errorf("a mismatched frame returned %v", err)
	}
}

// TestParserCannotReachTheSystem pins the package's whole dependency set: the code that reads an
// attacker-chosen document cannot name os, net, exec or syscall.
func TestParserCannotReachTheSystem(t *testing.T) {
	importcheck.AssertClosed(t, ".",
		"archive/zip", "bytes", "compress/gzip", "encoding/binary", "encoding/json", "io", "strconv", "strings",
		"github.com/shadow-ai-capture/device/classifier-host/norm",
		"github.com/shadow-ai-capture/device/protocol")
}
