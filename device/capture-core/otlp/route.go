package otlp

import (
	"context"
	"encoding/base64"
	"encoding/hex"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/shadow-ai-capture/device/protocol"
)

// Sender is what is known about the client that sent a request.
type Sender struct {
	// RemoteAddr is the client's address, host:port, on the loopback interface.
	RemoteAddr string
}

// Normalizer turns one tool's telemetry into observations. The receiver hands each resource to the
// first registered normalizer that accepts the resource's service.name.
type Normalizer interface {
	Name() string
	Accepts(serviceName string) bool
	Logs(ctx context.Context, from Sender, rl *logspb.ResourceLogs)
	Spans(ctx context.Context, from Sender, rs *tracepb.ResourceSpans)
}

const serviceNameKey = "service.name"

func serviceName(r *resourcepb.Resource) string {
	for _, kv := range r.GetAttributes() {
		if kv.GetKey() == serviceNameKey {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func (r *Receiver) normalizerFor(service string) Normalizer {
	for _, n := range r.cfg.Normalizers {
		if n.Accepts(service) {
			return n
		}
	}
	return nil
}

func (r *Receiver) routeLogs(ctx context.Context, from Sender, req *collogspb.ExportLogsServiceRequest) {
	for _, rl := range req.GetResourceLogs() {
		var n uint64
		for _, sl := range rl.GetScopeLogs() {
			n += uint64(len(sl.GetLogRecords()))
		}
		r.counters.Incr(protocol.CounterObserved, n)
		if norm := r.normalizerFor(serviceName(rl.GetResource())); norm != nil {
			norm.Logs(ctx, from, rl)
		}
	}
}

func (r *Receiver) routeSpans(ctx context.Context, from Sender, req *coltracepb.ExportTraceServiceRequest) {
	for _, rs := range req.GetResourceSpans() {
		var n uint64
		for _, ss := range rs.GetScopeSpans() {
			n += uint64(len(ss.GetSpans()))
		}
		r.counters.Incr(protocol.CounterObserved, n)
		if norm := r.normalizerFor(serviceName(rs.GetResource())); norm != nil {
			norm.Spans(ctx, from, rs)
		}
	}
}

// OTLP/JSON carries trace and span ids as hex, where protojson reads base64. Hex digits are base64
// digits, so a 32-digit trace id decodes to 24 bytes and a 16-digit span id to 12; encoding those
// bytes back gives the hex string, which is decoded here.
func hexID(id []byte, want int) []byte {
	if len(id) != want*3/2 {
		return id
	}
	if raw, err := hex.DecodeString(base64.StdEncoding.EncodeToString(id)); err == nil && len(raw) == want {
		return raw
	}
	return id
}

func traceID(id []byte) []byte { return hexID(id, 16) }
func spanID(id []byte) []byte  { return hexID(id, 8) }

func fixLogIDs(req *collogspb.ExportLogsServiceRequest) {
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				lr.TraceId = traceID(lr.TraceId)
				lr.SpanId = spanID(lr.SpanId)
			}
		}
	}
}

func fixSpanIDs(req *coltracepb.ExportTraceServiceRequest) {
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				s.TraceId = traceID(s.TraceId)
				s.SpanId = spanID(s.SpanId)
				s.ParentSpanId = spanID(s.ParentSpanId)
				for _, l := range s.GetLinks() {
					l.TraceId = traceID(l.TraceId)
					l.SpanId = spanID(l.SpanId)
				}
			}
		}
	}
}
