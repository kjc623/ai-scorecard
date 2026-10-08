package otlp

import (
	"context"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // exporters may compress with gzip
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/shadow-ai-capture/device/protocol"
)

func (r *Receiver) grpcServer() *grpc.Server {
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(MaxBody),
		grpc.UnaryInterceptor(r.authUnary),
		grpc.StatsHandler(connTagger{}),
	)
	collogspb.RegisterLogsServiceServer(srv, logsService{r: r})
	coltracepb.RegisterTraceServiceServer(srv, traceService{r: r})
	colmetricspb.RegisterMetricsServiceServer(srv, metricsService{r: r})
	return srv
}

// authUnary refuses a call whose authorization metadata is not the bearer token, before the
// request reaches a service, and resolves the connection's sender for an authenticated one.
func (r *Receiver) authUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) != 1 || !authorized(vals[0], r.token) {
		r.counters.Add(protocol.CounterErrors)
		r.cfg.Log.Printf("otlp: grpc %s refused: unauthenticated", info.FullMethod)
		return nil, status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	r.senderOf(ctx)
	resp, err := handler(ctx, req)
	if err == nil {
		r.succeeded()
	}
	return resp, err
}

type logsService struct {
	collogspb.UnimplementedLogsServiceServer
	r *Receiver
}

func (s logsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.r.routeLogs(ctx, s.r.senderOf(ctx), req)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

type traceService struct {
	coltracepb.UnimplementedTraceServiceServer
	r *Receiver
}

func (s traceService) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	s.r.routeSpans(ctx, s.r.senderOf(ctx), req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type metricsService struct {
	colmetricspb.UnimplementedMetricsServiceServer
	r *Receiver
}

func (s metricsService) Export(context.Context, *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	s.r.counters.Add(protocol.CounterSkippedNotGenerative)
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}
