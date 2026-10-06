//go:build !windows && !darwin && !linux

package hostinfo

// consoleUser has no portable answer on a platform without a console-session lookup, so the resolver
// falls back to this process's own user; a service running as root reports no console user.
func consoleUser() (User, error) { return User{}, ErrUnsupported }
