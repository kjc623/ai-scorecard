//go:build !windows

package main

import (
	"errors"
	"log/slog"
)

// runAsService is Windows-only. The --service flag still parses on every platform so the flag set is
// one shape, and a non-Windows binary refuses it clearly at run time rather than at parse time.
func runAsService(Config, *slog.Logger) error {
	return errors.New("--service is only supported on Windows")
}
