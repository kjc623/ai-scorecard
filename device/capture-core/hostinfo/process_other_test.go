//go:build !windows

package hostinfo

import (
	"errors"
	"net/netip"
	"testing"
)

func TestProcessLookupsAreUnsupportedOffWindows(t *testing.T) {
	if _, err := OwnerOfLocalTCP(netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("OwnerOfLocalTCP err = %v, want ErrUnsupported", err)
	}
	if _, err := ProcessInfo(1); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ProcessInfo err = %v, want ErrUnsupported", err)
	}
	if _, err := ListenersOn(11434); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ListenersOn err = %v, want ErrUnsupported", err)
	}
}
