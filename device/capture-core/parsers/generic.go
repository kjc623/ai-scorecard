package parsers

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
	"github.com/shadow-ai-capture/device/protocol"
)

// Generic reads a JSON body of no known target: a top-level `prompt`, else the last user-role
// message of `messages[]`, else `input`. A body it cannot interpret returns ErrNoText so the
// observation degrades instead of guessing which characters the user authored. It names the files
// such a message attaches in the OpenAI and Anthropic part shapes, and the files a multipart form
// uploads, whose text it does not read.
type Generic struct{}

// Match implements Parser: the generic parser reads any destination.
func (Generic) Match(string, string) bool { return true }

// Version implements Parser.
func (Generic) Version() string { return "1" }

// BlockResponse implements Parser: plain text, because a destination of no known target has no
// error shape its client is known to display.
func (Generic) BlockResponse(message, link string) (int, string, []byte) {
	return http.StatusForbidden, "text/plain; charset=utf-8", []byte(BlockText(message, link))
}

// Parse implements Parser.
func (Generic) Parse(payload []byte, mediaType string) (Result, error) {
	if len(payload) == 0 {
		return Result{}, ErrNoText
	}
	if files, ok := formFiles(payload, mediaType); ok {
		return Result{Attachments: files}, ErrNoText
	}
	var body struct {
		Prompt   string `json:"prompt"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return Result{}, ErrNoText
	}
	if body.Prompt != "" {
		return Result{Text: body.Prompt, Shape: "prompt"}, nil
	}
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i].Role != "user" {
			continue
		}
		text, files, ok := decodeContent(body.Messages[i].Content)
		if ok {
			return Result{Text: text, Attachments: files, Shape: "messages"}, nil
		}
		if len(files) > 0 {
			return Result{Attachments: files}, ErrNoText
		}
	}
	if len(body.Input) > 0 {
		if text, files, ok := decodeContent(body.Input); ok {
			return Result{Text: text, Attachments: files, Shape: "input"}, nil
		}
	}
	return Result{}, ErrNoText
}

// contentPart is one part of a content array, in the shapes this parser reads: text parts, and
// the file parts of the OpenAI (`file`, `input_file`) and Anthropic (`document`) APIs.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	File *struct {
		Filename string `json:"filename"`
	} `json:"file"`
	Filename string `json:"filename"`
	Title    string `json:"title"`
}

func (p contentPart) fileName() string {
	switch {
	case p.Type == "file" && p.File != nil:
		return p.File.Filename
	case p.Type == "input_file":
		return p.Filename
	case p.Type == "document":
		return p.Title
	}
	return ""
}

// decodeContent accepts both the string form and the content-part array form, because both are
// in the wild and both address the same authored text. It returns the names of the files the
// parts attach whether or not they carry text.
func decodeContent(raw json.RawMessage) (string, []dedup.Attachment, bool) {
	if len(raw) == 0 {
		return "", nil, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, true
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, false
	}
	var out string
	var files []dedup.Attachment
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" {
			if out != "" {
				out += " "
			}
			out += p.Text
		}
		if n := p.fileName(); n != "" {
			files = append(files, dedup.Attachment{Name: n})
		}
	}
	return out, files, out != ""
}

// formFiles names the files a multipart/form-data body uploads, with each one's media type and
// size, and reports whether the body is such a form. A body cut at the cap names the files whose
// headers it holds.
func formFiles(payload []byte, mediaType string) ([]dedup.Attachment, bool) {
	mt, params, err := mime.ParseMediaType(mediaType)
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, false
	}
	r := multipart.NewReader(bytes.NewReader(payload), params["boundary"])
	var files []dedup.Attachment
	for len(files) < protocol.MaxAttachments {
		part, err := r.NextPart()
		if err != nil {
			break
		}
		// The base name, on either separator: a client may send the file's path as its name.
		name := part.FileName()
		name = name[strings.LastIndexAny(name, `/\`)+1:]
		if name != "" {
			att := dedup.Attachment{Name: name, MediaType: part.Header.Get("Content-Type")}
			if n, err := io.Copy(io.Discard, part); err == nil {
				att.SizeBytes = n
			}
			files = append(files, att)
		}
		_ = part.Close()
	}
	return files, true
}
