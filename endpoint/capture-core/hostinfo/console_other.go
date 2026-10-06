//go:build !windows && !darwin

package hostinfo

// consoleUser has no portable answer on Linux, so the resolver falls back to this process's own
// user; a service running as root reports no console user.
func consoleUser() (User, error) { return User{}, ErrUnsupported }
