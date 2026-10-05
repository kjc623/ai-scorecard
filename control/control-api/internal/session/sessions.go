package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// The contract's session bounds: a signed-in person is asked to sign in again after eight hours
// whatever they do, and after an hour in which nothing asked for a token.
const (
	MaxAge      = 8 * time.Hour
	IdleTimeout = 1 * time.Hour
)

// Errors the manager distinguishes. ErrEnded is the only one a caller shows: the dashboard learns
// "sign in again", never which of the reasons applied.
var (
	ErrNotFound = errors.New("session: not found")
	ErrEnded    = errors.New("session: ended")
)

// Record is one ops.auth_session row. The opaque id is never stored; Hash is SHA-256 of it.
type Record struct {
	Hash         []byte
	TenantID     string
	ConnectionID string
	// Subject is the IdP subject (Entra: oid) without the connection prefix.
	Subject string
	Actor   string
	Roles   []string
	// UserRef is the person's canonical directory ref when SCIM knows them; empty otherwise.
	UserRef string
	// RefreshEnc is the sealed IdP refresh token, or nil when the provider issued none; RefreshedAt is
	// when it was last exercised (idp_refreshed_at), which the at-most-every-30-minutes rule reads.
	RefreshEnc  []byte
	RefreshedAt *time.Time
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

// SID is the token's sid claim for this session.
func (r Record) SID() string { return SID(r.Hash) }

// Store is the persistence seam over ops.auth_session. The lookup by hash runs before any tenant is
// known (the dashboard presents only the opaque id), so it goes through the contract's definer
// function; every write is tenant-scoped under row-level security.
type Store interface {
	Create(ctx context.Context, rec Record) error
	ByHash(ctx context.Context, hash []byte) (Record, error)
	Touch(ctx context.Context, tenantID string, hash []byte, at time.Time) error
	// Revoke sets revoked_at once; it reports whether this call was the one that revoked.
	Revoke(ctx context.Context, tenantID string, hash []byte, at time.Time) (bool, error)
	SetRefresh(ctx context.Context, tenantID string, hash []byte, enc []byte, at time.Time) error
}

// ManagerConfig bounds sessions. Zero values take the contract's bounds; larger values are clamped
// to them, because the bounds are a security property rather than a preference.
type ManagerConfig struct {
	MaxAge time.Duration
	Idle   time.Duration
	Now    func() time.Time
}

// Manager applies the session rules over a Store.
type Manager struct {
	store  Store
	maxAge time.Duration
	idle   time.Duration
	now    func() time.Time
}

// NewManager builds a manager.
func NewManager(st Store, cfg ManagerConfig) (*Manager, error) {
	if st == nil {
		return nil, errors.New("session: a store is required")
	}
	if cfg.MaxAge <= 0 || cfg.MaxAge > MaxAge {
		cfg.MaxAge = MaxAge
	}
	if cfg.Idle <= 0 || cfg.Idle > IdleTimeout {
		cfg.Idle = IdleTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{store: st, maxAge: cfg.MaxAge, idle: cfg.Idle, now: cfg.Now}, nil
}

// Create opens a session and returns its opaque id, which is shown to the caller exactly once.
func (m *Manager) Create(ctx context.Context, rec Record) (string, Record, error) {
	if !IsUUID(rec.TenantID) || !IsUUID(rec.ConnectionID) || rec.Subject == "" || rec.Actor == "" {
		return "", Record{}, errors.New("session: a session needs tenant, connection, subject and actor")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Record{}, fmt.Errorf("session: no entropy: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(raw[:])
	now := m.now().UTC()
	rec.Hash = HashID(id)
	rec.Roles = CanonicalRoles(rec.Roles)
	rec.CreatedAt, rec.LastSeenAt, rec.ExpiresAt, rec.RevokedAt = now, now, now.Add(m.maxAge), nil
	rec.RefreshedAt = nil
	if len(rec.RefreshEnc) > 0 {
		rec.RefreshedAt = &now // the sign-in itself just exercised the refresh token's grant
	}
	if err := m.store.Create(ctx, rec); err != nil {
		return "", Record{}, fmt.Errorf("session: create: %w", err)
	}
	return id, rec, nil
}

// Resume returns the live session for id. A session that is unknown, revoked, past its maximum age
// or idle too long is ErrEnded; Resume never extends one — the caller touches it once every other
// check has passed.
func (m *Manager) Resume(ctx context.Context, id string) (Record, error) {
	if id == "" || len(id) > 256 {
		return Record{}, fmt.Errorf("%w: malformed id", ErrEnded)
	}
	rec, err := m.store.ByHash(ctx, HashID(id))
	if errors.Is(err, ErrNotFound) {
		return Record{}, fmt.Errorf("%w: unknown", ErrEnded)
	}
	if err != nil {
		return Record{}, err
	}
	now := m.now().UTC()
	switch {
	case rec.RevokedAt != nil:
		return rec, fmt.Errorf("%w: revoked", ErrEnded)
	case !now.Before(rec.ExpiresAt) || !now.Before(rec.CreatedAt.Add(m.maxAge)):
		return rec, fmt.Errorf("%w: expired", ErrEnded)
	case !now.Before(rec.LastSeenAt.Add(m.idle)):
		return rec, fmt.Errorf("%w: idle", ErrEnded)
	}
	return rec, nil
}

// Lookup returns a session by id whatever its state, for revocation.
func (m *Manager) Lookup(ctx context.Context, id string) (Record, error) {
	if id == "" || len(id) > 256 {
		return Record{}, ErrNotFound
	}
	return m.store.ByHash(ctx, HashID(id))
}

// Touch records activity, which is what the idle bound measures.
func (m *Manager) Touch(ctx context.Context, rec Record) error {
	return m.store.Touch(ctx, rec.TenantID, rec.Hash, m.now().UTC())
}

// End revokes a session. It reports whether this call ended it, so a repeated revoke audits once.
func (m *Manager) End(ctx context.Context, rec Record) (bool, error) {
	return m.store.Revoke(ctx, rec.TenantID, rec.Hash, m.now().UTC())
}

// SetRefresh records a successful provider refresh: the (possibly rotated) sealed token, and now.
func (m *Manager) SetRefresh(ctx context.Context, rec Record, enc []byte) error {
	return m.store.SetRefresh(ctx, rec.TenantID, rec.Hash, enc, m.now().UTC())
}

// HashID is the stored form of a session id.
func HashID(id string) []byte {
	sum := sha256.Sum256([]byte(id))
	return sum[:]
}

// SID is the hex prefix of a session hash that tokens and audit rows carry. It correlates a token
// with its session; it is a prefix of a hash of the id, so it is never usable as the credential.
func SID(hash []byte) string {
	h := hex.EncodeToString(hash)
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
