package tlsproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// memSink is the spool as the pipeline sees it.
type memSink struct {
	mu      sync.Mutex
	entries []protocol.Entry
}

func (s *memSink) Append(e protocol.Entry) (protocol.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Seq = uint64(len(s.entries) + 1)
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *memSink) Stats() protocol.SpoolStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocol.SpoolStats{Depth: len(s.entries)}
}

func (s *memSink) confidences(t *testing.T) []protocol.Confidence {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Confidence, 0, len(s.entries))
	for _, e := range s.entries {
		var env struct {
			Confidence protocol.Confidence `json:"confidence"`
		}
		if err := json.Unmarshal(e.Payload, &env); err != nil {
			t.Fatal(err)
		}
		out = append(out, env.Confidence)
	}
	return out
}

// confidentClassifier finds nothing, with high confidence: any degraded record is the route's or
// the pipeline's doing, not the classifier's.
type confidentClassifier struct{}

func (confidentClassifier) Classify(context.Context, protocol.ClassifyRequest) (protocol.ClassifyResponse, error) {
	return protocol.ClassifyResponse{Labels: []protocol.Label{}, ClassifierVersion: "stub-1", Confidence: protocol.ConfidenceHigh}, nil
}

// received is one request as the upstream saw it.
type received struct {
	contentType string
	body        []byte
}

// A body no parser can read, and a body over the cap, reach the server byte for byte with their
// headers, and the event the real pipeline records for them is confidence degraded. A body its
// parser reads is the control: same path, high confidence.
func TestTLSUnparseableAndOverCapBodiesAreForwardedUnchanged(t *testing.T) {
	var mu sync.Mutex
	var got []received
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{contentType: r.Header.Get("Content-Type"), body: b})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	const cap = 256
	bundle := bundleIntercepting(upPort)
	bundle.TenantDefault = protocol.ModeM1
	sink := &memSink{}
	pipe, err := core.NewPipeline(sink, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	pipe.Bundles = func() *policy.Bundle { return bundle }
	pipe.Classifier = confidentClassifier{}
	pipe.SetIdentity(core.Identity{TenantID: "44444444-4444-4444-8444-444444444444", DeviceID: "aaaaaaaa-0000-7000-8000-000000000047", UserRef: "u_console"})

	p := newProviderForTest(t, Config{
		Listen: "127.0.0.1:0", Bundles: func() *policy.Bundle { return bundle },
		Pipeline: pipe, UpstreamRoots: upstreamPool(upstream),
		CanaryHost: "127.0.0.1", CanaryPort: upPort, BodyCap: cap,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cases := []struct {
		name, host, contentType, body string
		want                          protocol.Confidence
	}{
		{"parsed", "api.openai.com", "application/json", `{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`, protocol.ConfidenceHigh},
		{"unknown shape", "api.openai.com", "application/json", `{"model":"gpt-4.1","msgs":[{"role":"user","content":"hello"}]}`, protocol.ConfidenceDegraded},
		{"no parser", "127.0.0.1", "application/octet-stream", "\x00\x01binary\xff\xfe frame \r\n\r\n not json", protocol.ConfidenceDegraded},
		{"over the cap", "api.openai.com", "application/json", `{"model":"gpt-4.1","messages":[{"role":"user","content":"` + strings.Repeat("x", 2*cap) + `"}]}`, protocol.ConfidenceDegraded},
	}
	for i, c := range cases {
		conn, _ := dialThroughProxy(t, p.ListenAddr(), fmt.Sprintf("127.0.0.1:%d", upPort), &tls.Config{ServerName: "127.0.0.1", RootCAs: p.CA().Pool()})
		req := fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", c.host, c.contentType, len(c.body), c.body)
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatalf("%s: write: %v", c.name, err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatalf("%s: read response: %v", c.name, err)
		}
		_ = resp.Body.Close()
		conn.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, want the server's answer", c.name, resp.StatusCode)
		}
		waitFor(t, 2*time.Second, c.name+"'s event", func() bool { return len(sink.confidences(t)) == i+1 })

		mu.Lock()
		last := got[len(got)-1]
		mu.Unlock()
		if string(last.body) != c.body || last.contentType != c.contentType {
			t.Fatalf("%s: the server received %q (%s), want the body unchanged %q (%s)", c.name, last.body, last.contentType, c.body, c.contentType)
		}
		if conf := sink.confidences(t)[i]; conf != c.want {
			t.Fatalf("%s: confidence %q, want %q", c.name, conf, c.want)
		}
	}
}
