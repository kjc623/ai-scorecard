// Package etwsession runs the real-time ETW (Event Tracing for Windows) sessions the collectors read
// the kernel's providers through. A session is named ShadowAICapture-<name>, so one left behind by a
// crashed service is recognised and replaced when the service opens it again.
package etwsession

import (
	"errors"
	"strings"
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
	// EventIDs, when set, limits the provider to these events. The session discards the others
	// after decoding: EnableTraceEx2 rejects an event-id filter on the kernel's providers.
	EventIDs []uint16
}

// eventFilter is the event ids each provider delivers, by the provider's GUID in upper case; a
// provider without an entry delivers every event.
type eventFilter map[string]map[uint16]bool

func newEventFilter(providers []Provider) eventFilter {
	f := eventFilter{}
	for _, p := range providers {
		if len(p.EventIDs) == 0 {
			continue
		}
		ids := make(map[uint16]bool, len(p.EventIDs))
		for _, id := range p.EventIDs {
			ids[id] = true
		}
		f[strings.ToUpper(p.GUID)] = ids
	}
	return f
}

// keeps reports whether an event of provider with id is delivered.
func (f eventFilter) keeps(provider string, id uint16) bool {
	ids, limited := f[strings.ToUpper(provider)]
	return !limited || ids[id]
}

// Event is one event a session delivered.
type Event struct {
	// Provider is the GUID of the provider that wrote it, braced and upper-case.
	Provider string
	ID       uint16
	// PID is the process the event header names: the one in whose context the event was
	// written. A kernel provider may write in another process's context; its payload then names
	// the process the event is about.
	PID uint32
	// Time is when the provider wrote it.
	Time time.Time
	// Properties are the event's payload fields by name, formatted as text by the
	// operating system's event decoder (TDH).
	Properties map[string]string
}
