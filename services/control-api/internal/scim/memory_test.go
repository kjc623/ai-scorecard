package scim

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is the in-memory Store the tests run against. It keeps the
// properties the SQL store gets from the database: one tenant's transaction cannot see another
// tenant's rows, a failed transaction leaves nothing behind, and userName hashes are unique per
// tenant. Transactions are serialised, which is stricter than the database and harmless here.
type Memory struct {
	mu      sync.Mutex
	tenants map[string]*memTenant
}

type memAlias struct {
	canonical string
	at        time.Time
}

type memTenant struct {
	deviceIdentity string
	tokens         map[string]TokenRow
	users          map[string]UserRow
	aliases        map[string]memAlias
	dim            map[string]UserDimRow
	groups         map[string]GroupRow
	members        map[string]map[string]bool
	audit          []AuditEntry
}

// NewMemory returns an empty store with no tenants.
func NewMemory() *Memory { return &Memory{tenants: map[string]*memTenant{}} }

// AddTenant makes a tenant known, with its device_identity setting ('clear' or 'hashed').
func (m *Memory) AddTenant(tenantID, deviceIdentity string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[strings.ToLower(tenantID)] = &memTenant{
		deviceIdentity: deviceIdentity,
		tokens:         map[string]TokenRow{}, users: map[string]UserRow{}, aliases: map[string]memAlias{},
		dim: map[string]UserDimRow{}, groups: map[string]GroupRow{}, members: map[string]map[string]bool{},
	}
}

// TenantForToken implements Store: the definer function's view, across tenants.
func (m *Memory) TenantForToken(_ context.Context, tokenHash string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.tenants {
		for _, tok := range t.tokens {
			if tok.Hash == tokenHash && tok.RevokedAt == nil {
				return id, nil
			}
		}
	}
	return "", nil
}

// InTenant implements Store.
func (m *Memory) InTenant(_ context.Context, tenantID string, fn func(Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[strings.ToLower(tenantID)]
	if !ok {
		return ErrUnknownTenant
	}
	snapshot := t.clone()
	if err := fn(&memTx{t: t}); err != nil {
		*t = *snapshot
		return err
	}
	return nil
}

// Aliases returns the tenant's alias table (alias -> canonical), for inspection.
func (m *Memory) Aliases(tenantID string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	if t, ok := m.tenants[strings.ToLower(tenantID)]; ok {
		for a, v := range t.aliases {
			out[a] = v.canonical
		}
	}
	return out
}

// UserDim returns one ops.user_dim row, for inspection.
func (m *Memory) UserDim(tenantID, userRef string) (UserDimRow, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[strings.ToLower(tenantID)]
	if !ok {
		return UserDimRow{}, false
	}
	row, ok := t.dim[userRef]
	return row, ok
}

// AuditLog returns the tenant's audit rows in order, for inspection.
func (m *Memory) AuditLog(tenantID string) []AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tenants[strings.ToLower(tenantID)]; ok {
		return append([]AuditEntry(nil), t.audit...)
	}
	return nil
}

// StoredUsers returns the tenant's scim_user rows, for inspection.
func (m *Memory) StoredUsers(tenantID string) []UserRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[strings.ToLower(tenantID)]
	if !ok {
		return nil
	}
	return sortedUsers(t.users)
}

func (t *memTenant) clone() *memTenant {
	c := &memTenant{
		deviceIdentity: t.deviceIdentity,
		tokens:         map[string]TokenRow{}, users: map[string]UserRow{}, aliases: map[string]memAlias{},
		dim: map[string]UserDimRow{}, groups: map[string]GroupRow{}, members: map[string]map[string]bool{},
		audit: append([]AuditEntry(nil), t.audit...),
	}
	for k, v := range t.tokens {
		c.tokens[k] = v
	}
	for k, v := range t.users {
		c.users[k] = v
	}
	for k, v := range t.aliases {
		c.aliases[k] = v
	}
	for k, v := range t.dim {
		c.dim[k] = v
	}
	for k, v := range t.groups {
		c.groups[k] = v
	}
	for g, set := range t.members {
		cs := map[string]bool{}
		for u := range set {
			cs[u] = true
		}
		c.members[g] = cs
	}
	return c
}

