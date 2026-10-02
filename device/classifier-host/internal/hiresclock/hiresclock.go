// Package hiresclock is the monotonic, high-resolution clock the pipeline's budget measurement
// needs.
//
// time.Now() is not enough for §9.4 on Windows: the system timer there steps in ~0.5-15 ms
// increments, so every sub-millisecond stage — which is what a rules and validators stage costs on
// a prompt-sized body — measures as exactly zero. A budget "measured" with a clock that cannot see
// the stages is a comment with extra steps, and it would hide a real regression until it was
// already past the p95 it was supposed to protect. QueryPerformanceCounter is the platform's
// high-resolution monotonic source; on every other target time.Now() already has nanosecond
// resolution and needs no help.
//
// The returned time.Time carries no wall-clock meaning on Windows (its epoch is arbitrary), so it
// is only ever used for differences and deadline arithmetic within one process — which is exactly
// what §9.4's budget is. Nothing here is persisted or logged as a timestamp.
package hiresclock

// The implementation is per platform; see now_windows.go and now_other.go.
