package localipc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

func TestFramingIsLittleEndianLengthPrefixed(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte(`{"type":"health"}`)); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if n := binary.LittleEndian.Uint32(raw[:4]); n != 17 {
		t.Fatalf("length prefix = %d, want 17", n)
	}
	got, err := ReadFrame(&buf)
	if err != nil || string(got) != `{"type":"health"}` {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}

func TestFramingRefusesOversizeAndEmpty(t *testing.T) {
	if err := WriteFrame(io.Discard, make([]byte, MaxFrameBytes+1)); err == nil {
		t.Fatal("an oversize frame was written")
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], MaxFrameBytes+1)
	if _, err := ReadFrame(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("an oversize length was accepted before allocation")
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 0})); err == nil {
		t.Fatal("an empty frame was accepted")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// echoServer is a started server that answers each frame with the same frame and records the peer
// of each connection.
func echoServer(t *testing.T) (*Server, chan hostinfo.User) {
	t.Helper()
	peers := make(chan hostinfo.User, 4)
	srv := NewServer(testAddr(t), func(conn net.Conn, peer hostinfo.User) {
		peers <- peer
		for {
			payload, err := ReadFrame(conn)
			if err != nil {
				return
			}
			if WriteFrame(conn, payload) != nil {
				return
			}
		}
	}, testLogger())
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return srv, peers
}

// The server names the connecting process's account, a client that trusts the server's account
// exchanges frames with it, and Stop ends the connection.
func TestServerIdentifiesThePeerAndServesFrames(t *testing.T) {
	srv, peers := echoServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialSelf(ctx, srv.Addr(), true)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := WriteFrame(conn, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFrame(conn); err != nil || string(got) != `{"type":"ping"}` {
		t.Fatalf("echo = %q, %v", got, err)
	}
	me, err := hostinfo.SystemUserSources().Process()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case peer := <-peers:
		if !strings.EqualFold(peer.SID, me.SID) {
			t.Fatalf("peer = %+v, want this process's account %q", peer, me.SID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}
	if srv.Clients() != 1 {
		t.Fatalf("clients = %d, want 1", srv.Clients())
	}
	srv.Stop()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := ReadFrame(conn); err == nil {
		t.Fatal("the connection outlived Stop")
	}
	if srv.Clients() != 0 {
		t.Fatalf("clients after Stop = %d", srv.Clients())
	}
}

// A client hands nothing to an endpoint that is not served by the account it trusts.
func TestDialRefusesAnUntrustedServer(t *testing.T) {
	srv, _ := echoServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialSelf(ctx, srv.Addr(), false)
	if err == nil {
		conn.Close()
		t.Fatal("connected to an endpoint whose server is not trusted")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the refusal was a timeout, not the ownership check: %v", err)
	}
}
