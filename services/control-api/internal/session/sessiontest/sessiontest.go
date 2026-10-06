// Package sessiontest is an in-memory session.Store for tests. It mirrors the SQL store's
// semantics: a lookup by hash ignores the tenant (the definer function), and a write changes only a
// row of the tenant it names (row-level security).
package sessiontest

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/session"
)

// Store is the in-memory session.Store.
type Store struct {
	mu   sync.Mutex
	rows map[string]session.Record
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{rows: map[string]session.Record{}} }

// Create implements session.Store.
func (m *Store) Create(_ context.Context, rec session.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[hex.EncodeToString(rec.Hash)] = clone(rec)
	return nil
}

// ByHash implements session.Store.
func (m *Store) ByHash(_ context.Context, hash []byte) (session.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.rows[hex.EncodeToString(hash)]
	if !ok {
		return session.Record{}, session.ErrNotFound
	}
	return clone(rec), nil
}

func (m *Store) update(tenantID string, hash []byte, fn func(*session.Record) bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := hex.EncodeToString(hash)
	rec, ok := m.rows[k]
	if !ok || rec.TenantID != tenantID {
		return false
	}
	changed := fn(&rec)
	m.rows[k] = rec
	return changed
}

// Touch implements session.Store.
func (m *Store) Touch(_ context.Context, tenantID string, hash []byte, at time.Time) error {
	m.update(tenantID, hash, func(r *session.Record) bool {
		if r.RevokedAt == nil {
			r.LastSeenAt = at
		}
		return true
	})
	return nil
}

// Revoke implements session.Store.
func (m *Store) Revoke(_ context.Context, tenantID string, hash []byte, at time.Time) (bool, error) {
	return m.update(tenantID, hash, func(r *session.Record) bool {
		if r.RevokedAt != nil {
			return false
		}
		t := at
		r.RevokedAt = &t
		return true
	}), nil
}

// SetRefresh implements session.Store.
func (m *Store) SetRefresh(_ context.Context, tenantID string, hash []byte, enc []byte, at time.Time) error {
	m.update(tenantID, hash, func(r *session.Record) bool {
		if r.RevokedAt == nil {
			r.RefreshEnc = append([]byte(nil), enc...)
			r.RefreshedAt = &at
		}
		return true
	})
	return nil
}

func clone(r session.Record) session.Record {
	r.Hash = append([]byte(nil), r.Hash...)
	r.Roles = append([]string(nil), r.Roles...)
	r.RefreshEnc = append([]byte(nil), r.RefreshEnc...)
	if len(r.RefreshEnc) == 0 {
		r.RefreshEnc = nil
	}
	for _, p := range []**time.Time{&r.RevokedAt, &r.RefreshedAt} {
		if *p != nil {
			t := **p
			*p = &t
		}
	}
	return r
}
