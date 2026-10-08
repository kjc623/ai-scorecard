package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/protocol"
)

// The native messaging endpoint. A browser starts capture-core as its native-messaging host in the
// signed-in user's session; that process is a thin relay that connects to the running service on
// the local endpoint and shuttles frames both ways. The service names the connecting process's
// operating-system account, so each browser user's observations are attributed to that user, and
// handles the frames exactly as Chromium's own stdin/stdout channel would carry them. The
// user-session helper connects to the same endpoint and opens with helper_hello, which hands the
// connection to the helper provider; a tool's hook opens with hook_evaluate, which hands it to the
// hook relay.

// newNativeServer is the service's local endpoint at addr.
func newNativeServer(svc *service, addr string) *localipc.Server {
	return localipc.NewServer(addr, svc.serveLocal, svc.log)
}

// serveLocal handles one identified connection: a user-session helper's, a hook's, or a browser
// relay's until either side closes.
func (s *service) serveLocal(conn net.Conn, peer hostinfo.User) {
	read := func() ([]byte, bool) {
		payload, err := localipc.ReadFrame(conn)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			s.log.Info("native messaging: connection ended", "account", peer.Account, "error", err)
		}
		return payload, err == nil
	}
	payload, ok := read()
	if !ok {
		return
	}
	var first protocol.NativeMessage
	if json.Unmarshal(payload, &first) == nil && first.Type == protocol.TypeHelperHello {
		s.helpers.Serve(conn, peer, first)
		return
	}
	if first.Type == protocol.TypeHookEvaluate {
		s.hooks.Serve(conn, peer, first)
		return
	}
	session := newNativeSession(s, s.peerPerson(peer))
	s.log.Info("native messaging: browser connected", "account", peer.Account)
	ctx := context.Background()
	for ok {
		if err := localipc.WriteFrame(conn, session.Handle(ctx, payload)); err != nil {
			return
		}
		payload, ok = read()
	}
}
