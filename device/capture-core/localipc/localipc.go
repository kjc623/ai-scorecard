// Package localipc is capture-core's local endpoint: a named pipe on Windows and a Unix socket
// elsewhere, on which the service serves the processes it works with inside users' sessions (the
// browser's native-messaging relay and the user-session helper).
//
// Both ends check the other. The server names the account of every connecting process from the
// operating system (the pipe's client process, the socket's peer credentials) and refuses a
// connection it cannot name. A client hands nothing to an endpoint that is not served by the
// service's account.
//
// Every message is one frame: a 4-byte little-endian length, then that many bytes of JSON. This is
// Chromium's native-messaging framing, so the relay carries the browser's frames unchanged.
package localipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

const (
	// MaxFrameBytes is Chromium's limit for one message to the browser, and the limit the
	// extension holds itself to. A length that claims more is refused before allocating.
	MaxFrameBytes = 1 << 20
	headerBytes   = 4
)

// WriteFrame writes one frame in a single Write, so concurrent writers cannot interleave.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return fmt.Errorf("native message is %d bytes, over the %d limit", len(payload), MaxFrameBytes)
	}
	buf := make([]byte, headerBytes+len(payload))
	binary.LittleEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[headerBytes:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame. io.EOF before the first header byte is the peer closing.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [headerBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, fmt.Errorf("native frame declares %d bytes, over the %d limit", n, MaxFrameBytes)
	}
	if n == 0 {
		return nil, errors.New("native frame carries no payload")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// MaxClients bounds concurrent connections: one per browser profile and one helper per signed-in
// session is the norm.
const MaxClients = 64

// Handler serves one connection whose peer the server identified. The server closes the
// connection when the handler returns.
type Handler func(conn net.Conn, peer hostinfo.User)

// Server accepts connections on the endpoint and runs the handler for each.
type Server struct {
	addr   string
	handle Handler
	log    *slog.Logger

	mu      sync.Mutex
	ln      net.Listener
	conns   map[net.Conn]struct{}
	stopped bool
	wg      sync.WaitGroup
}

// NewServer returns a server for the endpoint at addr.
func NewServer(addr string, handle Handler, log *slog.Logger) *Server {
	return &Server{addr: addr, handle: handle, log: log, conns: map[net.Conn]struct{}{}}
}

// Addr is the endpoint the server listens on.
func (s *Server) Addr() string { return s.addr }

// Start listens on the endpoint and serves connections until Stop.
func (s *Server) Start() error {
	ln, err := Listen(s.addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go s.accept(ln)
	s.log.Info("local endpoint listening", "endpoint", s.addr)
	return nil
}

func (s *Server) accept(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			stopped := s.stopped
			s.mu.Unlock()
			if stopped || errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("local endpoint: accept failed", "error", err)
			continue
		}
		s.mu.Lock()
		if s.stopped || len(s.conns) >= MaxClients {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(conn)
	}
}

// serve identifies the peer and hands the connection to the handler.
func (s *Server) serve(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	peer, err := PeerUser(conn)
	if err != nil {
		// Without the peer's identity nothing it sends can be attributed to the right person, so
		// the connection is refused rather than attributed to a guess.
		s.log.Warn("local endpoint: the connecting process could not be identified; connection refused", "error", err)
		return
	}
	s.handle(conn, peer)
}

// Clients is the number of open connections.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Stop closes the listener and every connection, then waits for the handlers to return.
func (s *Server) Stop() {
	s.mu.Lock()
	s.stopped = true
	ln := s.ln
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
}
