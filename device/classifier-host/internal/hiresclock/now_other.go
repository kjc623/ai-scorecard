//go:build !windows

package hiresclock

import "time"

// Now is time.Now on every other target: the monotonic reading already has nanosecond resolution,
// including js/wasm, where performance.now() backs it.
func Now() time.Time { return time.Now() }
