package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// relayConnectBudget is how long the relay waits for the service's endpoint, which covers a
// service that is still starting when the browser launches the host.
const relayConnectBudget = 10 * time.Second

// runRelay is the native-messaging host a browser starts: it connects to the running service and
// copies frames from stdin to the service and from the service to stdout until either side
// closes. Each frame is read whole and re-written, so a malformed or oversized frame ends the
// relay instead of desynchronising the stream. Nothing but frames is written to stdout.
func runRelay(stdin io.Reader, stdout io.Writer, dial func(context.Context) (net.Conn, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), relayConnectBudget)
	conn, err := dialWithRetry(ctx, dial)
	cancel()
	if err != nil {
		return fmt.Errorf("the Shadow AI Capture service is not reachable: %w", err)
	}
	defer conn.Close()

	errs := make(chan error, 2)
	go func() { errs <- copyFrames(conn, stdin) }()
	go func() { errs <- copyFrames(stdout, conn) }()
	err = <-errs
	// Either direction ending ends the relay: the browser closed the port, or the service stopped.
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// dialWithRetry dials until it succeeds or ctx ends.
func dialWithRetry(ctx context.Context, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	delay := 100 * time.Millisecond
	for {
		conn, err := dial(ctx)
		if err == nil {
			return conn, nil
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
		delay = min(delay*2, time.Second)
	}
}

// copyFrames moves whole native-messaging frames from src to dst until src ends.
func copyFrames(dst io.Writer, src io.Reader) error {
	for {
		payload, err := readNativeFrame(src)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return io.EOF
			}
			return err
		}
		if err := writeNativeFrame(dst, payload); err != nil {
			return err
		}
	}
}
