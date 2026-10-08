// Package otlp is the loopback OTLP receiver: AI tools export their own logs, traces and metrics to
// it over OTLP/HTTP (protobuf or JSON) and OTLP/gRPC, authenticated by a per-device bearer token.
// Logs and traces are handed to the normalizer for the sending tool's service.name; metrics are
// acknowledged and discarded.
//
// Request bodies carry prompt text, so nothing here logs a body, an attribute value or a decoding
// error, which can quote the body.
package otlp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// Config is the receiver's configuration. The listen addresses come from the signed bundle
// (endpoint.otel), applied through ApplyPolicy; a test may set them here.
type Config struct {
	// TokenPath is the token file in the state directory (TokenFile).
	TokenPath string

	HTTPListen string
	GRPCListen string

	// Normalizers are consulted in order; the first that accepts a service.name receives it.
	Normalizers []Normalizer

	Log   core.Logger
	Clock func() time.Time
}

// errPortHeld is the start failure when a listen address cannot be bound.
var errPortHeld = errors.New(string(protocol.DetailPortHeldByOther))

// Receiver is the otel_receiver collector.
type Receiver struct {
	cfg   Config
	token string

	// life serialises Start, Stop and ApplyPolicy.
	life sync.Mutex

	mu          sync.Mutex
	httpAddr    string
	grpcAddr    string
	running     bool
	httpLn      net.Listener
	grpcLn      net.Listener
	httpSrv     *http.Server
	grpcSrv     *grpc.Server
	httpUp      bool
	grpcUp      bool
	detail      protocol.Detail
	startedAt   time.Time
	lastSuccess time.Time
	counters    *core.CounterSet
	wg          sync.WaitGroup
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// New returns an unstarted receiver, creating the token file when it does not exist.
func New(cfg Config) (*Receiver, error) {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	tok, err := loadToken(cfg.TokenPath)
	if err != nil {
		return nil, err
	}
	now := cfg.Clock()
	return &Receiver{
		cfg:       cfg,
		token:     tok,
		httpAddr:  cfg.HTTPListen,
		grpcAddr:  cfg.GRPCListen,
		startedAt: now,
		counters:  core.NewCounterSet(now),
	}, nil
}

// Name implements core.Provider.
func (r *Receiver) Name() protocol.Collector { return protocol.CollectorOTelReceiver }

// Enabled implements core.Toggled: the receiver runs only while the bundle in force switches it on.
func (r *Receiver) Enabled(b *policy.Bundle) bool { return b != nil && b.Endpoint.OTel.Enabled }

// Token is the bearer token a tool's exporter must present.
func (r *Receiver) Token() string { return r.token }

// HTTPAddr and GRPCAddr are the bound addresses, empty while a listener is down.
func (r *Receiver) HTTPAddr() string { return r.boundAddr(&r.httpLn) }
func (r *Receiver) GRPCAddr() string { return r.boundAddr(&r.grpcLn) }

func (r *Receiver) boundAddr(ln *net.Listener) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *ln == nil {
		return ""
	}
	return (*ln).Addr().String()
}

// Counters exposes the receiver's counter set.
func (r *Receiver) Counters() *core.CounterSet { return r.counters }

// Start binds both listeners or neither: a held port releases the other and fails the start with
// port_held_by_other.
func (r *Receiver) Start(ctx context.Context) error {
	r.life.Lock()
	defer r.life.Unlock()
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil
	}
	httpAddr, grpcAddr := r.httpAddr, r.grpcAddr
	r.mu.Unlock()

	var stack core.ResourceStack
	httpLn, err := listenLoopback(httpAddr)
	if err != nil {
		r.setDetail(detailFor(err))
		return stack.Failed(fmt.Errorf("otlp: http listener: %w", err))
	}
	stack.On(func() { _ = httpLn.Close() })
	grpcLn, err := listenLoopback(grpcAddr)
	if err != nil {
		r.setDetail(detailFor(err))
		return stack.Failed(fmt.Errorf("otlp: grpc listener: %w", err))
	}
	stack.Commit()

	r.mu.Lock()
	r.running = true
	r.detail = protocol.DetailNone
	r.startedAt = r.cfg.Clock()
	r.mu.Unlock()
	r.serveHTTP(httpLn)
	r.serveGRPC(grpcLn)
	return nil
}

// Stop closes both listeners. It is idempotent and safe after a failed Start.
func (r *Receiver) Stop(ctx context.Context) error {
	r.life.Lock()
	defer r.life.Unlock()
	r.mu.Lock()
	running := r.running
	r.running = false
	r.detail = protocol.DetailNone
	r.mu.Unlock()
	if !running {
		return nil
	}
	r.shutdown(ctx)
	return nil
}

// ApplyPolicy takes the listen addresses from the bundle. While running, a changed address rebinds
// both listeners; one that cannot be bound leaves the receiver degraded with the other serving.
func (r *Receiver) ApplyPolicy(b policy.Bundle) error {
	o := b.Endpoint.OTel
	if !o.Enabled {
		return nil
	}
	for _, a := range []string{o.HTTPListen, o.GRPCListen} {
		if err := checkLoopback(a); err != nil {
			return err
		}
	}
	r.life.Lock()
	defer r.life.Unlock()
	r.mu.Lock()
	if o.HTTPListen == r.httpAddr && o.GRPCListen == r.grpcAddr {
		r.mu.Unlock()
		return nil
	}
	r.httpAddr, r.grpcAddr = o.HTTPListen, o.GRPCListen
	running := r.running
	r.mu.Unlock()
	if !running {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.shutdown(ctx)
	var errs []error
	if ln, err := listenLoopback(o.HTTPListen); err != nil {
		errs = append(errs, fmt.Errorf("otlp: http listener: %w", err))
	} else {
		r.serveHTTP(ln)
	}
	if ln, err := listenLoopback(o.GRPCListen); err != nil {
		errs = append(errs, fmt.Errorf("otlp: grpc listener: %w", err))
	} else {
		r.serveGRPC(ln)
	}
	if len(errs) > 0 {
		r.setDetail(protocol.DetailPortHeldByOther)
		return errors.Join(errs...)
	}
	r.setDetail(protocol.DetailNone)
	return nil
}

// Health implements core.Provider: healthy only while both listeners serve.
func (r *Receiver) Health() core.Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !r.running:
		return r.counters.Snapshot(protocol.StateAbsent, r.detail, r.startedAt, r.lastSuccess)
	case r.httpUp && r.grpcUp:
		return r.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, r.startedAt, r.lastSuccess)
	default:
		return r.counters.Snapshot(protocol.StateDegraded, protocol.DetailPortHeldByOther, r.startedAt, r.lastSuccess)
	}
}

func (r *Receiver) setDetail(d protocol.Detail) {
	r.mu.Lock()
	r.detail = d
	r.mu.Unlock()
}

func (r *Receiver) succeeded() {
	t := r.cfg.Clock()
	r.mu.Lock()
	r.lastSuccess = t
	r.mu.Unlock()
}

func (r *Receiver) serveHTTP(ln net.Listener) {
	srv := &http.Server{
		Handler:           r.httpHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	r.mu.Lock()
	r.httpLn, r.httpSrv, r.httpUp = ln, srv, true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.cfg.Log.Printf("otlp: http listener on %s stopped: %v", ln.Addr(), err)
		}
	}()
}

func (r *Receiver) serveGRPC(ln net.Listener) {
	srv := r.grpcServer()
	r.mu.Lock()
	r.grpcLn, r.grpcSrv, r.grpcUp = ln, srv, true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			r.cfg.Log.Printf("otlp: grpc listener on %s stopped: %v", ln.Addr(), err)
		}
	}()
}

// shutdown stops both servers, letting in-flight requests finish until ctx ends.
func (r *Receiver) shutdown(ctx context.Context) {
	r.mu.Lock()
	httpSrv, grpcSrv := r.httpSrv, r.grpcSrv
	r.httpSrv, r.grpcSrv, r.httpLn, r.grpcLn = nil, nil, nil, nil
	r.httpUp, r.grpcUp = false, false
	r.mu.Unlock()

	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			_ = httpSrv.Close()
		}
	}
	if grpcSrv != nil {
		done := make(chan struct{})
		go func() {
			grpcSrv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			grpcSrv.Stop()
			<-done
		}
	}
	r.wg.Wait()
}

// checkLoopback refuses any address that is not a loopback IP host:port, whatever the bundle says.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("otlp: listen address %q is not host:port", addr)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("otlp: listen address %q is not a loopback IP", addr)
	}
	return nil
}

type bindError struct{ err error }

func (e bindError) Error() string { return fmt.Sprintf("%v: %v", errPortHeld, e.err) }
func (e bindError) Unwrap() error { return errPortHeld }

func listenLoopback(addr string) (net.Listener, error) {
	if err := checkLoopback(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, bindError{err}
	}
	return ln, nil
}

func detailFor(err error) protocol.Detail {
	if errors.Is(err, errPortHeld) {
		return protocol.DetailPortHeldByOther
	}
	return protocol.DetailNone
}
