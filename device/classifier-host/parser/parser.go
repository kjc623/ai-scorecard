// Package parser is the document-parsing child: it reads one framed document from its input,
// extracts the text, writes one framed result and returns. The classifier host runs it in a
// separate process per document (package parser/isolation), so a hostile document can at worst
// kill that process.
//
// The package imports no os, net or exec: the code that reads an attacker-chosen document cannot
// open a file, a socket or a process. Every limit is checked before the work that would cross it,
// so a decompression bomb, deep nesting or an oversized archive member becomes a status rather
// than an allocation.
package parser

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/shadow-ai-capture/device/classifier-host/norm"
	"github.com/shadow-ai-capture/device/protocol"
)

// Status is the outcome of one parse.
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

// Limits are the child's document limits.
type Limits struct {
	// MaxDocumentBytes is the largest document parsed.
	MaxDocumentBytes int64
	// MaxDecodedBytes bounds what a gzip body may expand to.
	MaxDecodedBytes int64
	// MaxOutputBytes bounds the extracted text.
	MaxOutputBytes int
	// MaxDepth bounds nesting in a structured member.
	MaxDepth int
	// MaxMembers bounds the members an archive may declare.
	MaxMembers int
	// MaxMemberBytes bounds one archive member's declared uncompressed size.
	MaxMemberBytes int64
	// MaxTokens bounds the tokens read from a structured member.
	MaxTokens int
}

// DefaultLimits are the limits the child runs with.
func DefaultLimits() Limits {
	return Limits{
		MaxDocumentBytes: 32 << 20,
		MaxDecodedBytes:  64 << 20,
		MaxOutputBytes:   1 << 20,
		MaxDepth:         64,
		MaxMembers:       64,
		MaxMemberBytes:   16 << 20,
		MaxTokens:        20000,
	}
}

