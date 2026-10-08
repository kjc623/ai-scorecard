//go:build !windows

package procmon

import "errors"

// snapshot is not supported here: the monitor runs only over ETW.
func snapshot() ([]proc, error) { return nil, errors.ErrUnsupported }

// fileVersion reads no version resource here.
func fileVersion(string) string { return "" }
