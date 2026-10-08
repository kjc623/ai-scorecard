//go:build !windows

package main

import "errors"

// runUserHelper refuses: the service starts user-session helpers on Windows only.
func runUserHelper() error {
	return errors.New("the user-session helper runs on Windows only")
}
