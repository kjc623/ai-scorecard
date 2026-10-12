package gemini

import (
	"encoding/json"
	"regexp"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/capture-core/parsers"
)

// uploadPath is the Files API's upload method. The request that starts an upload carries the
// file's metadata; the ones after it, to the same path, carry its bytes.
var uploadPath = regexp.MustCompile(`^/upload/(v1|v1beta)/files$`)

// Upload reads the Files API's uploads. None carries text: the one that starts an upload names
// the file when its metadata gives a display name, and the bytes after it name nothing.
type Upload struct{}

// Match implements parsers.Parser.
func (Upload) Match(h, p string) bool { return h == host && uploadPath.MatchString(p) }

// Version implements parsers.Parser.
func (Upload) Version() string { return version }

// BlockResponse implements parsers.Parser: the API's error shape, as for generateContent.
func (Upload) BlockResponse(message, link string) (int, string, []byte) {
	return Parser{}.BlockResponse(message, link)
}

// Parse implements parsers.Parser.
func (Upload) Parse(body []byte, _ string) (parsers.Result, error) {
	var req struct {
		File *fileData `json:"file"`
	}
	if json.Unmarshal(body, &req) != nil || req.File == nil || req.File.name() == "" {
		return parsers.Result{}, parsers.ErrNoText
	}
	return parsers.Result{Attachments: []dedup.Attachment{{Name: req.File.name()}}}, parsers.ErrNoText
}
