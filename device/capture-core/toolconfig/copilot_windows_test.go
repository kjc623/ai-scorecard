//go:build windows

package toolconfig

import (
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// The writer's registry is HKEY_LOCAL_MACHINE.
func TestSystemMachineRegistryIsLocalMachine(t *testing.T) {
	if r, ok := systemMachineRegistry().(winRegistry); !ok || r.root != registry.LOCAL_MACHINE {
		t.Fatalf("system registry = %#v, want HKEY_LOCAL_MACHINE", systemMachineRegistry())
	}
}

// Each kind of value reads back as written, a missing key or value reads as absent, and removing a
// missing one is not an error. The test works in a key of its own under HKEY_CURRENT_USER.
func TestWinRegistryRoundTrip(t *testing.T) {
	r := winRegistry{root: registry.CURRENT_USER}
	key := fmt.Sprintf(`Software\ShadowAICaptureTest-toolconfig-%d-%d`, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, key)
	})
	if _, ok, err := r.get(key, "missing"); err != nil || ok {
		t.Fatalf("get in a missing key = %v, %v", ok, err)
	}
	if err := r.remove(key, "missing"); err != nil {
		t.Fatalf("remove in a missing key: %v", err)
	}
	values := map[string]regValue{
		"sz":        stringValue("http://127.0.0.1:47318"),
		"expand":    {Kind: regExpandSZ, String: `%SystemRoot%\system32`},
		"multi":     {Kind: regMultiSZ, Strings: []string{"a", "b"}},
		"dword":     dwordValue(1),
		"qword":     {Kind: regQWORD, Integer: 1 << 40},
		"binary":    {Kind: regBinary, Binary: []byte{1, 2, 3}},
		"emptysz":   stringValue(""),
		"zerodword": dwordValue(0),
	}
	for name, v := range values {
		if err := r.set(key, name, v); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	for name, want := range values {
		got, ok, err := r.get(key, name)
		if err != nil || !ok || !got.equal(want) {
			t.Fatalf("get %s = %+v, %v, %v, want %+v", name, got, ok, err, want)
		}
	}
	if _, ok, err := r.get(key, "missing"); err != nil || ok {
		t.Fatalf("get of a missing value = %v, %v", ok, err)
	}
	if err := r.remove(key, "sz"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.get(key, "sz"); ok {
		t.Fatal("the value is still there after remove")
	}
	if err := r.remove(key, "sz"); err != nil {
		t.Fatalf("removing a missing value: %v", err)
	}
}