func sortedUsers(users map[string]UserRow) []UserRow {
	out := make([]UserRow, 0, len(users))
	for _, u := range users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func window[T any](all []T, offset, limit int) []T {
	if offset >= len(all) || limit <= 0 {
		return nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end]
}

type memTx struct{ t *memTenant }

func (x *memTx) DeviceIdentity() (string, error) { return x.t.deviceIdentity, nil }

func (x *memTx) TokenByHash(hash string) (TokenRow, error) {
	for _, tok := range x.t.tokens {
		if tok.Hash == hash {
			return tok, nil
		}
	}
	return TokenRow{}, ErrNotFound
}

func (x *memTx) Token(id string) (TokenRow, error) {
	tok, ok := x.t.tokens[id]
	if !ok {
		return TokenRow{}, ErrNotFound
	}
	return tok, nil
}

func (x *memTx) InsertToken(row TokenRow) error {
	for _, tok := range x.t.tokens {
		if tok.Hash == row.Hash {
			return ErrConflict
		}
	}
	x.t.tokens[row.ID] = row
	return nil
}

func (x *memTx) RevokeToken(id string, at time.Time) (bool, error) {
	tok, ok := x.t.tokens[id]
	if !ok || tok.RevokedAt != nil {
		return false, nil
	}
	tok.RevokedAt = &at
	x.t.tokens[id] = tok
	return true, nil
}

func (x *memTx) ListTokens() ([]TokenRow, error) {
	out := make([]TokenRow, 0, len(x.t.tokens))
	for _, tok := range x.t.tokens {
		out = append(out, tok)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (x *memTx) User(id string, _ bool) (UserRow, error) {
	u, ok := x.t.users[id]
	if !ok {
		return UserRow{}, ErrNotFound
	}
	return u, nil
}

func (x *memTx) usersWhere(pred func(UserRow) bool) []UserRow {
	var out []UserRow
	for _, u := range sortedUsers(x.t.users) {
		if pred(u) {
			out = append(out, u)
		}
	}
	return out
}

func (x *memTx) UsersByUserNameHash(hash []byte) ([]UserRow, error) {
	return x.usersWhere(func(u UserRow) bool { return bytes.Equal(u.UserNameHash, hash) }), nil
}

func (x *memTx) UsersByExternalIDHash(hash []byte) ([]UserRow, error) {
	return x.usersWhere(func(u UserRow) bool { return len(hash) > 0 && bytes.Equal(u.ExternalIDHash, hash) }), nil
}

func (x *memTx) ListUsers(offset, limit int) ([]UserRow, error) {
	return window(sortedUsers(x.t.users), offset, limit), nil
}

func (x *memTx) CountUsers() (int, error) { return len(x.t.users), nil }

func (x *memTx) UserRefOwner(ref string) (string, error) {
	for _, u := range sortedUsers(x.t.users) {
		if u.UserRef == ref {
			return u.ID, nil
		}
	}
	return "", nil
}

func (x *memTx) InsertUser(row UserRow) error {
	for _, u := range x.t.users {
		// The schema's two uniques: a userName hash, and a canonical ref (two people sharing one
		// would merge their histories).
		if bytes.Equal(u.UserNameHash, row.UserNameHash) || u.UserRef == row.UserRef {
			return ErrConflict
		}
	}
	x.t.users[row.ID] = row
	return nil
}

func (x *memTx) UpdateUser(row UserRow) error {
	if _, ok := x.t.users[row.ID]; !ok {
		return ErrNotFound
	}
	for id, u := range x.t.users {
		if id != row.ID && bytes.Equal(u.UserNameHash, row.UserNameHash) {
			return ErrConflict
		}
	}
	x.t.users[row.ID] = row
	return nil
}

func (x *memTx) PutAlias(alias, canonical string, at time.Time) error {
	if cur, ok := x.t.aliases[alias]; ok && cur.canonical == canonical {
		return nil
	}
	x.t.aliases[alias] = memAlias{canonical: canonical, at: at}
	return nil
}

func (x *memTx) UpsertUserDim(row UserDimRow) error {
	if prev, ok := x.t.dim[row.UserRef]; ok && row.DirectoryObjectIDEnc == nil {
		row.DirectoryObjectIDEnc = prev.DirectoryObjectIDEnc
	}
	x.t.dim[row.UserRef] = row
	return nil
}

func (x *memTx) Group(id string, _ bool) (GroupRow, error) {
	g, ok := x.t.groups[id]
	if !ok {
		return GroupRow{}, ErrNotFound
	}
	return g, nil
}

func (x *memTx) sortedGroups(pred func(GroupRow) bool) []GroupRow {
	var out []GroupRow
	for _, g := range x.t.groups {
		if pred == nil || pred(g) {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (x *memTx) GroupsByDisplayName(name string) ([]GroupRow, error) {
	return x.sortedGroups(func(g GroupRow) bool { return strings.EqualFold(g.DisplayName, name) }), nil
}

func (x *memTx) GroupsByExternalID(externalID string) ([]GroupRow, error) {
	return x.sortedGroups(func(g GroupRow) bool { return g.ExternalID != "" && g.ExternalID == externalID }), nil
}

func (x *memTx) ListGroups(offset, limit int) ([]GroupRow, error) {
	return window(x.sortedGroups(nil), offset, limit), nil
}

func (x *memTx) CountGroups() (int, error) { return len(x.t.groups), nil }

func (x *memTx) InsertGroup(row GroupRow) error {
	x.t.groups[row.ID] = row
	x.t.members[row.ID] = map[string]bool{}
	return nil
}

func (x *memTx) UpdateGroup(row GroupRow) error {
	if _, ok := x.t.groups[row.ID]; !ok {
		return ErrNotFound
	}
	x.t.groups[row.ID] = row
	return nil
}

func (x *memTx) DeleteGroup(id string) error {
	delete(x.t.groups, id)
	delete(x.t.members, id)
	return nil
}

func (x *memTx) Members(groupID string) ([]string, error) {
	var out []string
	for u := range x.t.members[groupID] {
		out = append(out, u)
	}
	sort.Strings(out)
	return out, nil
}

func (x *memTx) AddMember(groupID, userID string) (bool, error) {
	if _, ok := x.t.users[userID]; !ok {
		return false, nil
	}
	set, ok := x.t.members[groupID]
	if !ok || set[userID] {
		return false, nil
	}
	set[userID] = true
	return true, nil
}

func (x *memTx) RemoveMember(groupID, userID string) (bool, error) {
	set := x.t.members[groupID]
	if !set[userID] {
		return false, nil
	}
	delete(set, userID)
	return true, nil
}

func (x *memTx) Audit(e AuditEntry) error {
	x.t.audit = append(x.t.audit, e)
	return nil
}
