package tlsproxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"sync"
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

// bodyBuffer is the M1+ retention of a request body, bounded. An over-cap body is not read
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
