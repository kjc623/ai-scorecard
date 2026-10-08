package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/shadow-ai-capture/device/protocol"
)

// Server answers capture-core on one byte stream using protocol's length-prefixed frames: a
// handshake first, then one protocol.ClassifyResponse per request frame, in the order the frames
// arrived. capture-core pairs answers with requests by that order, so every request frame is
// answered exactly once or the connection ends.
//
// Malformed input is answered, never fatal: a malformed request gets a degraded response and the
// connection stays open; a frame in another protocol version, or one declaring more bytes than
// the classifier reads, gets a degraded response and ends the connection, because the stream
// cannot be resynchronised; a panic while classifying is answered with a degraded response.
type Server struct {
	host *Host
}

// NewServer returns a server for host.
func NewServer(host *Host) *Server { return &Server{host: host} }

// ServeConn serves one connection until the peer closes it.
func (s *Server) ServeConn(conn io.ReadWriteCloser) (err error) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			_ = s.refuse(conn, protocol.DetailHostUnreachable, fmt.Sprintf("the classifier failed while classifying: %v", r))
			err = fmt.Errorf("classify: recovered from a panic: %v", r)
		}
	}()

	version, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		return err
	}
	var hs protocol.HandshakeRequest
	resp := protocol.HandshakeResponse{OK: false, ClassifierVersion: s.host.Version(), Reason: protocol.DetailVersionMismatch}
	if version == protocol.Version && strictUnmarshal(payload, &hs) == nil {
		resp = s.host.Handshake(hs)
	}
	if err := writeJSON(conn, resp); err != nil || !resp.OK {
		return err
	}

	for {
		version, payload, err := protocol.ReadFrame(conn)
		switch {
		case errors.Is(err, protocol.ErrFrameTooLarge):
			return s.refuse(conn, protocol.DetailContentOverCap, "the request frame declares more bytes than the classifier reads")
		case err != nil:
			return nil // the peer closed the connection
		case version != protocol.Version:
			return s.refuse(conn, protocol.DetailVersionMismatch,
				fmt.Sprintf("the frame is protocol version %d; this host speaks %d", version, protocol.Version))
		}
		var req protocol.ClassifyRequest
		if err := strictUnmarshal(payload, &req); err != nil {
			if err := s.refuse(conn, protocol.DetailContentUnprocessable, "the request is not a valid ClassifyRequest: "+err.Error()); err != nil {
				return err
			}
			continue
		}
		if err := writeJSON(conn, s.host.Classify(context.Background(), req)); err != nil {
			return err
		}
	}
}

// refuse writes a degraded response for a request the server could not hand to the pipeline.
func (s *Server) refuse(w io.Writer, detail protocol.Detail, msg string) error {
	return writeJSON(w, protocol.ClassifyResponse{
		Labels:            []protocol.Label{},
		ClassifierVersion: s.host.Version(),
		Confidence:        protocol.ConfidenceDegraded,
		Stages:            []protocol.StageResult{{Stage: StageRequest, Failed: true, Detail: detail, Err: msg}},
	})
}

func writeJSON(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protocol.WriteFrame(w, payload)
}

// strictUnmarshal refuses unknown fields, so a request that carries a field this build does not
// know (an identity field, say) is refused rather than silently accepted.
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
