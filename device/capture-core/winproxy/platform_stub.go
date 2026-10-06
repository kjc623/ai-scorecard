//go:build !windows

package winproxy

import (
	"context"
)

// openInternetSettings, signedInUsers and fetchPAC are Windows-only. These stubs keep the package
// compiling on every platform; the Server is never started on a non-Windows platform.
func openInternetSettings(sid string) (Registry, error) { return nil, errUnsupported }

func signedInUsers(context.Context) ([]string, error) { return nil, errUnsupported }

func fetchPAC(context.Context, string) ([]byte, error) { return nil, errUnsupported }