// Result is the child's answer.
type Result struct {
	Status    Status `json:"status"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Err       string `json:"error,omitempty"`
}

// Header precedes the document bytes in a request.
type Header struct {
	MediaType    string `json:"media_type"`
	DeclaredSize int64  `json:"declared_size"`
}

// EncodeRequest builds a request payload: a big-endian uint32 header length, the header JSON,
// then the document bytes.
func EncodeRequest(h Header, doc []byte) ([]byte, error) {
	head, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 4, 4+len(head)+len(doc))
	binary.BigEndian.PutUint32(payload, uint32(len(head)))
	payload = append(payload, head...)
	return append(payload, doc...), nil
}

// DecodeRequest splits a request payload into its header and document.
func DecodeRequest(payload []byte) (Header, []byte, error) {
	var h Header
	if len(payload) < 4 {
		return h, nil, io.ErrUnexpectedEOF
	}
	n := int64(binary.BigEndian.Uint32(payload))
	if 4+n > int64(len(payload)) {
		return h, nil, io.ErrUnexpectedEOF
	}
	if err := json.Unmarshal(payload[4:4+n], &h); err != nil {
		return h, nil, err
	}
	return h, payload[4+n:], nil
}

// WriteRequest writes a framed request.
func WriteRequest(w io.Writer, h Header, doc []byte) error {
	payload, err := EncodeRequest(h, doc)
	if err != nil {
		return err
	}
	return protocol.WriteFrame(w, payload)
}

// ReadResult reads a framed result.
func ReadResult(r io.Reader) (Result, error) {
	var res Result
	payload, err := protocol.ReadFrameChecked(r)
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(payload, &res)
	return res, err
}

func writeResult(w io.Writer, res Result) error {
	payload, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return protocol.WriteFrame(w, payload)
}

// Execute is the child process: it reads one request, parses it, writes one result and returns
// the exit code. An unreadable request exits 2 without writing; every other outcome is reported
// in the result.
func Execute(stdin io.Reader, stdout io.Writer, lim Limits) int {
	payload, err := protocol.ReadFrameChecked(stdin)
	if err != nil {
		return 2
	}
	h, doc, err := DecodeRequest(payload)
	var res Result
	switch {
	case err != nil:
		res = Result{Status: StatusInputShort, Err: "request header is malformed: " + err.Error()}
	case h.DeclaredSize != int64(len(doc)):
		res = Result{Status: StatusInputShort, Err: "declared_size does not match the bytes received"}
	default:
		res = Parse(h.MediaType, doc, lim)
	}
	if err := writeResult(stdout, res); err != nil {
		return 1
	}
	return 0
}

// Parse extracts the text of one document.
func Parse(mediaType string, doc []byte, lim Limits) Result {
	if int64(len(doc)) > lim.MaxDocumentBytes {
		return Result{Status: StatusInputOverCap, Err: "document is over the parser's size limit"}
	}
	mt := strings.ToLower(mediaType)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	mt = strings.TrimSpace(mt)
	switch {
	case isZIP(mt):
		return parseZIP(mt, doc, lim)
	case mt == "application/gzip" || mt == "application/x-gzip":
		return parseGZIP(doc, lim)
	case mt == "application/pdf" || mt == "application/msword":
		return Result{Status: StatusUnsupported, Err: "no parser for " + mt}
	default:
		return extractText(mt, doc, lim)
	}
}

// parseGZIP decompresses at most the output limit and extracts text from it.
func parseGZIP(doc []byte, lim Limits) Result {
	zr, err := gzip.NewReader(bytes.NewReader(doc))
	if err != nil {
		return Result{Status: StatusMalformed, Err: "gzip: " + err.Error()}
	}
	defer zr.Close()
	limit := min(int64(lim.MaxOutputBytes), lim.MaxDecodedBytes)
	body, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return Result{Status: StatusMalformed, Err: "gzip: " + err.Error()}
	}
	truncated := int64(len(body)) > limit
	if truncated {
		body = body[:limit]
	}
	res := extractText("text/plain", body, lim)
	if truncated && res.Status == StatusOK {
		res.Status, res.Truncated = StatusOutputCap, true
		res.Err = "the decompressed body reached the parser's output limit"
	}
	return res
}

// memberPrefixes maps an office document type to the archive members that carry its text. A
// plain zip falls back to members with a text extension.
var memberPrefixes = map[string][]string{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {"word/"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {"xl/"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {"ppt/"},
	"application/vnd.oasis.opendocument.text":                                   {"content.xml", "styles.xml"},
	"application/vnd.oasis.opendocument.spreadsheet":                            {"content.xml", "styles.xml"},
}

var textExtensions = []string{".txt", ".md", ".csv", ".json", ".xml", ".html", ".htm", ".log", ".yaml", ".yml", ".tsv", ".rst"}

// parseZIP extracts the text of an archive's selected members, checking each member's declared
// size before opening it.
func parseZIP(mt string, doc []byte, lim Limits) Result {
	zr, err := zip.NewReader(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		return Result{Status: StatusMalformed, Err: "zip: " + err.Error()}
	}
	if len(zr.File) > lim.MaxMembers {
		return Result{Status: StatusInputOverCap, Err: "archive declares more members than the parser reads"}
	}
	var b strings.Builder
	truncated := false
	for _, f := range zr.File {
		if !memberSelected(f.Name, memberPrefixes[mt]) {
			continue
		}
		if f.UncompressedSize64 > uint64(lim.MaxMemberBytes) {
			return Result{Status: StatusInputOverCap,
				Err: "archive member declares " + strconv.FormatUint(f.UncompressedSize64, 10) + " bytes, over the parser's member limit"}
		}
		remaining := lim.MaxOutputBytes - b.Len()
		if remaining <= 0 {
			truncated = true
			break
		}
		rc, err := f.Open()
		if err != nil {
			return Result{Status: StatusMalformed, Err: "zip member " + f.Name + ": " + err.Error()}
		}
		chunk, err := io.ReadAll(io.LimitReader(rc, int64(remaining)+1))
		rc.Close()
		if err != nil {
			return Result{Status: StatusMalformed, Err: "zip member " + f.Name + ": " + err.Error()}
		}
		if len(chunk) > remaining {
			truncated = true
		}
		member := extractText(memberMediaType(f.Name), chunk, lim)
		if member.Truncated {
			truncated = true
		} else if member.Status != StatusOK {
			continue // a binary or unreadable member contributes nothing
		}
		if member.Text != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(member.Text)
		}
	}
	text := b.String()
	if truncated {
		if len(text) > lim.MaxOutputBytes {
			text = text[:lim.MaxOutputBytes]
		}
		return Result{Status: StatusOutputCap, Text: text, Truncated: true, Err: "the extracted text reached the parser's output limit"}
	}
	return Result{Status: StatusOK, Text: text}
}

func memberSelected(name string, prefixes []string) bool {
	if strings.HasSuffix(name, "/") {
		return false
	}
	if len(prefixes) > 0 {
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
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

func memberMediaType(name string) string {
	switch lower := strings.ToLower(name); {
	case strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".rels"):
		return "application/xml"
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	default:
		return "text/plain"
	}
}

// extractText runs the classifier's own normalisation over a text body, so a document's text
// means the same thing whether the host or the child read it.
func extractText(mt string, doc []byte, lim Limits) Result {
	out, err := norm.Normalise(mt, doc, norm.Limits{
		MaxInputBytes: len(doc),
		MaxTextBytes:  lim.MaxOutputBytes,
		MaxTokens:     lim.MaxTokens,
		MaxDepth:      lim.MaxDepth,
	})
	switch {
	case err != nil:
		return Result{Status: StatusUndecodable, Err: err.Error()}
	case out.Pathological == "nesting":
		return Result{Status: StatusDepthExceeded, Err: "the document nests deeper than the parser's depth limit"}
	case out.TruncationAffectsRules:
		return Result{Status: StatusOutputCap, Text: out.Text, Truncated: true, Err: "the extracted text reached the parser's output limit"}
	default:
		return Result{Status: StatusOK, Text: out.Text}
	}
}

func isZIP(mt string) bool {
	_, office := memberPrefixes[mt]
	return office || mt == "application/zip" || mt == "application/x-zip-compressed"
}
