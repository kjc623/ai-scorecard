package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/userhelper"
	"github.com/shadow-ai-capture/device/protocol"
)

// fakeSessions is a machine with one signed-in session, whose helper the test runs itself.
type fakeSessions struct {
	id   uint32
	user hostinfo.User
}

func (f fakeSessions) Sessions() ([]userhelper.Session, error) {
	return []userhelper.Session{{ID: f.id, User: f.user}}, nil
}

func (f fakeSessions) Owner(id uint32) (hostinfo.User, error) {
	if id != f.id {
		return hostinfo.User{}, errors.New("nobody is signed in to the session")
	}
	return f.user, nil
}

func (fakeSessions) Launch(uint32) (userhelper.Process, error) {
	return nil, errors.New("the test runs the helper itself")
}

// withHelperSessions replaces the signed-in sessions for one test.
func withHelperSessions(t *testing.T, f fakeSessions) {
	t.Helper()
	prev := platform.userSessions
	platform.userSessions = f
	t.Cleanup(func() { platform.userSessions = prev })
}

// connectHelper runs a helper for the session over the service's real endpoint, recording what it
// shows; the returned channel carries RunHelper's result.
func connectHelper(t *testing.T, sessionID uint32, shown *[]protocol.Notify, mu *sync.Mutex) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialWithRetry(ctx, dialNative)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	done := make(chan error, 1)
	go func() {
		done <- userhelper.RunHelper(conn, protocol.HelperHello{SessionID: sessionID, PID: 1}, func(n protocol.Notify) error {
			mu.Lock()
			defer mu.Unlock()
			*shown = append(*shown, n)
			return nil
		})
	}()
	return done
}

// A helper connecting to the service's endpoint with helper_hello for its own session is the
// user_helper provider's, and the service's notifications reach it.
func TestServiceHandsAHelperConnectionToTheProvider(t *testing.T) {
	trustTestServer(t)
	me, err := hostinfo.SystemUserSources().Process()
	if err != nil {
		t.Fatal(err)
	}
	withHelperSessions(t, fakeSessions{id: 4, user: me})
	svc, _ := enrolledService(t, protocol.ModeM0)

	var mu sync.Mutex
	var shown []protocol.Notify
	connectHelper(t, 4, &shown, &mu)
	n := protocol.Notify{Title: "Prompt blocked", Body: "Your organisation's policy blocks this prompt."}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = svc.helpers.Notify(4, n)
		if !errors.Is(err, userhelper.ErrNoHelper) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(shown) != 1 || shown[0] != n {
		t.Fatalf("shown = %+v, want %+v", shown, n)
	}
}

// helper_hello from a user who is not signed in to the session is refused at the endpoint.
func TestServiceRefusesAHelperForAnotherUsersSession(t *testing.T) {
	trustTestServer(t)
	withHelperSessions(t, fakeSessions{id: 4, user: hostinfo.User{SID: entraSID, Account: `AzureAD\AdaLovelace`}})
	svc, _ := enrolledService(t, protocol.ModeM0)

	var mu sync.Mutex
	var shown []protocol.Notify
	select {
	case err := <-connectHelper(t, 4, &shown, &mu):
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("helper ended with %v, want a refused helper_hello", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hello was neither accepted nor refused")
	}
	if err := svc.helpers.Notify(4, protocol.Notify{Title: "t", Body: "b"}); !errors.Is(err, userhelper.ErrNoHelper) {
		t.Fatalf("Notify = %v, want ErrNoHelper", err)
	}
}
