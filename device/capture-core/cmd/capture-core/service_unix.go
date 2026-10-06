//go:build !windows

package main

// runPlatformService reports false: on macOS and Linux, launchd and systemd run the agent as an
// ordinary foreground process and collect its standard error.
func runPlatformService(Config) (bool, error) { return false, nil }
