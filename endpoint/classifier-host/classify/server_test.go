package classify_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/protocol"
)

// serve starts a server on one end of an in-memory connection and returns the client end.
func serve(t *testing.T, h *classify.Host) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	srv := classify.NewServer(h)
	srv.FrameTimeout = 2 * time.Second
	go func() { _ = srv.ServeConn(server) }()
	t.Cleanup(func() { client.Close() })
	return client
}

func writeFrame(t *testing.T, c net.Conn, version byte, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrameVersion(c, version, body); err != nil {
		t.Fatalf("writing a frame: %v", err)
	}
}

func writeRawFrame(t *testing.T, c net.Conn, version byte, payload []byte) {
	t.Helper()
	if err := protocol.WriteFrameVersion(c, version, payload); err != nil {
		t.Fatalf("writing a frame: %v", err)
	}
}

func readFrame(t *testing.T, c net.Conn) (byte, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	v, payload, err := protocol.ReadFrame(c)
	if err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	return v, payload
}

func handshake(t *testing.T, c net.Conn) protocol.HandshakeResponse {
	t.Helper()
	writeFrame(t, c, protocol.Version, protocol.HandshakeRequest{CoreVersion: "core-1", ProtocolVersion: protocol.Version})
	_, payload := readFrame(t, c)
	var resp protocol.HandshakeResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatalf("handshake response is not JSON: %v", err)
	}
	return resp
}

func TestServerServesHandshakeThenClassifications(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateEnforcing, testrig.ReleaseOptions{Version: "server-1"})
	c := serve(t, testrig.Host(t, store))

	resp := handshake(t, c)
	if !resp.OK || resp.ClassifierVersion != "server-1" {
		t.Fatalf("handshake: %+v", resp)
	}

	for i := 0; i < 3; i++ {
		writeFrame(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
		_, payload := readFrame(t, c)
		var v classify.Verdict
		if err := json.Unmarshal(payload, &v); err != nil {
			t.Fatalf("verdict is not JSON: %v", err)
		}
		if err := v.Response.Validate(); err != nil {
			t.Fatalf("verdict %d violates the contract: %v", i, err)
		}
		if len(v.Response.Labels) == 0 {
			t.Fatalf("verdict %d lost its labels", i)
		}
	}
}

// TestVersionMismatchIsADegradedHandshakeResponse is §3.4: "a version handshake on connect marks
// classifier-host degraded on mismatch ... never failing the submission (C21)". It must not crash
// the server and must not be answered with a classification.
func TestVersionMismatchIsADegradedHandshakeResponse(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	c := serve(t, testrig.Host(t, store))

	writeFrame(t, c, protocol.Version+1, protocol.HandshakeRequest{CoreVersion: "core-1", ProtocolVersion: protocol.Version + 1})
	version, payload := readFrame(t, c)
	if version != protocol.Version {
		t.Errorf("the refusal must be answerable by this host's own version, got %d", version)
	}
	var resp protocol.HandshakeResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Reason != protocol.DetailVersionMismatch {
		t.Errorf("a framing version mismatch produced %+v", resp)
	}
	if !resp.Mismatch() {
		t.Error("Mismatch() did not report the refusal")
	}
	// The server closes the connection after a refused handshake, and stays alive for the next one.
	if _, _, err := protocol.ReadFrame(c); err == nil {
		t.Error("the connection stayed open after a refused handshake")
	}
}

func TestFirstFrameThatIsNotAHandshakeIsRefused(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	c := serve(t, testrig.Host(t, store))
	writeFrame(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", "hello"))
	_, payload := readFrame(t, c)
	var resp protocol.HandshakeResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Error("a classification sent before the handshake was accepted as one")
	}
}

func TestMismatchedFrameMidStreamDegradesAndCloses(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	c := serve(t, testrig.Host(t, store))
	if resp := handshake(t, c); !resp.OK {
		t.Fatalf("handshake: %+v", resp)
	}
	writeFrame(t, c, protocol.Version+7, req(protocol.ModeM1, "text/plain", cardBody))
	_, payload := readFrame(t, c)
	var v classify.Verdict
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.Response.Confidence != protocol.ConfidenceDegraded {
		t.Errorf("a mid-stream version mismatch produced confidence %q", v.Response.Confidence)
	}
	if err := v.Response.Validate(); err != nil {
		t.Errorf("the degraded answer violates the contract: %v", err)
	}
	if _, _, err := protocol.ReadFrame(c); err == nil {
		t.Error("the stream was not closed after a mid-stream version mismatch")
	}
}

func TestMalformedRequestPayloadDegradesAndTheConnectionSurvives(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	c := serve(t, testrig.Host(t, store))
	if resp := handshake(t, c); !resp.OK {
		t.Fatalf("handshake: %+v", resp)
	}

	writeRawFrame(t, c, protocol.Version, []byte("{not json"))
	_, payload := readFrame(t, c)
	var v classify.Verdict
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.Response.Confidence != protocol.ConfidenceDegraded {
		t.Errorf("a malformed request produced confidence %q", v.Response.Confidence)
	}
	if err := v.Response.Validate(); err != nil {
		t.Errorf("the refusal violates the contract: %v", err)
	}

	// A field this host does not know is refused rather than ignored: a future protocol that adds
	// an identity field must not be silently accepted here.
	writeRawFrame(t, c, protocol.Version, []byte(`{"mode":"m1","content":"aGk=","user_ref":"u1"}`))
	_, payload = readFrame(t, c)
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.Response.Confidence != protocol.ConfidenceDegraded {
		t.Errorf("an unknown request field was accepted: %+v", v.Response)
	}

	// The connection still works.
	writeFrame(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
	_, payload = readFrame(t, c)
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.Response.Confidence == protocol.ConfidenceDegraded {
		t.Errorf("a valid request after a malformed one was still degraded: %+v", v.Response.Stages)
	}
}

func TestOversizedFrameIsRefusedWithoutAllocatingIt(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	c := serve(t, testrig.Host(t, store))
	if resp := handshake(t, c); !resp.OK {
		t.Fatalf("handshake: %+v", resp)
	}
	// A header declaring more than MaxFrameBytes, and no payload at all: a naive reader would
	// allocate 64 MB and block. ReadFrame refuses it before allocation.
	header := []byte{protocol.Version, 0x7f, 0xff, 0xff, 0xff}
	done := make(chan struct{})
	go func() {
		_, _ = c.Write(header)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writing the oversized frame header blocked")
	}
	_, payload := readFrame(t, c)
	var v classify.Verdict
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.Response.Confidence != protocol.ConfidenceDegraded {
		t.Errorf("an oversized frame declaration produced confidence %q", v.Response.Confidence)
	}
}

// TestPanicInsideTheHostBecomesADegradedAnswer is §10's "the host is the component that must be
// trusted to say `degraded` honestly, so it must not be the component an attacker can crash".
func TestPanicInsideTheHostBecomesADegradedAnswer(t *testing.T) {
	store := release.NewStore()
	// A release with no compiled rules is not something Load can produce; SetActive installs it so
	// the test can drive the panic path deterministically.
	store.SetActive(&release.Release{Version: "broken", State: release.StateShadow})
	c := serve(t, testrig.Host(t, store))
	if resp := handshake(t, c); !resp.OK {
		t.Fatalf("handshake: %+v", resp)
	}
	writeFrame(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
	_, payload := readFrame(t, c)
	var v classify.Verdict
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.Response.Confidence != protocol.ConfidenceDegraded {
		t.Fatalf("a panic inside the host produced confidence %q", v.Response.Confidence)
	}
	if err := v.Response.Validate(); err != nil {
		t.Errorf("the recovered verdict violates the contract: %v", err)
	}
	found := false
	for _, s := range v.Response.Stages {
		if s.Detail == protocol.DetailHostUnreachable {
			found = true
		}
	}
	if !found {
		t.Errorf("the recovery did not name host_unreachable: %+v", v.Response.Stages)
	}
}

func TestServerRefusesAModeViolation(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	c := serve(t, testrig.Host(t, store))
	if resp := handshake(t, c); !resp.OK {
		t.Fatalf("handshake: %+v", resp)
	}
	writeFrame(t, c, protocol.Version, req(protocol.ModeM0, "text/plain", cardBody))
	_, payload := readFrame(t, c)
	var v classify.Verdict
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	if err := v.Response.Validate(); err != nil {
		t.Fatal(err)
	}
	if v.Response.Confidence != protocol.ConfidenceDegraded || len(v.Response.Labels) != 0 {
		t.Errorf("content at M0 must be refused, got %+v", v.Response)
	}
}

// TestServerSurvivesManySequentialConnections is the "never crash" property at connection scope.
func TestServerSurvivesManySequentialConnections(t *testing.T) {
	store, _, _ := testrig.Store(t, release.StateShadow, testrig.ReleaseOptions{})
	h := testrig.Host(t, store)
	for i := 0; i < 5; i++ {
		c := serve(t, h)
		if resp := handshake(t, c); !resp.OK {
			t.Fatalf("connection %d: %+v", i, resp)
		}
		writeFrame(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
		readFrame(t, c)
		if i == 1 {
			// one bad connection in the middle: a version mismatch, then close
			writeFrame(t, c, protocol.Version+1, req(protocol.ModeM1, "text/plain", "x"))
			readFrame(t, c)
		}
		c.Close()
	}
}

var _ = io.EOF
var _ = bytes.MinRead
