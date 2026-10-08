//go:build !windows

package etwsession

import (
	"errors"
	"testing"
)

// Without ETW no session opens, and the platform says so.
func TestOpenIsUnsupported(t *testing.T) {
	s, err := Open("process", []Provider{{GUID: "{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}", Level: 4, Keywords: 0x10}})
	if !errors.Is(err, ErrUnsupported) || s != nil {
		t.Fatalf("Open = %v, %v; want nil, ErrUnsupported", s, err)
	}
}
