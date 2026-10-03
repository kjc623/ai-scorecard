package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// runService is the production path: build, start in §3.5's order, wait for the service manager's
// signal, then shut down in §3.5's order (loopback released first).
func runService(cfg Config, log *slog.Logger) error {
	ctx, cancel := signalContext()
	defer cancel()

	svc, err := newService(ctx, cfg, log)
	if err != nil {
		return err
	}
	if err := svc.Start(ctx); err != nil {
		return err
	}

	// One health snapshot at startup, on stderr, so an operator sees which rows are in the path
	// before anything else happens. The periodic channel (if a file is configured) continues from
	// here; the production transport is POST /v1/health and belongs to the ingest client.
	if err := svc.health.PrintSnapshot(os.Stderr); err != nil {
		log.Warn("could not print the startup health snapshot", "error", err)
	}

	log.Info("capture-core running; signal to stop")
	<-ctx.Done()
	log.Info("stop signal received; shutting down (§3.5)", "drain_deadline", cfg.DrainDeadline)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.DrainDeadline+15*time.Second)
	defer cancelShutdown()
	return svc.Stop(shutdownCtx)
}

// nativeHost returns the frame handler for this service. The pipeline, the policy view and the
// health channel are the same objects the providers use, so a browser observation and a proxy
// observation cannot diverge.
func (s *service) nativeHost() *nativeHost {
	store := &policyStoreView{
		currentVersion: func() string {
			if b := s.currentBundle(); b != nil {
				return b.Version
			}
			return ""
		},
		currentBytes: func() []byte {
			if s.store == nil {
				return nil
			}
			return s.store.InForceRaw()
		},
	}
	return newNativeHost(s.pipe, store, s.cfg, s.health, s.log)
}

// peekSpool exposes the spooled entries for the selftest's evidence. It is deliberately on the
// service rather than on the spool holder's public API: nothing else in the agent reads the spool
// back, because the agent's only spool operation is append.
func (s *service) peekSpool(n int) ([]protocol.Entry, error) {
	s.spool.mu.Lock()
	sp := s.spool.sp
	s.spool.mu.Unlock()
	if sp == nil {
		return nil, nil
	}
	return sp.Peek(n)
}

func readRand(b []byte) (int, error) { return rand.Read(b) }

func protocolFramingVersion() byte { return protocol.Version }
