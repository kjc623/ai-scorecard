package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// Server is the host side of the local request/response channel between capture-core and
// classifier-host (docs/01-collectors.md §3.4).
//
// §3.4 fixes the shape: request/response, length-prefixed, with a version byte, and "a version
// handshake on connect" that "marks classifier-host degraded on mismatch and falls back to
// rules-only with confidence: degraded, never failing the submission (C21)".
//
// The transport is deliberately not here. ServeConn takes anything that is a byte stream — a
// Unix-domain socket on macOS, the child's stdin/stdout on Windows where a named-pipe listener
// needs a platform binding the offline toolchain cannot fetch, or a loopback TCP socket in tests.
// The framing is protocol's, so both platforms share one implementation.
//
// Malformed input is answered, never fatal: a bad frame produces a degraded verdict, an
// unreadable length prefix closes the connection, and a panic inside the host is recovered into a
// degraded answer. §10 says why that matters — "the host is the component that must be trusted to
// say `degraded` honestly", so it cannot also be a component an attacker can crash with a crafted
// frame.
type Server struct {
	host *Host

	// FrameTimeout bounds the wait for one frame on a connection that supports deadlines. Zero
	// means 10 seconds.
	FrameTimeout time.Duration

	// mu guards the protocol counters; the host itself is concurrency-safe.
	mu       sync.Mutex
	rejected uint64
}

// NewServer wraps a host.
func NewServer(h *Host) *Server {
	return &Server{host: h, FrameTimeout: 10 * time.Second}
}

// ServeConn serves one connection until it closes. A version mismatch ends the connection after a
// degraded handshake response; a mismatched frame mid-stream ends it too, because the stream
// cannot be resynchronised.
func (s *Server) ServeConn(conn io.ReadWriteCloser) (err error) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			// A panic on the interactive path is still a submission that must not fail: answer
			// degraded, then close.
			_ = s.writeVerdict(conn, s.refusal(StageRelease, protocol.DetailHostUnreachable,
				fmt.Sprintf("classifier host panicked: %v", r)))
			err = fmt.Errorf("classify: recovered from panic while serving: %v", r)
		}
	}()

	version, payload, err := s.readFrame(conn)
	if err != nil {
		return err
	}
	if version != protocol.Version {
		return s.writeHandshake(conn, protocol.HandshakeResponse{
			OK: false, ClassifierVersion: s.host.Version(), Reason: protocol.DetailVersionMismatch,
		})
	}
	var hs protocol.HandshakeRequest
	if err := strictUnmarshal(payload, &hs); err != nil {
		// The first frame is not a handshake: the peer is not speaking this contract. It gets a
		// degraded handshake answer, not a crash and not a silent close.
		return s.writeHandshake(conn, protocol.HandshakeResponse{
			OK: false, ClassifierVersion: s.host.Version(), Reason: protocol.DetailVersionMismatch,
		})
	}
	resp := s.host.Handshake(hs)
	if err := s.writeHandshake(conn, resp); err != nil {
		return err
	}
	if !resp.OK {
		// §3.4: the core falls back to rules-only with `confidence: degraded`. There is nothing
		// to serve on a connection whose versions do not match.
		return nil
	}

	for {
		version, payload, err := s.readFrame(conn)
		if err != nil {
			if errors.Is(err, protocol.ErrFrameTooLarge) {
				// The declared length is a lie about the stream: it cannot be skipped, so the
				// connection ends. The core sees no answer and degrades (C21).
				_ = s.writeVerdict(conn, s.refusal(StageRelease, protocol.DetailContentOverCap,
					"the request frame declares more bytes than the classifier will read"))
				return nil
			}
			return nil // EOF, a truncated frame, or a closed pipe: a normal end of connection
		}
		if version != protocol.Version {
			_ = s.writeVerdict(conn, s.refusal(StageRelease, protocol.DetailVersionMismatch,
				fmt.Sprintf("frame speaks protocol version %d, this host speaks %d", version, protocol.Version)))
			return nil
		}
		var req protocol.ClassifyRequest
		if err := strictUnmarshal(payload, &req); err != nil {
			// A malformed request is answered with a degraded verdict and the connection stays
			// open: the frame was well-formed, only its payload was not.
			_ = s.writeVerdict(conn, s.refusal(StageRelease, protocol.DetailContentUnprocessable,
				"the request frame is not a valid ClassifyRequest: "+err.Error()))
			continue
		}
		v := s.host.Classify(context.Background(), req)
		if err := s.writeVerdict(conn, v); err != nil {
			return err
		}
	}
}

// refusal builds a protocol-valid degraded verdict for a server-level refusal.
func (s *Server) refusal(stage string, detail protocol.Detail, errText string) Verdict {
	s.mu.Lock()
	s.rejected++
	s.mu.Unlock()
	resp := protocol.ClassifyResponse{
		ClassifierVersion: s.host.classifierVersion(s.host.opts.Store.Active()),
		Labels:            []protocol.Label{},
		Confidence:        protocol.ConfidenceDegraded,
		Stages: []protocol.StageResult{{
			Stage: stage, Ran: false, Failed: true, Detail: detail, Err: errText,
		}},
		Counters: map[string]uint64{"server_refusals": 1},
	}
	return Verdict{Response: resp, Action: protocol.ActionLogged, DecidedLocally: true,
		EnforcementSuppressed: true, SuppressionCause: "degraded"}
}

func (s *Server) readFrame(conn io.ReadWriteCloser) (byte, []byte, error) {
	if d, ok := conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		timeout := s.FrameTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		_ = d.SetReadDeadline(time.Now().Add(timeout))
	}
	return protocol.ReadFrame(conn)
}

func (s *Server) writeHandshake(conn io.Writer, resp protocol.HandshakeResponse) error {
	return writeJSONFrame(conn, resp)
}

func (s *Server) writeVerdict(conn io.Writer, v Verdict) error {
	return writeJSONFrame(conn, v)
}

func writeJSONFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protocol.WriteFrame(w, payload)
}

// strictUnmarshal refuses an object with fields this build does not know. That is not pedantry on
// this channel: a future protocol that adds an identity field to ClassifyRequest
// (`user_ref`, `destination`) would otherwise be *silently accepted* here, and the classifier
// would have widened what it can see without a single line changing in this package.
func strictUnmarshal(payload []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing content after the JSON value")
	}
	return nil
}

// Rejected counts protocol-level refusals (bad frames, malformed payloads), for the health
// channel.
func (s *Server) Rejected() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected
}
