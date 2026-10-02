// Package parser is the child side of docs/01-collectors.md §10: one document in, extracted text
// and a status code out.
//
// §10's table fixes this package's contract:
//
//   - **Placement and lifetime**: a child of classifier-host, one per document, spawned for the
//     parse and reaped after it. Execute() reads exactly one request and writes exactly one
//     result; it never loops for a second document.
//   - **Input**: document bytes over a pipe. The child has no path to the file's location and
//     never opens a file itself, so it cannot be pointed at an unrelated document. Nothing in
//     this package imports os, net or os/exec — asserted by test — so "the child cannot reach
//     the network, the spool, the CA key or the credential store" is a property of the import
//     graph rather than of the platform sandbox alone. (The platform sandbox of §10's
//     Privileges row still applies on top; see parser/isolation.)
//   - **Output**: extracted text, offsets, a status code — never a path, a handle or a command.
//   - **Bounded**: every cap is checked before the work that would cross it, so a gzip bomb, a
//     deeply nested body, an oversized declared member or an undecodable container becomes a
//     status code rather than an allocation.
//
// The parent enforces the memory cap, the wall-clock timeout and the kill (parser/isolation);
// this package's caps are the ones that make the *common* hostile document cheap, so the parent's
// kill is the last line of defence rather than the first.
package parser

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"runtime"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/protocol"
)

// Status is the bounded result-code set of §10's Output row.
type Status string

const (
	StatusOK            Status = "ok"
	StatusUnsupported   Status = "unsupported_media_type"
	StatusInputOverCap  Status = "input_over_cap"
	StatusOutputCap     Status = "output_capped"
	StatusDepthExceeded Status = "nesting_over_cap"
	StatusUndecodable   Status = "undecodable"
	StatusInputShort    Status = "input_short"
	StatusMalformed     Status = "malformed_document"
)

// Limits are the child's own caps. They are parameters (A12), not design.
type Limits struct {
	// MaxDeclaredBytes is the largest document the child will look at. The parent refuses a
	// larger one before spawning; this is defence in depth.
	MaxDeclaredBytes int64
	// MaxDecodedBytes bounds a container's expansion (gzip/zip), which is where a
	// decompression bomb would otherwise allocate.
	MaxDecodedBytes int64
	// MaxOutputBytes bounds the extracted text: §10's "output cap", so a 500-page document does
	// not become a 500-page label input.
	MaxOutputBytes int
	// MaxDepth bounds nesting in a structured member.
	MaxDepth int
	// MaxMembers bounds the archive members considered.
	MaxMembers int
	// MaxEntryBytes bounds one archive member's declared uncompressed size, checked *before*
	// the member is read.
	MaxEntryBytes int64
	// MaxTokens bounds the tokens read from a structured member.
	MaxTokens int
}

