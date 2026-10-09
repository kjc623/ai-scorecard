//go:build !windows

package userhelper

// SystemPlatform is nil: helpers run on Windows only so far, and the provider reports absent with
// helper_unavailable elsewhere.
func SystemPlatform(string, ...string) Platform { return nil }
