package tlsproxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"time"
)

func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// idleConn is a connection whose deadline moves on with each read and write, so an exchange is cut
// when it stalls for idle, never because a response streams for long.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(b []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c idleConn) Write(b []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}

// idle is conn with the proxy's idle deadline: four dial timeouts without a byte either way.
func (p *Provider) idle(conn net.Conn) idleConn {
	_ = conn.SetDeadline(time.Now().Add(4 * p.cfg.DialTimeout))
	return idleConn{Conn: conn, idle: 4 * p.cfg.DialTimeout}
}

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

// bodyBuffer is the M1+ retention of a request body, bounded. The body is held before anything is
// forwarded, so the rules can be decided over its classification. An over-cap body is not read
// into memory beyond the cap: the proxy records size_bytes and a digest of the first N bytes,
// classifies nothing and emits with `confidence: degraded`, never a silent "clean".
type bodyBuffer struct {
	data []byte // at most cap+1 bytes: one past the cap tells an over-cap body
	cap  int64
}

// readBody reads r until it ends or passes cap.
func readBody(r io.Reader, cap int64) (*bodyBuffer, error) {
	data, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, err
	}
	return &bodyBuffer{data: data, cap: cap}, nil
}

// Read implements core.ContentReader; only ever called when the resolved mode permits reading.
func (b *bodyBuffer) Read(context.Context) ([]byte, error) {
	held := b.data
	if b.overCap() {
		held = held[:b.cap]
	}
	return bytes.Clone(held), nil
}

func (b *bodyBuffer) overCap() bool { return int64(len(b.data)) > b.cap }

// replay is the whole body for forwarding: the bytes already read, then the rest of r.
func (b *bodyBuffer) replay(rest io.Reader) io.Reader {
	return io.MultiReader(bytes.NewReader(b.data), rest)
}
