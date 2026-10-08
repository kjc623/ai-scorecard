package classifierlink

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
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

// echoDialer dials a fake host that, like classifier-host, serves the requests on a connection one
// at a time. Each answer carries the request's content as its rule id, so a caller can tell its own
// answer from another caller's. delay holds the answer to a marker before it is written; received,
// when set, is told each marker the host reads.
//
// Requests are read as they arrive, whatever the host is answering, because a child's stdin is an
// OS pipe that buffers writes; a bare net.Pipe would block each writer until the host is free and
// so hide interleaved callers.
func echoDialer(t *testing.T, delay func(marker string) time.Duration, received chan<- string) Dialer {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	return func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		frames := make(chan []byte, 64)
		go func() {
			defer close(frames)
			for {
				_, payload, err := protocol.ReadFrame(server)
				if err != nil {
					return
				}
				frames <- payload
			}
		}()
		go func() {
			defer server.Close()
			if _, ok := <-frames; !ok {
				return
			}
			hs, _ := json.Marshal(protocol.HandshakeResponse{OK: true, ClassifierVersion: "v1"})
			if err := protocol.WriteFrame(server, hs); err != nil {
				return
			}
			for payload := range frames {
				var req protocol.ClassifyRequest
				if err := json.Unmarshal(payload, &req); err != nil {
					return
				}
				marker := string(req.Content)
				if received != nil {
					received <- marker
				}
				if delay != nil {
					select {
					case <-time.After(delay(marker)):
					case <-stop:
						return
					}
				}
				b, _ := json.Marshal(protocol.ClassifyResponse{
					Labels:            []protocol.Label{{Class: "source_code", Score: 0.5, RuleID: marker}},
					ClassifierVersion: "v1",
					Confidence:        protocol.ConfidenceHigh,
				})
				if err := protocol.WriteFrame(server, b); err != nil {
					return
				}
			}
		}()
		return client, nil
	}
}

// ruleID names the answer a caller received: the marker it echoes, or what came back instead.
func ruleID(resp protocol.ClassifyResponse) string {
	if len(resp.Labels) != 1 {
		return fmt.Sprintf("<%d labels, confidence %s>", len(resp.Labels), resp.Confidence)
	}
	return resp.Labels[0].RuleID
}

// Concurrent callers share one host connection, and each gets the answer to its own request.
func TestLinkConcurrentCallersGetTheirOwnAnswers(t *testing.T) {
	c := NewWithDialer(echoDialer(t, nil, nil), "core-1", 5*time.Second)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	const callers = 50
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			marker := fmt.Sprintf("caller-%02d", i)
			resp, err := c.Classify(context.Background(), protocol.ClassifyRequest{
				Content: []byte(marker), Mode: protocol.ModeM1, BudgetMS: 5000,
			})
			if err != nil {
				t.Errorf("%s: Classify: %v", marker, err)
				return
			}
			if got := ruleID(resp); got != marker {
				t.Errorf("%s received the answer %s", marker, got)
			}
		})
	}
	wg.Wait()
}

// A call that gives up before its answer arrives must not leave that late answer, or its broken
// connection, to the call that follows it.
func TestLinkTimedOutCallDoesNotHandItsAnswerToTheNext(t *testing.T) {
	received := make(chan string, 4)
	delay := func(marker string) time.Duration {
		if marker == "slow" {
			return time.Second
		}
		return 0
	}
	c := NewWithDialer(echoDialer(t, delay, received), "core-1", 5*time.Second)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	slow := make(chan protocol.ClassifyResponse, 1)
	go func() {
		resp, _ := c.Classify(context.Background(), protocol.ClassifyRequest{
			Content: []byte("slow"), Mode: protocol.ModeM1, BudgetMS: 100,
		})
		slow <- resp
	}()
	// The next call starts while the host still holds the slow call's answer.
	if got := <-received; got != "slow" {
		t.Fatalf("host received %q first, want the slow request", got)
	}
	resp, err := c.Classify(context.Background(), protocol.ClassifyRequest{
		Content: []byte("next"), Mode: protocol.ModeM1, BudgetMS: 3000,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got := ruleID(resp); got != "next" {
		t.Errorf("the call after a timed-out call received the answer %s, want its own", got)
	}

	timedOut := <-slow
	if timedOut.Confidence != protocol.ConfidenceDegraded || timedOut.ClassifierVersion != RulesOnlyVersion {
		t.Errorf("the timed-out call = %+v, want the rules-only degraded fallback", timedOut)
	}
}
