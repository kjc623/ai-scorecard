package session

import (
	"context"
	"encoding/hex"
	"sync"
	"time"
)

// MemoryStore is the in-memory double of the SQL store, for tests and a local run. It mirrors the
// SQL semantics: lookups by hash ignore the tenant (the definer function), writes require the
// session's own tenant (row-level security).
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Record
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{rows: map[string]Record{}} }

func (m *MemoryStore) Create(_ context.Context, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[hex.EncodeToString(rec.Hash)] = clone(rec)
	return nil
}

func (m *MemoryStore) ByHash(_ context.Context, hash []byte) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.rows[hex.EncodeToString(hash)]
	if !ok {
		return Record{}, ErrNotFound
	}
	return clone(rec), nil
}

func (m *MemoryStore) update(tenantID string, hash []byte, fn func(*Record) bool) bool {
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

func (m *MemoryStore) Touch(_ context.Context, tenantID string, hash []byte, at time.Time) error {
	m.update(tenantID, hash, func(r *Record) bool {
		if r.RevokedAt == nil {
			r.LastSeenAt = at
		}
		return true
	})
	return nil
}

func (m *MemoryStore) Revoke(_ context.Context, tenantID string, hash []byte, at time.Time) (bool, error) {
	return m.update(tenantID, hash, func(r *Record) bool {
		if r.RevokedAt != nil {
			return false
		}
		t := at
		r.RevokedAt = &t
		return true
	}), nil
}

func (m *MemoryStore) SetRefresh(_ context.Context, tenantID string, hash []byte, enc []byte, at time.Time) error {
	m.update(tenantID, hash, func(r *Record) bool {
		if r.RevokedAt == nil {
			r.RefreshEnc = append([]byte(nil), enc...)
			r.RefreshedAt = &at
		}
		return true
	})
	return nil
}

func clone(r Record) Record {
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
