package tlsproxy

import (
	"errors"
	"syscall"
)

// connReset reports whether err is the peer resetting or aborting the connection, as Winsock
// reports it.
func connReset(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.WSAECONNABORTED)
}
