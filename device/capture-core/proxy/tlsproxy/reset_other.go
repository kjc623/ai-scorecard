//go:build !windows

package tlsproxy

import (
	"errors"
	"syscall"
)

// connReset reports whether err is the peer resetting the connection.
func connReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }
