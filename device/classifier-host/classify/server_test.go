package classify_test

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/classifier-host/internal/testrig"
	"github.com/shadow-ai-capture/device/classifier-host/release"
	"github.com/shadow-ai-capture/device/protocol"
)

// serve runs a server on one end of an in-memory connection and returns the other end.
func serve(t *testing.T, h *classify.Host) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	go func() { _ = classify.NewServer(h).ServeConn(server) }()
	t.Cleanup(func() { client.Close() })
	return client
}

func send(t *testing.T, c net.Conn, version byte, payload any) {
	t.Helper()
	body, ok := payload.([]byte)
	if !ok {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := protocol.WriteFrameVersion(c, version, body); err != nil {
		t.Fatalf("writing a frame: %v", err)
	}
}

func receive[T any](t *testing.T, c net.Conn) T {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	version, payload, err := protocol.ReadFrame(c)
	if err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	if version != protocol.Version {
		t.Fatalf("the server answered in protocol version %d", version)
	}
	var v T
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	return v
}

func handshake(t *testing.T, c net.Conn) {
	t.Helper()
	send(t, c, protocol.Version, protocol.HandshakeRequest{CoreVersion: "core", ProtocolVersion: protocol.Version})
	if resp := receive[protocol.HandshakeResponse](t, c); !resp.OK || resp.ClassifierVersion != "test-1" {
		t.Fatalf("handshake: %+v", resp)
	}
}

func requireClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := protocol.ReadFrame(c); err == nil {
		t.Error("the connection stayed open")
	}
}

func requireDegraded(t *testing.T, resp protocol.ClassifyResponse, detail protocol.Detail) {
	t.Helper()
	if err := resp.Validate(); err != nil {
		t.Fatalf("the response violates the contract: %v", err)
	}
	if resp.Confidence != protocol.ConfidenceDegraded || resp.Stages[len(resp.Stages)-1].Detail != detail {
		t.Fatalf("response %+v, want degraded with %s", resp, detail)
	}
}

func TestServerAnswersEachRequestWithAResponse(t *testing.T) {
	c := serve(t, newHost(t))
	handshake(t, c)
	for i := 0; i < 3; i++ {
		send(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
		resp := receive[protocol.ClassifyResponse](t, c)
		if err := resp.Validate(); err != nil || classes(resp) != "payment_card" || resp.ClassifierVersion != "test-1" {
			t.Fatalf("response %d: %+v (%v)", i, resp, err)
		}
	}
}

// capture-core matches answers to requests by their order on the connection: requests written
// before any answer is read get one answer each, in the order they were sent, malformed ones
// included.
func TestServerAnswersPipelinedRequestsInOrder(t *testing.T) {
	c := serve(t, newHost(t))
	handshake(t, c)
	frames := []any{
		req(protocol.ModeM1, "text/plain", cardBody),
		[]byte("{not json"),
		req(protocol.ModeM1, "text/plain", "nothing to see here"),
		req(protocol.ModeM1, "text/plain", cardBody),
	}
	go func() {
		for _, f := range frames {
			body, ok := f.([]byte)
			if !ok {
				body, _ = json.Marshal(f)
			}
			if protocol.WriteFrame(c, body) != nil {
				return
			}
		}
	}()
	if resp := receive[protocol.ClassifyResponse](t, c); classes(resp) != "payment_card" {
		t.Fatalf("answer 1: %+v", resp)
	}
	requireDegraded(t, receive[protocol.ClassifyResponse](t, c), protocol.DetailContentUnprocessable)
	if resp := receive[protocol.ClassifyResponse](t, c); resp.Validate() != nil || classes(resp) != "" {
		t.Fatalf("answer 3: %+v", resp)
	}
	if resp := receive[protocol.ClassifyResponse](t, c); classes(resp) != "payment_card" {
		t.Fatalf("answer 4: %+v", resp)
	}
}

func TestHandshakeInAnotherVersionIsRefusedAndCloses(t *testing.T) {
	c := serve(t, newHost(t))
	send(t, c, protocol.Version+1, protocol.HandshakeRequest{CoreVersion: "core", ProtocolVersion: protocol.Version + 1})
	if resp := receive[protocol.HandshakeResponse](t, c); resp.OK || resp.Reason != protocol.DetailVersionMismatch {
		t.Errorf("handshake: %+v", resp)
	}
	requireClosed(t, c)
}

func TestFirstFrameThatIsNotAHandshakeIsRefused(t *testing.T) {
	c := serve(t, newHost(t))
	send(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", "hello"))
	if resp := receive[protocol.HandshakeResponse](t, c); resp.OK {
		t.Error("a request sent before the handshake was accepted as one")
	}
	requireClosed(t, c)
}

func TestFrameInAnotherVersionMidStreamDegradesAndCloses(t *testing.T) {
	c := serve(t, newHost(t))
	handshake(t, c)
	send(t, c, protocol.Version+7, req(protocol.ModeM1, "text/plain", cardBody))
	requireDegraded(t, receive[protocol.ClassifyResponse](t, c), protocol.DetailVersionMismatch)
	requireClosed(t, c)
}

func TestMalformedRequestsDegradeAndTheConnectionContinues(t *testing.T) {
	c := serve(t, newHost(t))
	handshake(t, c)
	send(t, c, protocol.Version, []byte("{not json"))
	requireDegraded(t, receive[protocol.ClassifyResponse](t, c), protocol.DetailContentUnprocessable)

	// A field this build does not know is refused rather than ignored.
	send(t, c, protocol.Version, []byte(`{"mode":"m1","content":"aGk=","user_ref":"u1"}`))
	requireDegraded(t, receive[protocol.ClassifyResponse](t, c), protocol.DetailContentUnprocessable)

	send(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
	if resp := receive[protocol.ClassifyResponse](t, c); classes(resp) != "payment_card" {
		t.Errorf("a valid request after malformed ones: %+v", resp)
	}
}

func TestOversizedFrameIsRefusedWithoutReadingIt(t *testing.T) {
	c := serve(t, newHost(t))
	handshake(t, c)
	go c.Write([]byte{protocol.Version, 0x7f, 0xff, 0xff, 0xff})
	requireDegraded(t, receive[protocol.ClassifyResponse](t, c), protocol.DetailContentOverCap)
	requireClosed(t, c)
}

func TestPanicWhileClassifyingIsADegradedAnswer(t *testing.T) {
	// A release with no compiled rules cannot be loaded; built by hand, it makes the pipeline panic.
	h, err := classify.New(classify.Options{Release: &release.Release{Version: "test-1", Model: testrig.Release(t, "test-1").Model}})
	if err != nil {
		t.Fatal(err)
	}
	c := serve(t, h)
	handshake(t, c)
	send(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
	requireDegraded(t, receive[protocol.ClassifyResponse](t, c), protocol.DetailHostUnreachable)
}

func TestServerSurvivesManyConnections(t *testing.T) {
	h := newHost(t)
	for i := 0; i < 5; i++ {
		c := serve(t, h)
		handshake(t, c)
		send(t, c, protocol.Version, req(protocol.ModeM1, "text/plain", cardBody))
		receive[protocol.ClassifyResponse](t, c)
		c.Close()
	}
}
