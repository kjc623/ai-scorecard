package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// signalContext returns a context cancelled by SIGINT or SIGTERM, which is how launchd, systemd
// and a console stop the agent.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
