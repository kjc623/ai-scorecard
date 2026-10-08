package loopback

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/enforce"
	"github.com/shadow-ai-capture/device/capture-core/parsers/targets"
	"github.com/shadow-ai-capture/device/protocol"
)

// maxRequestBytes is the ceiling on how much of a request line and headers the broker will
// read. A local client sending an unbounded header block is a defect, not a submission.
const maxRequestBytes = 1 << 20

func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// countingReader counts the body bytes as they stream to the upstream. Counting is not reading
// content: size_bytes is available at every mode, including M0, so the counter runs whether or
// not the body is retained.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// bodyBuffer is the M1+ retention of a request body, bounded. It is written to as a tee while
// the body streams upstream, so forwarding is never delayed by observation.
type bodyBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	cap    int64
	over   bool
	closed bool
}

func newBodyBuffer(cap int64) *bodyBuffer { return &bodyBuffer{cap: cap} }

func (b *bodyBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over {
		return len(p), nil // over cap: keep forwarding, retain nothing more
	}
	room := b.cap - int64(b.buf.Len())
	if room <= 0 {
		b.over = true
		return len(p), nil
	}
	if int64(len(p)) > room {
		b.buf.Write(p[:room])
		b.over = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// Read implements core.ContentReader. It is only ever called by the pipeline, and only when
// the resolved mode permits reading content.
func (b *bodyBuffer) Read(context.Context) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	b.closed = true
	return out, nil
}

func (b *bodyBuffer) overCap() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over
}

// isSubmission is the broker's local shape decision. The port is already the generative
// endpoint, so the predicate here is deliberately narrow: a body-bearing POST that is not the
// preflight request. Everything else is counted as `skipped_not_generative`, which proves the
// predicate is running without claiming it is right.
func isSubmission(req *http.Request, preflightPath string) bool {
	if req.Method != http.MethodPost {
		return false
	}
	if preflightPath != "" && req.URL != nil && req.URL.Path == preflightPath {
		return false
	}
	if req.ContentLength == 0 {
		return false
	}
	return true
}

func (r *portRunner) handleConn(ctx context.Context, client net.Conn) {
	defer client.Close()
	b := r.broker

	// A kill switch in force carries the connection as bytes: nothing is parsed, read, recorded or
	// enforced, and the port stays held so the tool's clients keep reaching its server.
	if b.killSwitchActive() {
		r.tunnel(client)
		return
	}

	br := bufio.NewReader(io.LimitReader(client, maxRequestBytes))
	req, err := http.ReadRequest(br)
	if err != nil {
		// A connection that closes without sending a request is not a unit of work: liveness
		// probes and port scanners do this, and counting them would inflate `observed` with
		// things the broker never saw. A malformed request *is* a defect and is counted.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return
		}
		b.counters.Add(protocol.CounterErrors)
		return
	}
	defer req.Body.Close()
	// A parsed request is a unit of work observed, which is what the coverage denominator means.
	b.counters.Add(protocol.CounterObserved)

	submission := isSubmission(req, r.currentSpec().PreflightPath)
	if !submission {
		b.counters.Add(protocol.CounterSkippedNotGenerative)
	}

	// Resolve the mode before any byte of the body is retained.
	mode := r.currentSpec().Mode
	if b.cfg.Pipeline != nil {
		res := b.cfg.Pipeline.ResolveMode(core.ScopeQuery{
			ToolFingerprint: r.currentSpec().ToolFingerprint,
			Population:      b.cfg.Agent.Population,
			UserRef:         b.cfg.Agent.UserRef,
		})
		mode = res.Mode
	}

	var buf *bodyBuffer
	var body io.Reader = req.Body
	if submission && mode.ReadsContent() {
		buf = newBodyBuffer(b.cfg.BodyCap)
		body = io.TeeReader(req.Body, buf)
	}
	counted := &countingReader{r: body}

	upstream, err := net.DialTimeout("tcp", r.upstreamAddr(), b.cfg.PreflightTimeout)
	if err != nil {
		// An upstream failure: the client gets the connection error it
		// would have seen anyway. Closing without a response is that error; no response is
		// substituted, and nothing is invented.
		b.counters.Add(protocol.CounterErrors)
		return
	}
	defer upstream.Close()
	_ = upstream.SetDeadline(time.Now().Add(2 * b.cfg.PreflightTimeout))

	outReq := req.Clone(ctx)
	outReq.URL = &url.URL{Scheme: "http", Host: r.upstreamAddr(), Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	outReq.Host = r.upstreamAddr()
	outReq.RequestURI = ""
	outReq.Body = io.NopCloser(counted)

	writeDone := make(chan error, 1)
	go func() { writeDone <- outReq.Write(upstream) }()

	resp, rerr := http.ReadResponse(bufio.NewReader(upstream), req)
	if rerr != nil {
		// The upstream failed mid-exchange: the client sees the failure, and we do not
		// substitute a response.
		b.counters.Add(protocol.CounterErrors)
		<-writeDone
		return
	}
	// Responses are streamed through without buffering: responses are not captured.
	werr := resp.Write(client)
	_ = resp.Body.Close()
	select {
	case <-writeDone:
	case <-time.After(2 * b.cfg.PreflightTimeout):
		b.counters.Add(protocol.CounterErrors)
	}
	if werr != nil {
		b.counters.Add(protocol.CounterErrors)
		return
	}
	b.markSuccess(b.cfg.Clock())

	if !submission || b.cfg.Pipeline == nil {
		return
	}
	r.observe(ctx, req, counted.n, buf, mode)
}

