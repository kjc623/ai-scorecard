//go:build windows

package hiresclock

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procQueryPerformanceCounter   = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")
	freqOnce                      sync.Once
	freq                          int64
)

func frequency() int64 {
	freqOnce.Do(func() {
		var f int64
		procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&f)))
		if f <= 0 {
			f = 1
		}
		freq = f
	})
	return freq
}

// Now returns a monotonic instant derived from QueryPerformanceCounter. It is a time.Time so the
// pipeline's deadline arithmetic is unchanged; its wall-clock value is arbitrary and must not be
// stored or compared across processes.
func Now() time.Time {
	var c int64
	procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&c)))
	f := frequency()
	// Split into whole seconds and a remainder before scaling, so a machine that has been up for
	// weeks cannot overflow the nanosecond conversion.
	return time.Unix(c/f, (c%f)*int64(time.Second)/f)
}
