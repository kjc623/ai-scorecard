//go:build !js

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
)

// runServe serves the local request/response channel of §3.4.
//
// Transports (A2/A11 territory — the requirement is a local channel capture-core can reach, and
// the platform primitives are implementation choices):
//
//   - `stdio` (default on Windows): the host is a child of capture-core and speaks the
//     length-prefixed frames on its own stdin/stdout, exactly as the parser child does. This is
//     the portable path and the one this build host can exercise end to end.
//   - `unix` (default elsewhere): a Unix-domain socket under the service's directory, which is
//     what §3.4 names for macOS. It compiles everywhere and runs where the platform has it.
//   - `tcp`: a loopback listener, used by tests and by a deployment that wants a port.
//
// A Windows named pipe is NOT implemented: Go's standard library has no named-pipe listener, the
// offline toolchain cannot fetch one (ADR 0016), and a hand-rolled CreateNamedPipe transport is
// its own review surface. That gap is reported in README.md rather than papered over with a
// loopback socket pretending to be a pipe.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	releaseDir := fs.String("release", "", "signed release directory")
	pubkey := fs.String("pubkey", "", "hex ed25519 release-signing public key")
	transport := fs.String("transport", defaultTransport(), "stdio | unix | tcp")
	addr := fs.String("addr", "", "unix socket path, or tcp host:port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r, err := loadRig(*releaseDir, *pubkey)
	if err != nil {
		return fatalf("serve: %v", err)
	}
	srv := classify.NewServer(r.host)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *transport {
	case "stdio":
		conn := &stdioConn{r: os.Stdin, w: os.Stdout}
		fmt.Fprintf(os.Stderr, "classifier-host: serving on stdio, release %s\n", r.host.Health().Release)
		err = srv.ServeConn(conn)
		if err != nil {
			return fatalf("serve: %v", err)
		}
		return 0
	case "unix", "tcp":
		if *addr == "" {
			return fatalf("serve: --addr is required for transport %s", *transport)
		}
		ln, err := net.Listen(*transport, *addr)
		if err != nil {
			return fatalf("serve: listening on %s %s: %v", *transport, *addr, err)
		}
		defer ln.Close()
		// One writer per connection, bounded concurrency: a flood of connections must not
		// become unbounded goroutines holding a pipeline each.
		sem := make(chan struct{}, 32)
		var wg sync.WaitGroup
		go func() {
			<-ctx.Done()
			ln.Close()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					wg.Wait()
					return 0
				default:
					return fatalf("serve: accept: %v", err)
				}
			}
			select {
			case sem <- struct{}{}:
			default:
				conn.Close() // shed load rather than queue it
				continue
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { <-sem }()
				_ = srv.ServeConn(c)
			}(conn)
		}
	default:
		return fatalf("serve: unknown transport %q", *transport)
	}
}

func defaultTransport() string {
	if runtime.GOOS == "windows" {
		return "stdio"
	}
	return "unix"
}

// stdioConn adapts a reader and a writer into the ReadWriteCloser ServeConn expects.
type stdioConn struct {
	r io.Reader
	w io.Writer
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *stdioConn) Close() error                { return nil }
