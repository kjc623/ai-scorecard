package classifierlink

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// fakeHost answers frames on the other end of a net.Pipe: no sockets, no ports, no fixed
// addresses.
type fakeHost struct {
	handshake protocol.HandshakeResponse
	reply     protocol.ClassifyResponse
	silent    bool
	frameVers byte // when non-zero, frames are written with this version
	mu        chan struct{}
}

func newFakeHost(t *testing.T, h fakeHost) (net.Conn, *fakeHost) {
	t.Helper()
	client, server := net.Pipe()
	h.mu = make(chan struct{}, 1)
	go func() {
		defer server.Close()
		_, payload, err := protocol.ReadFrame(server)
		if err != nil {
			return
		}
		var req protocol.HandshakeRequest
		_ = json.Unmarshal(payload, &req)
		if h.frameVers != 0 {
			b, _ := json.Marshal(h.handshake)
			_ = protocol.WriteFrameVersion(server, h.frameVers, b)
		} else {
			b, _ := json.Marshal(h.handshake)
			_ = protocol.WriteFrame(server, b)
		}
		if !h.handshake.OK {
			return
		}
		if h.silent {
			// A hung host: read the request and never answer.
			_, _, _ = protocol.ReadFrame(server)
			time.Sleep(2 * time.Second)
			return
		}
		_, _, err = protocol.ReadFrame(server)
		if err != nil {
			return
		}
		rb, _ := json.Marshal(h.reply)
		_ = protocol.WriteFrame(server, rb)
	}()
	return client, &h
}

func clientWithHost(t *testing.T, h fakeHost) *Client {
	t.Helper()
	conn, _ := newFakeHost(t, h)
	return NewWithDialer(func(context.Context) (net.Conn, error) { return conn, nil }, "core-1", 300*time.Millisecond)
}

func TestLinkHandshakeAndClassifyRoundTrip(t *testing.T) {
	c := clientWithHost(t, fakeHost{
		handshake: protocol.HandshakeResponse{OK: true, ClassifierVersion: "rel-2026-10-01"},
		reply: protocol.ClassifyResponse{
			Labels:            []protocol.Label{{Class: "source_code", Score: 0.8, RuleID: "R_SOURCE"}},
			ClassifierVersion: "rel-2026-10-01",
			Confidence:        protocol.ConfidenceHigh,
		},
	})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	resp, err := c.Classify(context.Background(), protocol.ClassifyRequest{
		Content: []byte("hello"), Mode: protocol.ModeM1, BudgetMS: 200,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if resp.ClassifierVersion != "rel-2026-10-01" || len(resp.Labels) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	if degraded, _ := c.Degraded(); degraded {
		t.Fatal("a successful round trip marked the link degraded")
	}
	if got := c.ClassifierVersion(); got != "rel-2026-10-01" {
		t.Fatalf("ClassifierVersion = %q", got)
	}
	_ = c.Close()
}

// A version handshake mismatch marks the host degraded and falls back to rules-only with
// `confidence: degraded` — never failing the submission, and never "no labels found".
func TestLinkVersionMismatchDegradesToRulesOnly(t *testing.T) {
	cases := []struct {
		name string
		host fakeHost
		want protocol.Detail
	}{
		{
			name: "host refuses the handshake",
			host: fakeHost{
				handshake: protocol.HandshakeResponse{OK: false, ClassifierVersion: "v2", Reason: protocol.DetailVersionMismatch},
			},
			want: protocol.DetailVersionMismatch,
		},
		{
			name: "framing version differs",
			host: fakeHost{
				handshake: protocol.HandshakeResponse{OK: true, ClassifierVersion: "v2"},
				frameVers: protocol.Version + 1,
			},
			want: protocol.DetailVersionMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := clientWithHost(t, tc.host)
			// Connect may report the mismatch; the property under test is what Classify does.
			_ = c.Connect(context.Background())
			resp, err := c.Classify(context.Background(), protocol.ClassifyRequest{
				Content: []byte("hello"), Mode: protocol.ModeM1, BudgetMS: 200,
			})
			if err != nil {
				t.Fatalf("a handshake mismatch failed the submission: %v", err)
			}
			if resp.Confidence != protocol.ConfidenceDegraded {
				t.Fatalf("confidence = %q, want degraded", resp.Confidence)
			}
			if resp.ClassifierVersion != RulesOnlyVersion {
				t.Fatalf("classifier version = %q, want the rules-only baseline", resp.ClassifierVersion)
			}
			if err := resp.Validate(); err != nil {
				t.Fatalf("the rules-only fallback does not satisfy the response contract: %v", err)
			}
			degraded, reason := c.Degraded()
			if !degraded {
				t.Fatal("the link did not record itself degraded")
			}
			if reason != tc.want {
				t.Fatalf("degraded reason = %q, want %q", reason, tc.want)
			}
			if got := c.ClassifierVersion(); got != RulesOnlyVersion {
				t.Fatalf("ClassifierVersion = %q, want %q while degraded", got, RulesOnlyVersion)
			}
		})
	}
}

// A hung host is detected by request timeout: "no answer" is degraded, never "no labels found".
func TestLinkHungHostTimesOutToDegraded(t *testing.T) {
	c := clientWithHost(t, fakeHost{
		handshake: protocol.HandshakeResponse{OK: true, ClassifierVersion: "v1"},
		silent:    true,
	})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	start := time.Now()
	resp, err := c.Classify(context.Background(), protocol.ClassifyRequest{
		Content: []byte("hello"), Mode: protocol.ModeM1, BudgetMS: 150,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Classify took %v; the request timeout did not bound it", elapsed)
	}
	if resp.Confidence != protocol.ConfidenceDegraded || resp.ClassifierVersion != RulesOnlyVersion {
		t.Fatalf("response = %+v, want the rules-only degraded fallback", resp)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("fallback response does not validate: %v", err)
	}
	if degraded, reason := c.Degraded(); !degraded || reason != protocol.DetailHostUnreachable {
		t.Fatalf("degraded = %v reason = %q, want true/%s", degraded, reason, protocol.DetailHostUnreachable)
	}
}

// The mode gate is a refusal, not an outage: a request that asks the classifier to read content
// at M0 is a defect upstream and is returned as an error rather than silently degraded.
func TestLinkRefusesAModeViolatingRequest(t *testing.T) {
	c := clientWithHost(t, fakeHost{handshake: protocol.HandshakeResponse{OK: true, ClassifierVersion: "v1"}})
	_ = c.Connect(context.Background())
	_, err := c.Classify(context.Background(), protocol.ClassifyRequest{
		Content: []byte("secret"), Mode: protocol.ModeM0,
	})
	if err == nil {
		t.Fatal("a classify request carrying content at M0 was accepted")
	}
}

func TestLinkCloseIsIdempotent(t *testing.T) {
	c := clientWithHost(t, fakeHost{handshake: protocol.HandshakeResponse{OK: true, ClassifierVersion: "v1"}})
	if err := c.Close(); err != nil {
		t.Fatalf("Close before connect: %v", err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
