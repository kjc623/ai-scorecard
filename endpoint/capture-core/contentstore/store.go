// Package contentstore is the M3 local content store (docs/01-collectors.md §11.3, ADR 0017):
// content keyed by event, sealed at rest, held on the device within local retention, and never
// leaving it except through a per-event grant (docs/02-ingest-and-transport.md §3, §10).
//
// It holds two things per event. The content itself is one file sealed with AES-256-GCM under a
// key file that lives outside the store directory, so a copy of the directory alone opens nothing.
// The grant state is one index entry: whether the event has been delivered (a grant can only be
// requested for an event the server has), and what the server decided. Nothing here enters an
// envelope — a device claiming it holds content is not evidence that it does.
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
)

// State is where one held object is on the grant path.
type State string

const (
	// StateHeld: stored locally; its event has not been delivered, so no grant can be asked for.
	StateHeld State = "held"
	// StateReady: the event was delivered; a grant may be requested.
	StateReady State = "ready"
	// StateUploaded: a grant was issued and the ciphertext was written. Terminal.
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
	CollectionMode string `json:"collection_mode,omitempty"`
	ContentDigest  string `json:"content_digest,omitempty"`
	PolicyRuleID   string `json:"policy_rule_id,omitempty"`
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

// Open opens (or creates) the store in dir, sealed under the key in keyFile. The key file must be
// outside dir, for the reason the spool's must (§12): the key and what it seals do not travel
// together. A missing key file is created with 32 random bytes, mode 0600.
func Open(dir, keyFile string, now func() time.Time) (*Store, error) {
	if dir == "" || keyFile == "" {
		return nil, errors.New("contentstore: a directory and a key file are both required")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	absKey, err := filepath.Abs(keyFile)
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(absDir, absKey); err == nil && rel != ".." && !filepath.IsAbs(rel) && len(rel) >= 1 && rel[0] != '.' {
		return nil, fmt.Errorf("contentstore: key file %s is inside the content directory", keyFile)
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, fmt.Errorf("contentstore: creating %s: %w", dir, err)
	}
	key, err := loadOrCreateKey(absKey)
	if err != nil {
		return nil, err
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

func loadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("contentstore: key file %s is %d bytes, want 32", path, len(key))
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("contentstore: reading key file: %w", err)
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("contentstore: writing key file: %w", err)
	}
	return key, nil
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

// writeAtomic writes through a temporary file and a rename, so a crash leaves the old file or the
// new one, never a torn one.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
