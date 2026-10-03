//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package spool

import (
	"fmt"
	"os"
)

// No tested writer-lock primitive exists for this platform in the standard library, so the
// spool refuses to open rather than running with a lock it cannot honour. The product
// targets Windows and macOS (docs/01-collectors.md §14); a new platform gets a real
// implementation here first.
func lockExclusive(f *os.File) error {
	return fmt.Errorf("spool: no writer-lock implementation for this platform; one writer cannot be guaranteed")
}

func unlockExclusive(f *os.File) error { return nil }
