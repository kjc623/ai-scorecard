//go:build windows

package toolconfig

import (
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// copilotSupported: Windows has VS Code's machine policies and a machine environment.
const copilotSupported = true

func systemMachineRegistry() machineRegistry { return winRegistry{root: registry.LOCAL_MACHINE} }

// winRegistry is the registry's 64-bit view under root, HKEY_LOCAL_MACHINE outside tests.
type winRegistry struct{ root registry.Key }

func (r winRegistry) get(key, name string) (regValue, bool, error) {
	k, err := registry.OpenKey(r.root, key, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return regValue{}, false, nil
	}
	if err != nil {
		return regValue{}, false, err
	}
	defer k.Close()
	n, kind, err := k.GetValue(name, nil)
	if errors.Is(err, registry.ErrNotExist) {
		return regValue{}, false, nil
	}
	if err != nil {
		return regValue{}, false, err
	}
	v := regValue{Kind: kind}
	switch kind {
	case registry.SZ, registry.EXPAND_SZ:
		v.String, _, err = k.GetStringValue(name)
	case registry.MULTI_SZ:
		v.Strings, _, err = k.GetStringsValue(name)
	case registry.DWORD, registry.QWORD:
		v.Integer, _, err = k.GetIntegerValue(name)
	default:
		v.Binary = make([]byte, n)
		n, _, err = k.GetValue(name, v.Binary)
		v.Binary = v.Binary[:min(n, len(v.Binary))]
	}
	if err != nil {
		return regValue{}, false, err
	}
	return v, true, nil
}

// set writes v with its kind. A kind other than the string, list and integer kinds is written back
// as REG_BINARY, which keeps its bytes.
func (r winRegistry) set(key, name string, v regValue) error {
	k, _, err := registry.CreateKey(r.root, key, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	switch v.Kind {
	case registry.SZ:
		return k.SetStringValue(name, v.String)
	case registry.EXPAND_SZ:
		return k.SetExpandStringValue(name, v.String)
	case registry.MULTI_SZ:
		return k.SetStringsValue(name, v.Strings)
	case registry.DWORD:
		return k.SetDWordValue(name, uint32(v.Integer))
	case registry.QWORD:
		return k.SetQWordValue(name, v.Integer)
	default:
		return k.SetBinaryValue(name, v.Binary)
	}
}

func (r winRegistry) remove(key, name string) error {
	k, err := registry.OpenKey(r.root, key, registry.SET_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

var procSendMessageTimeoutW = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

const (
	hwndBroadcast    = 0xffff
	wmSettingChange  = 0x001A
	smtoAbortIfHung  = 0x0002
	broadcastTimeout = 5000 // milliseconds per window
)

// environmentChanged sends WM_SETTINGCHANGE with "Environment" to every top-level window of the
// sender's session, as setx does. A failed broadcast only delays when running programs see the
// change, so it is not reported.
func (winRegistry) environmentChanged() {
	param, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var result uintptr
	_, _, _ = procSendMessageTimeoutW.Call(hwndBroadcast, wmSettingChange, 0,
		uintptr(unsafe.Pointer(param)), smtoAbortIfHung, broadcastTimeout, uintptr(unsafe.Pointer(&result)))
}

// keyRetry is how often a registry watch looks again for a key that does not exist.
const keyRetry = time.Second

// watch waits on RegNotifyChangeKeyValue for values set or deleted in key. A key that does not
// exist is looked for every keyRetry; its deletion and its creation count as changes. A watch that
// cannot create its events returns at once, and the backstop's comparisons remain.
func (r winRegistry) watch(key string, stop <-chan struct{}, changed func()) {
	stopEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return
	}
	defer windows.CloseHandle(stopEv)
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return
	}
	defer windows.CloseHandle(ev)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			_ = windows.SetEvent(stopEv)
		case <-done:
		}
	}()

	const filter = windows.REG_NOTIFY_CHANGE_NAME | windows.REG_NOTIFY_CHANGE_LAST_SET | windows.REG_NOTIFY_THREAD_AGNOSTIC
	missing := false
	for {
		k, err := registry.OpenKey(r.root, key, registry.NOTIFY|registry.WOW64_64KEY)
		if err != nil {
			missing = true
			if s, _ := windows.WaitForSingleObject(stopEv, uint32(keyRetry/time.Millisecond)); s == windows.WAIT_OBJECT_0 {
				return
			}
			continue
		}
		if missing {
			missing = false
			changed()
		}
		for {
			// Arming fails once the key is deleted.
			if err := windows.RegNotifyChangeKeyValue(windows.Handle(k), false, filter, ev, true); err != nil {
				changed()
				break
			}
			i, err := windows.WaitForMultipleObjects([]windows.Handle{ev, stopEv}, false, windows.INFINITE)
			if err != nil || i != windows.WAIT_OBJECT_0 {
				k.Close()
				return
			}
			changed()
		}
		k.Close()
		missing = true
		if s, _ := windows.WaitForSingleObject(stopEv, uint32(keyRetry/time.Millisecond)); s == windows.WAIT_OBJECT_0 {
			return
		}
	}
}