// tunnel copies bytes between the client and the upstream in both directions until either side
// closes, and counts the connection blind_tunnelled.
func (r *portRunner) tunnel(client net.Conn) {
	b := r.broker
	upstream, err := net.DialTimeout("tcp", r.upstreamAddr(), b.cfg.PreflightTimeout)
	if err != nil {
		// The client sees the connection error it would have seen without the broker.
		b.counters.Add(protocol.CounterErrors)
		return
	}
	defer upstream.Close()
	b.counters.Add(protocol.CounterBlindTunnelled)
	b.markSuccess(b.cfg.Clock())
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if c, ok := client.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// observe hands the request to the pipeline. The content reader is passed only when the body
// was retained, and the body is only retained when the mode permits reading it — so an M0
// observation reaches the pipeline with no reader at all, and the pipeline's gate would refuse
// one if it were there.
func (r *portRunner) observe(ctx context.Context, req *http.Request, counted int64, buf *bodyBuffer, mode protocol.CollectionMode) {
	b := r.broker
	size := counted
	if req.ContentLength > 0 {
		size = req.ContentLength
	}
	var content core.ContentReader
	if buf != nil {
		content = buf
	}
	tool := r.currentSpec().ToolFingerprint
	extract := targets.Registry().For(req.Host, req.URL.Path)
	// The request has been forwarded by now, so whatever a rule asks for is recorded as logged.
	obs := core.Observation{
		Route:           protocol.RouteProxyLoopback,
		Kind:            protocol.KindPrompt,
		ToolFingerprint: tool,
		Population:      b.cfg.Agent.Population,
		MediaType:       req.Header.Get("Content-Type"),
		OccurredAt:      b.cfg.Clock(),
		SizeBytes:       size,
		Enforce:         enforce.Hook(b.cfg.Bundles, protocol.RouteProxyLoopback, tool, false),
		Content:         content,
		OverCap:         buf != nil && buf.overCap(),
		Extract:         extract,
	}
	// The monotonic offset is milliseconds since the broker started, which is an arbitrary but
	// per-device-consistent origin: it gives intra-device ordering that survives clock changes.
	obs.MonotonicOffsetMS = obs.OccurredAt.Sub(b.startedAt).Milliseconds()

	out, err := b.cfg.Pipeline.Process(ctx, obs)
	switch {
	case out.Reason == core.ReasonIdentityUnresolved:
		// Fail-closed for identity: the request is forwarded, the envelope is not minted, and the
		// coverage row reports degraded with the named detail rather than claiming healthy.
		b.setIdentityDetail(protocol.DetailIdentityUnresolved)
		b.counters.Add(protocol.CounterErrors)
	case err != nil:
		b.setIdentityDetail(protocol.DetailNone)
		b.counters.Add(protocol.CounterDropped)
	case out.Emitted:
		b.setIdentityDetail(protocol.DetailNone)
		b.counters.Add(protocol.CounterEmitted)
	}
	if extract.Panicked() {
		b.counters.Add(protocol.CounterErrors)
	}
}
