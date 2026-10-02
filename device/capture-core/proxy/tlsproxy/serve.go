package tlsproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/shadow-ai-capture/device/capture-core/dedup"
)

func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// countingReader counts body bytes as they stream. Counting is not reading content: size_bytes is
// available at every mode, including M0.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// bodyBuffer is the M1+ retention of a request body, bounded. §5.3: an over-cap body is not read
// into memory — the proxy records size_bytes and a digest of the first N bytes, classifies
// nothing and emits with `confidence: degraded`, never a silent "clean".
type bodyBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	cap  int64
	over bool
}

func newBodyBuffer(cap int64) *bodyBuffer { return &bodyBuffer{cap: cap} }

func (b *bodyBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over {
		return len(p), nil
	}
	room := b.cap - int64(b.buf.Len())
	if room <= 0 {
		b.over = true
		return len(p), nil
	}
	if int64(len(p)) > room {
		b.buf.Write(p[:room])
		b.over = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// Read implements core.ContentReader; only ever called when the resolved mode permits reading.
func (b *bodyBuffer) Read(context.Context) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	return out, nil
}

func (b *bodyBuffer) overCap() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over
}

// JSONExtractor is C1 for a JSON request body: the last user-role message of `messages[]`, or a
// top-level `prompt`. Unrecognised bodies return an error so the observation degrades to the
// tier S surrogate rather than guessing which characters the user authored.
type JSONExtractor struct{}

// Extract implements core.Extractor.
func (JSONExtractor) Extract(payload []byte, _ string) (string, []dedup.Attachment, error) {
	if len(payload) == 0 {
		return "", nil, errNoExtraction
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
		return "", nil, errNoExtraction
	}
	if body.Prompt != "" {
		return body.Prompt, nil, nil
	}
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i].Role != "user" {
			continue
		}
		if text, ok := decodeContent(body.Messages[i].Content); ok {
			return text, nil, nil
		}
	}
	if len(body.Input) > 0 {
		if text, ok := decodeContent(body.Input); ok {
			return text, nil, nil
		}
	}
	return "", nil, errNoExtraction
}

func decodeContent(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		out := ""
		for _, p := range parts {
			if p.Type == "text" || p.Type == "input_text" {
				if out != "" {
					out += " "
				}
				out += p.Text
			}
		}
		if out != "" {
			return out, true
		}
	}
	return "", false
}

type errExtraction string

func (e errExtraction) Error() string { return string(e) }

var errNoExtraction = errExtraction("tlsproxy: no user-authored segment identifiable in this body")
