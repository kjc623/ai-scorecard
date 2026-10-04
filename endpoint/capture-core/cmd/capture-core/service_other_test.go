//go:build !windows

package main

import "testing"

// On a non-Windows host, --service must refuse at run time with a clear error rather than silently
// doing nothing. This is the platform half of the contract the installer relies on.
func TestServiceModeIsWindowsOnly(t *testing.T) {
	log := newLogger(Config{LogFormat: "json", LogLevel: "error"}, runMode{})
	if err := runAsService(Config{}, log); err == nil {
		t.Fatal("runAsService on a non-Windows host returned nil")
	}
}