// DefaultLimits: A12's "memory cap in the tens of megabytes, a timeout in the low hundreds of
// milliseconds, an output cap in the low megabytes" expressed as document-side caps.
func DefaultLimits() Limits {
	return Limits{
		MaxDeclaredBytes: 32 << 20,
		MaxDecodedBytes:  64 << 20,
		MaxOutputBytes:   1 << 20,
		MaxDepth:         64,
		MaxMembers:       64,
		MaxEntryBytes:    16 << 20,
		MaxTokens:        20000,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxDeclaredBytes <= 0 {
		l.MaxDeclaredBytes = d.MaxDeclaredBytes
	}
	if l.MaxDecodedBytes <= 0 {
		l.MaxDecodedBytes = d.MaxDecodedBytes
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxMembers <= 0 {
		l.MaxMembers = d.MaxMembers
	}
	if l.MaxEntryBytes <= 0 {
		l.MaxEntryBytes = d.MaxEntryBytes
	}
	if l.MaxTokens <= 0 {
		l.MaxTokens = d.MaxTokens
	}
	return l
}

// Offset is one extracted segment's span inside Result.Text. §10's Output row asks for offsets;
// they are what lets a later label point at a region without keeping the document.
type Offset struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Label string `json:"label,omitempty"`
}

// Result is the child's answer.
type Result struct {
	Status    Status   `json:"status"`
	Text      string   `json:"text,omitempty"`
	Offsets   []Offset `json:"offsets,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Err       string   `json:"error,omitempty"`
	BytesIn   int      `json:"bytes_in"`
	BytesOut  int      `json:"bytes_out"`
	MaxDepth  int      `json:"max_depth,omitempty"`
	// PeakAllocBytes is the child's own reported peak heap. It is informational: the parent's
	// number is the one that counts (§10: "a limit a child enforces on itself is not a limit").
	PeakAllocBytes int64 `json:"peak_alloc_bytes,omitempty"`
}

// OK reports whether the text is usable.
func (r Result) OK() bool { return r.Status == StatusOK }

// Header is the per-document request header the parent sends before the bytes.
type Header struct {
	MediaType    string `json:"media_type"`
	DeclaredSize int64  `json:"declared_size"`
	Digest       string `json:"digest,omitempty"`
	// Limits carries the parent's child-side caps, so the process the parent configured is the
	// process whose caps are in force. Nil means the child's defaults.
	Limits *Limits `json:"limits,omitempty"`
}

const headerLenBytes = 4

// EncodeRequest builds the single-frame payload the child reads: a big-endian uint32 header
// length, the header JSON, then exactly the document bytes. One frame per document is §10's "one
// document, one process" made visible in the wire shape.
func EncodeRequest(h Header, doc []byte) ([]byte, error) {
	head, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, headerLenBytes+len(head)+len(doc))
	binary.BigEndian.PutUint32(payload[:headerLenBytes], uint32(len(head)))
	copy(payload[headerLenBytes:], head)
	copy(payload[headerLenBytes+len(head):], doc)
	return payload, nil
}

// DecodeRequest splits a request payload back into its header and document.
func DecodeRequest(payload []byte) (Header, []byte, error) {
	var h Header
	if len(payload) < headerLenBytes {
		return h, nil, io.ErrUnexpectedEOF
	}
	n := int(binary.BigEndian.Uint32(payload[:headerLenBytes]))
	if n < 0 || headerLenBytes+n > len(payload) {
		return h, nil, io.ErrUnexpectedEOF
	}
	if err := json.Unmarshal(payload[headerLenBytes:headerLenBytes+n], &h); err != nil {
		return h, nil, err
	}
	return h, payload[headerLenBytes+n:], nil
}

// WriteRequest writes the framed request to the parent's pipe.
func WriteRequest(w io.Writer, h Header, doc []byte) error {
	payload, err := EncodeRequest(h, doc)
	if err != nil {
		return err
	}
	return protocol.WriteFrame(w, payload)
}

// ReadResult reads the child's framed result.
func ReadResult(r io.Reader) (Result, error) {
	var res Result
	payload, err := protocol.ReadFrameChecked(r)
	if err != nil {
		return res, err
	}
	if err := json.Unmarshal(payload, &res); err != nil {
		return res, err
	}
	return res, nil
}

// WriteResult writes the child's framed result.
func WriteResult(w io.Writer, res Result) error {
	payload, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return protocol.WriteFrame(w, payload)
}

// Execute is the child's whole life: read one request, parse one document, write one result.
//
// It returns a process exit code. A read failure returns 2 without writing (there is no request
// to answer); everything after that is reported *in* the result frame, because §10 makes a
// malformed result indistinguishable from a crash to the parent — reporting a status is more
// useful than exiting.
//
// hook, when non-nil, runs after the request is read and before parsing. It exists only so the
// isolation tests can drive a child that exceeds its memory cap, hangs, exits or writes garbage;
// cmd/classifier-host wires it to CLASSIFIER_HOST_TESTHOOK_* environment variables and nothing
// in production sets them (see README).
func Execute(stdin io.Reader, stdout io.Writer, lim Limits, hook func()) int {
	payload, err := protocol.ReadFrameChecked(stdin)
	if err != nil {
		return 2
	}
	h, doc, err := DecodeRequest(payload)
	if err != nil {
		_ = WriteResult(stdout, Result{Status: StatusInputShort, Err: "request header is malformed: " + err.Error()})
		return 0
	}
	if hook != nil {
		hook()
	}
	// The parent's child-side caps win over this process's defaults: the parent configured the
	// child it spawned, and §10's "a limit a child enforces on itself is not a limit" applies to
	// which numbers are in force, not only to who does the killing.
	if h.Limits != nil {
		lim = h.Limits.withDefaults()
	}
	res := Parse(h.MediaType, doc, lim)
	res.PeakAllocBytes = HeapBytes()
	if h.DeclaredSize != int64(len(doc)) {
		res = Result{Status: StatusInputShort, BytesIn: len(doc),
			Err: "declared_size does not match the bytes received"}
	}
	_ = WriteResult(stdout, res)
	return 0
}

// Parse is the child's document handling, pure over bytes.
func Parse(mediaType string, doc []byte, lim Limits) Result {
	lim = lim.withDefaults()
	res := Result{BytesIn: len(doc), MaxDepth: 0}
	if int64(len(doc)) > lim.MaxDeclaredBytes {
		res.Status = StatusInputOverCap
		res.Err = "document is over the parser's declared-size cap"
		return res
	}
	mt := normMediaType(mediaType)
	switch {
	case isZIP(mt):
		return parseZIP(mt, doc, lim, res)
	case mt == "application/gzip" || mt == "application/x-gzip":
		return parseGZIP(doc, lim, res)
	case mt == "application/pdf" || mt == "application/msword":
		// No PDF or legacy-binary-Word parser ships in the standard library, and pretending to
		// parse them would report "no matching content" for content nobody read. §10's
		// per-format coverage row is how this becomes visible rather than silent.
		res.Status = StatusUnsupported
		res.Err = "no parser for " + mt + " ships in this build"
		return res
	default:
		return extractText(mt, doc, lim, res)
	}
}

// parseGZIP decompresses with a bounded read. A decompression bomb cannot allocate: the reader
// is capped at the output limit and the copy stops there.
func parseGZIP(doc []byte, lim Limits, res Result) Result {
	zr, err := gzip.NewReader(bytes.NewReader(doc))
	if err != nil {
		res.Status = StatusMalformed
		res.Err = "gzip: " + err.Error()
		return res
	}
	defer zr.Close()
	cap := lim.MaxOutputBytes
	if int64(cap) > lim.MaxDecodedBytes {
		cap = int(lim.MaxDecodedBytes)
	}
	buf := make([]byte, 0, min(cap, 64<<10))
	n, err := io.Copy(appendWriter{&buf}, io.LimitReader(zr, int64(cap)+1))
	if err != nil {
		res.Status = StatusMalformed
		res.Err = "gzip: " + err.Error()
		return res
	}
	_ = n
	if len(buf) > cap {
		buf = buf[:cap]
		res.Truncated = true
	}
	inner := res
	inner = extractText("text/plain", buf, lim, inner)
	inner.BytesIn = res.BytesIn
	if res.Truncated {
		// §10's output cap: the status must say the cap cut the result, not merely that the text
		// happened to extract cleanly from the prefix.
		inner.Truncated = true
		if inner.Status == StatusOK {
			inner.Status = StatusOutputCap
			inner.Err = "the decompressed body reached the parser's output cap"
		}
	}
	return inner
}

// memberSelectors maps a container media type to the members that carry its text. A generic zip
// falls back to text-ish extensions.
var memberSelectors = map[string][]string{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {"word/"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {"xl/"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {"ppt/"},
	"application/vnd.oasis.opendocument.text":                                   {"content.xml", "styles.xml"},
	"application/vnd.oasis.opendocument.spreadsheet":                            {"content.xml", "styles.xml"},
}

var textExtensions = []string{".txt", ".md", ".csv", ".json", ".xml", ".html", ".htm", ".log", ".yaml", ".yml", ".tsv", ".rst"}

// parseZIP walks the archive's selected members with every cap checked before the read.
func parseZIP(mt string, doc []byte, lim Limits, res Result) Result {
	zr, err := zip.NewReader(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		res.Status = StatusMalformed
		res.Err = "zip: " + err.Error()
		return res
	}
	if len(zr.File) > lim.MaxMembers {
		res.Status = StatusInputOverCap
		res.Err = "archive declares more members than the parser will consider"
		return res
	}
	selectors := memberSelectors[mt]
	var b strings.Builder
	var offsets []Offset
	truncated := false
	for _, f := range zr.File {
		if !memberSelected(f.Name, selectors) {
			continue
		}
		// The declared size is checked *before* the member is opened: this is the case a
		// hostile archive would otherwise use to make the parser allocate 4 GB for a 4 KB file.
		if f.UncompressedSize64 > uint64(lim.MaxEntryBytes) {
			return Result{
				Status:  StatusInputOverCap,
				BytesIn: res.BytesIn,
				Err:     "archive member declares " + itoa64(int64(f.UncompressedSize64)) + " bytes, over the parser's per-member cap",
			}
		}
		if b.Len() >= lim.MaxOutputBytes {
			truncated = true
			break
		}
		rc, err := f.Open()
		if err != nil {
			return Result{Status: StatusMalformed, BytesIn: res.BytesIn, Err: "zip member " + f.Name + ": " + err.Error()}
		}
		remaining := int64(lim.MaxOutputBytes-b.Len()) + 1
		chunk, err := io.ReadAll(io.LimitReader(rc, remaining))
		rc.Close()
		if err != nil {
			return Result{Status: StatusMalformed, BytesIn: res.BytesIn, Err: "zip member " + f.Name + ": " + err.Error()}
		}
		if len(chunk) > int(remaining)-1 {
			truncated = true
		}
		text, st, memberTruncated := memberText(f.Name, chunk, lim)
		if st != "" {
			continue // a binary or unreadable member is skipped, and the skip is not silent
		}
		if memberTruncated {
			truncated = true
		}
		if text == "" {
			continue
		}
		start := b.Len()
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(text)
		offsets = append(offsets, Offset{Start: start, End: b.Len(), Label: f.Name})
	}
	res.Text = b.String()
	res.BytesOut = len(res.Text)
	res.Offsets = offsets
	res.Truncated = res.Truncated || truncated
	if res.Truncated {
		res.Status = StatusOutputCap
		res.Err = "the extracted text reached the output cap"
		if len(res.Text) > lim.MaxOutputBytes {
			res.Text = res.Text[:lim.MaxOutputBytes]
		}
		return res
	}
	res.Status = StatusOK
	return res
}

func memberSelected(name string, selectors []string) bool {
	if strings.HasSuffix(name, "/") {
		return false
	}
	if len(selectors) > 0 {
		for _, s := range selectors {
			if strings.HasPrefix(name, s) {
				return strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".rels")
			}
		}
		return false
	}
	lower := strings.ToLower(name)
	for _, ext := range textExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// memberText extracts one member's text. A non-empty status means the member cannot contribute,
// which the caller skips rather than failing the whole document; truncated reports that the
// output cap cut the member short, which the caller must surface as `parser_output_cap`.
func memberText(name string, chunk []byte, lim Limits) (text string, fatal Status, truncated bool) {
	mt := "text/plain"
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".rels"):
		mt = "application/xml"
	case strings.HasSuffix(lower, ".json"):
		mt = "application/json"
	}
	res := extractText(mt, chunk, lim, Result{})
	if res.Truncated {
		return res.Text, "", true
	}
	if res.Status != StatusOK {
		return "", res.Status, false
	}
	return res.Text, "", false
}

// extractText runs the bounded text extraction shared with the classifier's normalise stage. The
// parser and the host must agree on what "text" means or a document's labels would depend on
// which side looked at it, and norm is already linear and depth-bounded by construction.
func extractText(mt string, doc []byte, lim Limits, res Result) Result {
	out, err := norm.Normalise(mt, doc, norm.Limits{
		MaxInputBytes: int(min(lim.MaxDeclaredBytes, int64(len(doc))+1)),
		MaxTextBytes:  lim.MaxOutputBytes,
		MaxJSONTokens: lim.MaxTokens,
		MaxDepth:      lim.MaxDepth,
	})
	if err != nil {
		res.Status = StatusUndecodable
		res.Err = err.Error()
		return res
	}
	res.Text = out.Text
	res.BytesOut = len(res.Text)
	switch {
	case out.Pathological == "nesting":
		res.Status = StatusDepthExceeded
		res.Err = "the document nests deeper than the parser's depth cap"
		res.Text = ""
		return res
	case out.Pathological != "" || out.TruncationAffectsRules:
		res.Status = StatusOutputCap
		res.Err = "the extracted text reached the parser's output cap"
		res.Truncated = true
		return res
	default:
		res.Status = StatusOK
		return res
	}
}

func normMediaType(mt string) string {
	m := strings.ToLower(strings.TrimSpace(mt))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

func isZIP(mt string) bool {
	switch mt {
	case "application/zip", "application/x-zip-compressed",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.oasis.opendocument.text",
		"application/vnd.oasis.opendocument.spreadsheet":
		return true
	}
	return false
}

// appendWriter appends into a byte slice without a second copy. Deliberately not exported.
type appendWriter struct{ b *[]byte }

func (w appendWriter) Write(p []byte) (int, error) {
	*w.b = append(*w.b, p...)
	return len(p), nil
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// HeapBytes reports the process's current heap footprint. Informational only: §10 is explicit
// that a limit the child enforces on itself is not a limit, so the parent's figure is the one
// that counts.
func HeapBytes() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapSys)
}
