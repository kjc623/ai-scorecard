package winproxy

import (
	"errors"
	"fmt"
	"strings"
)

// errUnsupported is returned by the non-Windows platform stubs.
var errUnsupported = errors.New("winproxy: desktop-app PAC capture is Windows-only")

// Registry is the per-user Internet Settings values the PAC path reads and writes. It is a seam so
// a test supplies a fake and nothing touches the machine's real proxy settings. The name and
// meaning of each value follow WinINET's Internet Settings key.
type Registry interface {
	// GetString returns the REG_SZ value and whether it exists.
	GetString(name string) (string, bool, error)
	// GetDWORD returns the REG_DWORD value and whether it exists.
	GetDWORD(name string) (uint32, bool, error)
	SetString(name, value string) error
	Delete(name string) error
	// Close releases the underlying handle. It is a no-op for an in-memory registry.
	Close() error
}

// The Internet Settings values the PAC path reads and writes.
const (
	autoConfigURL = "AutoConfigURL"
	proxyEnable   = "ProxyEnable"
	proxyServer   = "ProxyServer"
	proxyOverride = "ProxyOverride"
)

// ReadOriginal reads a user's existing proxy behaviour: a PAC (AutoConfigURL), a fixed proxy
// (ProxyEnable + ProxyServer + ProxyOverride), or neither.
func ReadOriginal(reg Registry) (Original, error) {
	o := Original{}
	if v, ok, err := reg.GetString(autoConfigURL); err != nil {
		return o, fmt.Errorf("winproxy: reading AutoConfigURL: %w", err)
	} else if ok {
		o.AutoConfigURL = v
	}
	if v, ok, err := reg.GetDWORD(proxyEnable); err != nil {
		return o, fmt.Errorf("winproxy: reading ProxyEnable: %w", err)
	} else if ok {
		o.ProxyEnable = v != 0
	}
	if v, ok, err := reg.GetString(proxyServer); err != nil {
		return o, fmt.Errorf("winproxy: reading ProxyServer: %w", err)
	} else if ok {
		o.ProxyServer = v
	}
	if v, ok, err := reg.GetString(proxyOverride); err != nil {
		return o, fmt.Errorf("winproxy: reading ProxyOverride: %w", err)
	} else if ok {
		o.ProxyOverride = splitOverride(v)
	}
	return o, nil
}

// splitOverride splits a WinINET ProxyOverride (";"-separated) list into its entries.
func splitOverride(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ";") {
		e = strings.TrimSpace(e)
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Restore is what Apply remembers so the user's previous AutoConfigURL can be put back.
type Restore struct {
	hadOriginal bool
	original    string
}

// Settings writes one user's AutoConfigURL and restores it afterwards.
type Settings struct {
	reg Registry
}

// NewSettings binds a Settings writer to one user's Internet Settings.
func NewSettings(reg Registry) *Settings { return &Settings{reg: reg} }

// Apply points the user's AutoConfigURL at pacURL, remembering the previous value so Restore can
// put it back exactly.
func (s *Settings) Apply(pacURL string) (Restore, error) {
	if s == nil || s.reg == nil {
		return Restore{}, fmt.Errorf("winproxy: no settings registry")
	}
	prev, ok, err := s.reg.GetString(autoConfigURL)
	if err != nil {
		return Restore{}, err
	}
	if err := s.reg.SetString(autoConfigURL, pacURL); err != nil {
		return Restore{}, fmt.Errorf("winproxy: writing AutoConfigURL: %w", err)
	}
	return Restore{hadOriginal: ok, original: prev}, nil
}

// Restore puts the previous AutoConfigURL back, or removes it when there was none. It is
// idempotent: an empty Restore removes the value.
func (s *Settings) Restore(r Restore) error {
	if s == nil || s.reg == nil {
		return fmt.Errorf("winproxy: no settings registry")
	}
	if r.hadOriginal {
		return s.reg.SetString(autoConfigURL, r.original)
	}
	return s.reg.Delete(autoConfigURL)
}
