package winproxy

import (
	"reflect"
	"testing"
)

// fakeReg is an in-memory Registry so a test never touches the machine's real proxy settings.
type fakeReg struct {
	strings map[string]string
	dwords  map[string]uint32
	deleted map[string]bool
}

func newFakeReg() *fakeReg {
	return &fakeReg{
		strings: map[string]string{},
		dwords:  map[string]uint32{},
		deleted: map[string]bool{},
	}
}

func (f *fakeReg) GetString(name string) (string, bool, error) {
	v, ok := f.strings[name]
	return v, ok, nil
}

func (f *fakeReg) GetDWORD(name string) (uint32, bool, error) {
	v, ok := f.dwords[name]
	return v, ok, nil
}

func (f *fakeReg) SetString(name, value string) error {
	f.strings[name] = value
	delete(f.deleted, name)
	return nil
}

func (f *fakeReg) Delete(name string) error {
	delete(f.strings, name)
	f.deleted[name] = true
	return nil
}

func (f *fakeReg) Close() error { return nil }

func TestReadOriginal(t *testing.T) {
	reg := newFakeReg()
	reg.strings["AutoConfigURL"] = "http://corp/proxy.pac"
	reg.dwords["ProxyEnable"] = 1
	reg.strings["ProxyServer"] = "proxy.corp:8080;backup:8081"
	reg.strings["ProxyOverride"] = "<local>;*.corp.example;intranet"

	o, err := ReadOriginal(reg)
	if err != nil {
		t.Fatalf("ReadOriginal: %v", err)
	}
	if o.AutoConfigURL != "http://corp/proxy.pac" {
		t.Fatalf("AutoConfigURL = %q", o.AutoConfigURL)
	}
	if !o.ProxyEnable {
		t.Fatal("ProxyEnable not read")
	}
	if o.ProxyServer != "proxy.corp:8080;backup:8081" {
		t.Fatalf("ProxyServer = %q", o.ProxyServer)
	}
	if !reflect.DeepEqual(o.ProxyOverride, []string{"<local>", "*.corp.example", "intranet"}) {
		t.Fatalf("ProxyOverride = %v", o.ProxyOverride)
	}

	// A missing value is "not present", not an error.
	empty, err := ReadOriginal(newFakeReg())
	if err != nil {
		t.Fatalf("ReadOriginal(empty): %v", err)
	}
	if empty.ProxyEnable || empty.ProxyServer != "" || empty.AutoConfigURL != "" {
		t.Fatalf("empty registry read as %+v", empty)
	}
}

func TestSettingsApplyRestoreExisting(t *testing.T) {
	reg := newFakeReg()
	reg.strings["AutoConfigURL"] = "http://corp/proxy.pac"

	s := NewSettings(reg)
	r, err := s.Apply("http://127.0.0.1:8350/proxy.pac?u=S-1-5-21-1")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := reg.strings["AutoConfigURL"]; got != "http://127.0.0.1:8350/proxy.pac?u=S-1-5-21-1" {
		t.Fatalf("AutoConfigURL after Apply = %q", got)
	}
	if !r.hadOriginal || r.original != "http://corp/proxy.pac" {
		t.Fatalf("Restore = %+v", r)
	}

	if err := s.Restore(r); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := reg.strings["AutoConfigURL"]; got != "http://corp/proxy.pac" {
		t.Fatalf("AutoConfigURL after Restore = %q", got)
	}
}

func TestSettingsApplyRestoreAbsent(t *testing.T) {
	reg := newFakeReg()
	s := NewSettings(reg)
	r, err := s.Apply("http://127.0.0.1:8350/proxy.pac?u=S-1-5-21-2")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.hadOriginal {
		t.Fatalf("Restore = %+v, want no original", r)
	}
	if err := s.Restore(r); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, ok := reg.strings["AutoConfigURL"]; ok {
		t.Fatal("AutoConfigURL should be removed after Restore")
	}
	if !reg.deleted["AutoConfigURL"] {
		t.Fatal("Delete not called for an originally-absent value")
	}
}

func TestSplitOverride(t *testing.T) {
	got := splitOverride(" <local> ; *.corp.example ; intranet ; ")
	if !reflect.DeepEqual(got, []string{"<local>", "*.corp.example", "intranet"}) {
		t.Fatalf("splitOverride = %v", got)
	}
	if len(splitOverride("")) != 0 {
		t.Fatal("splitOverride(\"\") should be empty")
	}
}

func TestProxyRoute(t *testing.T) {
	if got := proxyRoute(Original{}); got != `"DIRECT"` {
		t.Fatalf("proxyRoute(empty) = %s", got)
	}
	if got := proxyRoute(Original{ProxyEnable: true, ProxyServer: "a:1;b:2"}); got != `"PROXY a:1; PROXY b:2; DIRECT"` {
		t.Fatalf("proxyRoute = %s", got)
	}
	// The WinINET "http=host:port" form drops the scheme key.
	if got := proxyRoute(Original{ProxyEnable: true, ProxyServer: "http=a:1;https=a:2"}); got != `"PROXY a:1; PROXY a:2; DIRECT"` {
		t.Fatalf("proxyRoute(scheme form) = %s", got)
	}
}
