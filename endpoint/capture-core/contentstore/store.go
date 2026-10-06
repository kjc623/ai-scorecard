// Package contentstore holds M3 content on the device: keyed by event, sealed at rest, kept within
// local retention, and leaving the device only through a per-event grant.
//
// It holds two things per event. The content is one file sealed with AES-256-GCM under a key the
// caller keeps outside the store directory, so a copy of the directory alone opens nothing. The
// grant state is one index entry: whether the event has been delivered (a grant can only be
// requested for an event the server has) and what the server decided. Nothing here enters an
// envelope.
package contentstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// State is where one held object is on the grant path.
type State string

const (
	// StateHeld: stored locally; its event has not been delivered, so no grant can be asked for.
	StateHeld State = "held"
	// StateReady: the event was delivered; a grant may be requested.
	StateReady State = "ready"
	// StateUploaded: a grant was issued and the content was uploaded. Terminal.
	StateUploaded State = "uploaded"
	// StateDenied: the server decided against an upload. Terminal; the content stays local.
	StateDenied State = "denied"
)

// Item is the index entry for one event's content. Request carries what the grant request needs
// and the store cannot know: it is supplied when the event is delivered.
type Item struct {
	EventID   string    `json:"event_id"`
	SizeBytes int64     `json:"size_bytes"`
	ExpiresAt time.Time `json:"expires_at"`
	State     State     `json:"state"`
	// Detail is the denial reason, or the last failure of a request that will be retried.
	Detail   string    `json:"detail,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
	Request  Request   `json:"request"`
}

// Request is the event's own description of its content, taken from the delivered envelope.
type Request struct {
	CollectionMode  string `json:"collection_mode,omitempty"`
	ContentDigest   string `json:"content_digest,omitempty"`
	PolicyRuleID    string `json:"policy_rule_id,omitempty"`
	AttachmentCount int    `json:"attachment_count,omitempty"`
}

// ErrNotHeld is returned for an event the store holds no content for.
var ErrNotHeld = errors.New("contentstore: no content is held for this event")

// eventIDPattern keeps an event id usable as a file name: the store never joins caller text into
// a path it has not checked.
var eventIDPattern = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// Store is the device's content store. It is safe for concurrent use.
type Store struct {
	dir string
	gcm cipher.AEAD
	now func() time.Time

	mu    sync.Mutex
	items map[string]*Item
}

// Open opens (or creates) the store in dir, sealed under key (32 bytes). The caller keeps the key
// outside dir: the key and what it seals do not travel together.
func Open(dir string, key []byte, now func() time.Time) (*Store, error) {
	if dir == "" {
		return nil, errors.New("contentstore: a directory is required")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, fmt.Errorf("contentstore: creating %s: %w", dir, err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: absDir, gcm: gcm, now: now, items: map[string]*Item{}}
	if err := s.loadIndex(); err != nil {
		return nil, err
	}
	return s, nil
}

// Put implements core.ContentStore: seal the content for one event and index it as held.
func (s *Store) Put(ctx context.Context, eventID string, content []byte, expiresAt time.Time) error {
	if !eventIDPattern.MatchString(eventID) {
		return fmt.Errorf("contentstore: event id %q is not usable as an object name", eventID)
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	// The event id is the additional data, so a sealed file renamed to another event fails to open.
	sealed := s.gcm.Seal(nonce, nonce, content, []byte(eventID))
	if err := writeAtomic(s.objectPath(eventID), sealed); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[eventID] = &Item{EventID: eventID, SizeBytes: int64(len(content)), ExpiresAt: expiresAt, State: StateHeld}
	return s.saveIndexLocked()
}

// Get opens the content held for one event.
func (s *Store) Get(eventID string) ([]byte, error) {
	s.mu.Lock()
	_, ok := s.items[eventID]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotHeld
	}
	sealed, err := os.ReadFile(s.objectPath(eventID))
	if err != nil {
		return nil, fmt.Errorf("contentstore: reading object: %w", err)
	}
	n := s.gcm.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("contentstore: object is truncated")
	}
	return s.gcm.Open(nil, sealed[:n], sealed[n:], []byte(eventID))
}

// MarkDelivered records that the event reached the server, with the envelope facts a grant
// request repeats. It reports whether content is held for the event; an event with none (any mode
// below M3, or an M3 event whose local write failed) is not an error.
func (s *Store) MarkDelivered(eventID string, req Request) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[eventID]
	if !ok {
		return false, nil
	}
	if it.State == StateHeld {
		it.State = StateReady
		it.Request = req
		return true, s.saveIndexLocked()
	}
	return true, nil
}

// Ready lists the objects a grant may be requested for now, oldest expiry first.
func (s *Store) Ready() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []Item
	for _, it := range s.items {
		if it.State == StateReady && !now.Before(it.RetryAt) {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out
}

// Settle records a terminal decision for one event: uploaded, or denied with its reason.
func (s *Store) Settle(eventID string, state State, detail string) error {
	if state != StateUploaded && state != StateDenied {
		return fmt.Errorf("contentstore: %q is not a terminal state", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[eventID]
	if !ok {
		return ErrNotHeld
	}
	it.State, it.Detail, it.RetryAt = state, detail, time.Time{}
	return s.saveIndexLocked()
}

// Retry leaves the object ready and records why the last attempt failed and when to try again.
func (s *Store) Retry(eventID, detail string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[eventID]
	if !ok {
		return ErrNotHeld
	}
	it.Detail, it.RetryAt = detail, at
	it.Attempts++
	return s.saveIndexLocked()
}

// Expire removes every object past its local retention deadline, whatever its state, and returns
// how many were removed.
func (s *Store) Expire(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, it := range s.items {
		if it.ExpiresAt.IsZero() || now.Before(it.ExpiresAt) {
			continue
		}
		if err := os.Remove(s.objectPath(id)); err != nil && !os.IsNotExist(err) {
			return n, fmt.Errorf("contentstore: removing expired object: %w", err)
		}
		delete(s.items, id)
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.saveIndexLocked()
}

// HeldObjects and HeldBytes implement core.ContentStateReporter.
func (s *Store) HeldObjects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func (s *Store) HeldBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, it := range s.items {
		n += it.SizeBytes
	}
	return n
}

// Counts reports how many objects are in each state, for the device's own health snapshot.
func (s *Store) Counts() map[State]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[State]int{}
	for _, it := range s.items {
		out[it.State]++
	}
	return out
}

func (s *Store) objectPath(eventID string) string {
	return filepath.Join(s.dir, eventID+".sealed")
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }

func (s *Store) loadIndex() error {
	raw, err := os.ReadFile(s.indexPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("contentstore: reading index: %w", err)
	}
	var items []*Item
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("contentstore: index is unreadable: %w", err)
	}
	for _, it := range items {
		s.items[it.EventID] = it
	}
	return nil
}

func (s *Store) saveIndexLocked() error {
	items := make([]*Item, 0, len(s.items))
	for _, it := range s.items {
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].EventID < items[j].EventID })
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return writeAtomic(s.indexPath(), raw)
}

// writeAtomic writes through a protected temporary file and a rename, so a crash leaves the old
// file or the new one, never a torn one.
func writeAtomic(path string, data []byte) error { return state.WriteFile(path, data) }
