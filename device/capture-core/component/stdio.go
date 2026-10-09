package component

import (
	"io"
	"net"
	"sync"
	"time"
)

// stdioConn is a child's stdin and stdout as a net.Conn. An anonymous pipe has no deadlines, so
// they are accepted and ignored: a caller bounds its own requests, and closes the connection, which
// kills the child, only when the child stops answering.
type stdioConn struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	name   string
	kill   func()

	once sync.Once
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close ends the child: stdin EOF is its normal end of connection, and the kill covers a hung one.
// The supervisor then starts the next one.
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		c.kill()
	})
	return nil
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func (c *stdioConn) LocalAddr() net.Addr              { return pipeAddr(c.name) }
func (c *stdioConn) RemoteAddr() net.Addr             { return pipeAddr(c.name) }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }
