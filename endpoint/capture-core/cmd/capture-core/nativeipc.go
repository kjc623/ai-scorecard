package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// The native messaging endpoint. A browser starts capture-core as its native-messaging host in the
// signed-in user's session; that process is a thin relay that connects to the running service on a
// local endpoint (a named pipe on Windows, a Unix socket elsewhere) and shuttles frames both ways.
// The service names the connecting process's operating-system account, so each browser user's
// observations are attributed to that user, and handles the frames exactly as Chromium's own
// stdin/stdout channel would carry them.

// maxNativeClients bounds concurrent relay connections: one per browser profile is the norm.
const maxNativeClients = 64

// nativeServer accepts relay connections and runs one session per connection.
type nativeServer struct {
	svc  *service
	addr string

	mu      sync.Mutex
	ln      net.Listener
	conns   map[net.Conn]struct{}
	stopped bool
	wg      sync.WaitGroup
}

func newNativeServer(svc *service, addr string) *nativeServer {
	return &nativeServer{svc: svc, addr: addr, conns: map[net.Conn]struct{}{}}
}

// Start listens on the endpoint and serves connections until Stop.
func (n *nativeServer) Start() error {
	ln, err := listenNative(n.addr)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.ln = ln
	n.mu.Unlock()
	n.wg.Add(1)
	go n.accept(ln)
	n.svc.log.Info("native messaging endpoint listening", "endpoint", n.addr)
	return nil
}

func (n *nativeServer) accept(ln net.Listener) {
	defer n.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			n.mu.Lock()
			stopped := n.stopped
			n.mu.Unlock()
			if stopped || errors.Is(err, net.ErrClosed) {
				return
			}
			n.svc.log.Warn("native messaging: accept failed", "error", err)
			continue
		}
		n.mu.Lock()
		if n.stopped || len(n.conns) >= maxNativeClients {
			n.mu.Unlock()
			_ = conn.Close()
			continue
		}
		n.conns[conn] = struct{}{}
		n.mu.Unlock()
		n.wg.Add(1)
		go n.serve(conn)
	}
}

// serve identifies the peer and handles its frames until either side closes.
func (n *nativeServer) serve(conn net.Conn) {
	defer n.wg.Done()
	defer func() {
		_ = conn.Close()
		n.mu.Lock()
		delete(n.conns, conn)
		n.mu.Unlock()
	}()
	user, err := peerUser(conn)
	if err != nil {
		// Without the peer's identity the browser's observations cannot be attributed to the
		// right person, so the connection is refused rather than attributed to a guess.
		n.svc.log.Warn("native messaging: the connecting process could not be identified; connection refused", "error", err)
		return
	}
	session := newNativeSession(n.svc, n.svc.peerPerson(user))
	n.svc.log.Info("native messaging: browser connected", "account", user.Account)
	ctx := context.Background()
	for {
		payload, err := readNativeFrame(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				n.svc.log.Info("native messaging: connection ended", "account", user.Account, "error", err)
			}
			return
		}
		if err := writeNativeFrame(conn, session.Handle(ctx, payload)); err != nil {
			return
		}
	}
}

// Clients is the number of connected relays.
func (n *nativeServer) Clients() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.conns)
}

// Stop closes the listener and every connection, then waits for the sessions to end.
func (n *nativeServer) Stop() {
	n.mu.Lock()
	n.stopped = true
	ln := n.ln
	for c := range n.conns {
		_ = c.Close()
	}
	n.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	n.wg.Wait()
}
