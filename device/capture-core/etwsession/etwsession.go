// Package etwsession runs the real-time ETW (Event Tracing for Windows) sessions the collectors read
// the kernel's providers through. A session is named ShadowAICapture-<name>, so one left behind by a
// crashed service is recognised and replaced when the service opens it again.
package etwsession

import (
	"errors"
	"time"
)

// Prefix begins the name of every session the agent opens.
const Prefix = "ShadowAICapture-"

// ErrUnsupported is returned where the platform has no ETW.
var ErrUnsupported = errors.New("etwsession: ETW is not available on this platform")

// Provider is one provider a session enables.
type Provider struct {
	// GUID is the provider's GUID, braced: {22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}.
	GUID string
	// Level is the most verbose level delivered (4 is informational).
	Level uint8
	// Keywords is the match-any keyword mask; 0 enables every keyword.
	Keywords uint64
	// EventIDs, when set, limits the provider to these events, filtered before they reach the
	// session's buffers.
	EventIDs []uint16
}

// Event is one event a session delivered.
type Event struct {
	// Provider is the GUID of the provider that wrote it, braced and upper-case.
	Provider string
	ID       uint16
	// Time is when the provider wrote it.
	Time time.Time
	// Properties are the event's payload fields by name, formatted as text by the
	// operating system's event decoder (TDH).
	Properties map[string]string
}
