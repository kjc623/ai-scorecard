//go:build windows

package etwsession

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/0xrawsec/golang-etw/etw"
)

// eventBuffer is how many decoded events wait for the reader. While it is full the decoder waits,
// and the session's own buffers hold what the providers write meanwhile.
const eventBuffer = 1024

// Session is one open real-time session and its consumer.
type Session struct {
	trace    *etw.RealTimeSession
	consumer *etw.Consumer

	events chan Event
	// closing is closed by Close; ended when the consumer's ProcessTrace returns, which is when
	// the session stops, by Close or from outside; forwarded when the forwarder has finished.
	closing   chan struct{}
	ended     chan struct{}
	forwarded chan struct{}
	once      sync.Once
}

// Open starts the session ShadowAICapture-<name> with providers enabled and consumes it in real
// time. A session of the same name, left by a crashed service, is stopped first. Starting a session
// needs administrative rights.
func Open(name string, providers []Provider) (*Session, error) {
	if name == "" || len(providers) == 0 {
		return nil, errors.New("etwsession: a session needs a name and at least one provider")
	}
	full := Prefix + name
	enable := make([]etw.Provider, 0, len(providers))
	for _, p := range providers {
		g, err := etw.ParseGUID(p.GUID)
		if err != nil {
			return nil, fmt.Errorf("etwsession: provider %q: %w", p.GUID, err)
		}
		enable = append(enable, etw.Provider{
			GUID:            g.String(),
			EnableLevel:     p.Level,
			MatchAnyKeyword: p.Keywords,
			Filter:          p.EventIDs,
		})
	}
	if err := stopStale(full); err != nil {
		return nil, err
	}

	trace := etw.NewRealTimeSession(full)
	for _, p := range enable {
		// The first provider enabled starts the session.
		if err := trace.EnableProvider(p); err != nil {
			stopTrace(trace)
			return nil, fmt.Errorf("etwsession: enabling %s in %s: %w", p.GUID, full, err)
		}
	}
	consumer := etw.NewRealTimeConsumer(context.Background())
	consumer.FromSessions(trace)
	if err := consumer.Start(); err != nil {
		_ = consumer.Stop()
		stopTrace(trace)
		return nil, fmt.Errorf("etwsession: consuming %s: %w", full, err)
	}

	s := &Session{
		trace:     trace,
		consumer:  consumer,
		events:    make(chan Event, eventBuffer),
		closing:   make(chan struct{}),
		ended:     make(chan struct{}),
		forwarded: make(chan struct{}),
	}
	go func() {
		consumer.Wait()
		close(s.ended)
	}()
	go s.forward()
	return s, nil
}

// Events delivers the session's events in the order they were decoded. It is closed when the
// session stops delivering: by Close, or because the session was stopped from outside.
func (s *Session) Events() <-chan Event { return s.events }

// Close stops the session and its consumer. Events not yet read are discarded.
func (s *Session) Close() {
	s.once.Do(func() {
		close(s.closing)
		// Stopping the session first makes ProcessTrace return without waiting for a buffer. A
		// session that already ended is not stopped again: its handle may name a newer session
		// by now. One that ended on an error and still runs is stopped as stale by the next Open.
		select {
		case <-s.ended:
		default:
			stopTrace(s.trace)
		}
		_ = s.consumer.Stop()
		<-s.forwarded
	})
}

// forward converts the consumer's events onto Events until the session ends or is closed.
func (s *Session) forward() {
	defer close(s.forwarded)
	defer close(s.events)
	src := s.consumer.Events
	for {
		select {
		case e, ok := <-src:
			if !ok {
				return
			}
			if !s.deliver(e) {
				s.discard(src)
				return
			}
		case <-s.ended:
			// Nothing is decoded after ProcessTrace returns: deliver what is buffered, then end.
			for {
				select {
				case e, ok := <-src:
					if !ok || !s.deliver(e) {
						return
					}
				default:
					return
				}
			}
		case <-s.closing:
			s.discard(src)
			return
		}
	}
}

func (s *Session) deliver(e *etw.Event) bool {
	if e == nil {
		return true
	}
	select {
	case s.events <- convert(e):
		return true
	case <-s.closing:
		return false
	}
}

// discard drains the consumer so its decoder, which may be blocked handing over an event, can
// return, until the consumer closes its channel or its ProcessTrace has returned.
func (s *Session) discard(src chan *etw.Event) {
	for {
		select {
		case _, ok := <-src:
			if !ok {
				return
			}
		case <-s.ended:
			return
		}
	}
}

func convert(e *etw.Event) Event {
	props := make(map[string]string, len(e.EventData)+len(e.UserData))
	for _, m := range []map[string]interface{}{e.EventData, e.UserData} {
		for k, v := range m {
			if s, ok := v.(string); ok {
				props[k] = s
			}
		}
	}
	return Event{
		Provider:   e.System.Provider.Guid,
		ID:         e.System.EventID,
		PID:        e.System.Execution.ProcessID,
		Time:       e.System.TimeCreated.SystemTime,
		Properties: props,
	}
}

// stopStale stops a session named name, if one is running. ControlTrace writes the session's name
// back into the properties it is given, so they are allocated with room for it.
func stopStale(name string) error {
	u16, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	props := etw.NewRealTimeEventTraceSessionProperties(name)
	err = etw.ControlTrace(0, u16, props, etw.EVENT_TRACE_CONTROL_STOP)
	if err == nil || errors.Is(err, etw.ERROR_WMI_INSTANCE_NOT_FOUND) {
		return nil
	}
	return fmt.Errorf("etwsession: stopping a stale %s: %w", name, err)
}

func stopTrace(t *etw.RealTimeSession) {
	if t.IsStarted() {
		_ = t.Stop()
	}
}
