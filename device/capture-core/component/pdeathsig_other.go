//go:build !windows && !linux

package component

import "syscall"

// setParentDeathSignal does nothing: the platform has no parent-death signal.
func setParentDeathSignal(*syscall.SysProcAttr) {}
