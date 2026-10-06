package classifierlink

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// ChildDialer returns a Dialer that starts the classifier host as a child process and speaks
// frames on its stdin and stdout. The host's lifetime is the connection's: closing it ends the
// child, and a child that dies is replaced on the next dial because Classify re-dials a dropped
// connection. stderr receives the child's own log lines; nil discards them.
func ChildDialer(exe string, args []string, stderr io.Writer) Dialer {
	return func(context.Context) (net.Conn, error) {
		// Not CommandContext: the context bounds the dial, and the child must outlive it.
		cmd := exec.Command(exe, args...)
		cmd.Stderr = stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("classifierlink: child stdin: %w", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("classifierlink: child stdout: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("classifierlink: starting %s: %w", exe, err)
		}
		return &childConn{cmd: cmd, stdin: stdin, stdout: stdout, name: exe}, nil
	}
}

// childConn adapts a child's stdin/stdout to net.Conn. Deadlines are not supported on an anonymous
// pipe, so they are accepted and ignored: Classify bounds every request with its own timer, and a
// timed-out request closes the connection, which kills the child.
type childConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	name   string

	once sync.Once
}

func (c *childConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *childConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close ends the child: stdin EOF is its normal end of connection, and the kill covers a hung one.
func (c *childConn) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	})
	return nil
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func (c *childConn) LocalAddr() net.Addr                { return pipeAddr(c.name) }
func (c *childConn) RemoteAddr() net.Addr               { return pipeAddr(c.name) }
func (c *childConn) SetDeadline(t time.Time) error      { return nil }
func (c *childConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *childConn) SetWriteDeadline(t time.Time) error { return nil }
