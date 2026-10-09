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

// A watch reports the key's creation, a value set in it, a value deleted from it and the key's
// deletion, and returns when stopped. The test works in a key of its own under
// HKEY_CURRENT_USER.
func TestWinRegistryWatch(t *testing.T) {
	r := winRegistry{root: registry.CURRENT_USER}
	key := fmt.Sprintf(`Software\ShadowAICaptureTest-toolconfig-watch-%d-%d`, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, key)
	})
	changes := make(chan struct{}, 64)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		r.watch(key, stop, func() { changes <- struct{}{} })
		close(done)
	}()
	drain := func() {
		for {
			select {
			case <-changes:
			default:
				return
			}
		}
	}
	expect := func(what string) {
		t.Helper()
		select {
		case <-changes:
		case <-time.After(3 * keyRetry):
			t.Fatalf("%s was not reported", what)
		}
		time.Sleep(100 * time.Millisecond)
		drain()
	}

	time.Sleep(100 * time.Millisecond)
	drain()
	if err := r.set(key, "CopilotOtelEndpoint", stringValue("http://127.0.0.1:47318")); err != nil {
		t.Fatal(err)
	}
	expect("the key's creation")
	if err := r.set(key, "CopilotOtelEndpoint", stringValue("https://otel.corp.example")); err != nil {
		t.Fatal(err)
	}
	expect("a value set")
	if err := r.remove(key, "CopilotOtelEndpoint"); err != nil {
		t.Fatal(err)
	}
	expect("a value deleted")
	if err := registry.DeleteKey(registry.CURRENT_USER, key); err != nil {
		t.Fatal(err)
	}
	expect("the key's deletion")

	close(stop)
	select {
	case <-done:
	case <-time.After(3 * keyRetry):
		t.Fatal("the watch did not return when stopped")
	}
}
