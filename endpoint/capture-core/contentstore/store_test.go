package contentstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const eventA = "11111111-1111-4111-8111-111111111111"

func open(t *testing.T, dir string, now func() time.Time) *Store {
	t.Helper()
	s, err := Open(filepath.Join(dir, "content"), filepath.Join(dir, "content.key"), now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// §11.3: content is held sealed, keyed by event, and survives a restart with its grant state.
func TestHeldContentIsSealedAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	content := []byte("the prompt as the person typed it")
	s := open(t, dir, time.Now)
	if err := s.Put(context.Background(), eventA, content, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "content", eventA+".sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, content) {
		t.Fatal("the content directory holds plaintext")
	}
	if held, err := s.MarkDelivered(eventA, Request{CollectionMode: "m3"}); err != nil || !held {
		t.Fatalf("MarkDelivered = %v, %v", held, err)
	}

	reopened := open(t, dir, time.Now)
	got, err := reopened.Get(eventA)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Get after reopen = %q, %v", got, err)
	}
	ready := reopened.Ready()
	if len(ready) != 1 || ready[0].Request.CollectionMode != "m3" {
		t.Fatalf("Ready after reopen = %+v, want the delivered event", ready)
	}
}

// A grant can only be requested for an event the server has, and only until a decision is terminal.
func TestGrantStateMachine(t *testing.T) {
	now := time.Now()
	s := open(t, t.TempDir(), func() time.Time { return now })
	if err := s.Put(context.Background(), eventA, []byte("x"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(s.Ready()) != 0 {
		t.Fatal("content was requestable before its event was delivered")
	}
	if held, _ := s.MarkDelivered("22222222-2222-4222-8222-222222222222", Request{}); held {
		t.Fatal("an event with no held content was reported as held")
	}
	_, _ = s.MarkDelivered(eventA, Request{})
	if len(s.Ready()) != 1 {
		t.Fatal("a delivered event's content is not requestable")
	}
	if err := s.Retry(eventA, "transport", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(s.Ready()) != 0 {
		t.Fatal("a backed-off request was offered again before its retry time")
	}
	now = now.Add(2 * time.Minute)
	if len(s.Ready()) != 1 {
		t.Fatal("a backed-off request was not offered again after its retry time")
	}
	if err := s.Settle(eventA, StateDenied, "over_budget"); err != nil {
		t.Fatal(err)
	}
	if len(s.Ready()) != 0 || s.Counts()[StateDenied] != 1 {
		t.Fatal("a denied event is still requestable")
	}
	if _, err := s.Get(eventA); err != nil {
		t.Fatalf("a denial must leave the content on the device: %v", err)
	}
}

// Local retention removes held content whatever its state, and the key must not live with it.
func TestExpiryAndKeyPlacement(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	s := open(t, dir, func() time.Time { return now })
	if err := s.Put(context.Background(), eventA, []byte("x"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Expire(now.Add(2 * time.Minute)); err != nil || n != 1 {
		t.Fatalf("Expire = %d, %v, want 1", n, err)
	}
	if _, err := s.Get(eventA); err != ErrNotHeld {
		t.Fatalf("expired content is still readable: %v", err)
	}
	if s.HeldObjects() != 0 || s.HeldBytes() != 0 {
		t.Fatal("expired content is still counted as held")
	}
	if _, err := Open(filepath.Join(dir, "c2"), filepath.Join(dir, "c2", "key"), nil); err == nil {
		t.Fatal("a key file inside the content directory was accepted")
	}
}
