//go:build !windows

package cli

// systemRootsPEM is the Windows half only: see roots_windows.go.
func systemRootsPEM() []byte { return nil }
