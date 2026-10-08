//go:build !windows

package etwsession

// Session is never opened here.
type Session struct{}

// Open returns ErrUnsupported: the platform has no ETW.
func Open(name string, providers []Provider) (*Session, error) { return nil, ErrUnsupported }

// Events returns nil.
func (s *Session) Events() <-chan Event { return nil }

// Close does nothing.
func (s *Session) Close() {}
