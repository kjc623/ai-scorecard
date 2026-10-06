// Package identitytest is an in-memory identity.Store for tests. It mirrors the SQL semantics that
// matter for security: the definer lookups by Entra tenant return active connections only,
// tenant-scoped reads see one tenant, an attempt is deleted as it is taken, and activation and
// pending-connection creation are all-or-nothing.
package identitytest

import (
	"context"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/identity"
	"github.com/shadow-ai-capture/control-api/internal/session"
)

// Store is the in-memory identity.Store.
type Store struct {
	mu       sync.Mutex
	tenants  map[string]*memTenant
	domains  map[string]string
	conns    map[string]identity.Connection
	invites  map[string]identity.Invite
	grants   map[string]string // tenant|conn|subject|role -> granted_by
	attempts map[string]identity.Attempt
	scim     map[string]bool   // tenant|user_ref -> active
	aliases  map[string]string // tenant|alias -> user_ref
	audit    []AuditRow
	now      func() time.Time
}

type memTenant struct {
	name       string
	userRefKey []byte
	status     string
	readClosed bool
}

// AuditRow is a recorded audit entry, for tests.
type AuditRow struct {
	TenantID string
	identity.AuditEntry
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		tenants: map[string]*memTenant{}, domains: map[string]string{}, conns: map[string]identity.Connection{},
		invites: map[string]identity.Invite{}, grants: map[string]string{}, attempts: map[string]identity.Attempt{},
		scim: map[string]bool{}, aliases: map[string]string{}, now: time.Now,
	}
}

// --- seeding ---

// AddTenant creates a tenant.
func (m *Store) AddTenant(id, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[id] = &memTenant{name: name, status: "active"}
}

// SetTenantAccess sets a tenant's status and read gate.
func (m *Store) SetTenantAccess(id, status string, readEnabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[id].status, m.tenants[id].readClosed = status, !readEnabled
}

// TenantAccess implements identity.Store.
func (m *Store) TenantAccess(_ context.Context, tenantID string) (identity.TenantAccess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return identity.TenantAccess{}, identity.ErrNotFound
	}
	return identity.TenantAccess{Status: t.status, ReadEnabled: !t.readClosed}, nil
}

// SetUserRefKey stores a tenant's sealed user-reference key.
func (m *Store) SetUserRefKey(tenantID string, sealed []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[tenantID].userRefKey = append([]byte(nil), sealed...)
}

// AddEmailDomain maps a domain to a tenant.
func (m *Store) AddEmailDomain(domain, tenantID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.domains[domain] = tenantID
}

// AddConnection stores a connection as given, assigning an id when it has none.
func (m *Store) AddConnection(c identity.Connection) identity.Connection {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		c.ID, _ = session.NewUUID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = m.now()
	}
	m.conns[c.ID] = c
	return c
}

// SetConnectionStatus changes a connection's status.
func (m *Store) SetConnectionStatus(id, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[id]
	c.Status = status
	m.conns[id] = c
}

// identity.Connection returns a connection by id.
func (m *Store) Connection(id string) (identity.Connection, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	return c, ok
}

// AddInvite stores an invite, assigning an id when it has none.
func (m *Store) AddInvite(inv identity.Invite) identity.Invite {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inv.ID == "" {
		inv.ID, _ = session.NewUUID()
	}
	m.invites[inv.ID] = inv
	return inv
}

// InviteState returns an invite by id.
func (m *Store) InviteState(id string) identity.Invite {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invites[id]
}

// AddRoleGrant records a grant.
func (m *Store) AddRoleGrant(tenantID, connectionID, subject, role, by string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[tenantID+"|"+connectionID+"|"+subject+"|"+role] = by
}

// SetScimUser records a SCIM-provisioned person.
func (m *Store) SetScimUser(tenantID, userRef string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scim[tenantID+"|"+userRef] = active
}

// DeleteScimUser removes one, as a SCIM DELETE does.
func (m *Store) DeleteScimUser(tenantID, userRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.scim, tenantID+"|"+userRef)
}

// AddAlias maps an alias ref to a canonical ref.
func (m *Store) AddAlias(tenantID, alias, userRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aliases[tenantID+"|"+alias] = userRef
}

// AuditRows returns every audit row written.
func (m *Store) AuditRows() []AuditRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditRow(nil), m.audit...)
}

// Attempts is the number of attempts held, for the sweep test.
func (m *Store) Attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.attempts)
}

// --- identity.Store ---

func (m *Store) ConnectionForEntraTenant(_ context.Context, tid string) (identity.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		if c.Provider == identity.ProviderEntra && c.EntraTenantID == tid && c.Status == identity.StatusActive {
			return c, nil
		}
	}
	return identity.Connection{}, identity.ErrNotFound
}

func (m *Store) ConnectionByID(_ context.Context, id string) (identity.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return identity.Connection{}, identity.ErrNotFound
	}
	return c, nil
}

func (m *Store) TenantForEmailDomain(_ context.Context, domain string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.domains[domain]
	if !ok {
		return "", identity.ErrNotFound
	}
	return t, nil
}

func (m *Store) PutAttempt(_ context.Context, a identity.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts[hex.EncodeToString(a.Hash)] = a
	return nil
}

func (m *Store) TakeAttempt(_ context.Context, hash []byte) (identity.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := hex.EncodeToString(hash)
	a, ok := m.attempts[k]
	if !ok {
		return identity.Attempt{}, identity.ErrNotFound
	}
	delete(m.attempts, k)
	return a, nil
}

func (m *Store) SweepAttempts(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, a := range m.attempts {
		if a.ExpiresAt.Before(before) {
			delete(m.attempts, k)
		}
	}
	return nil
}

func (m *Store) TenantConnections(_ context.Context, tenantID string) ([]identity.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []identity.Connection
	for _, c := range m.conns {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Store) TenantSummary(_ context.Context, tenantID string) (identity.TenantSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return identity.TenantSummary{}, identity.ErrNotFound
	}
	sum := identity.TenantSummary{Name: t.name}
	for d, owner := range m.domains {
		if owner == tenantID {
			sum.Domains = append(sum.Domains, d)
		}
	}
	sort.Strings(sum.Domains)
	return sum, nil
}

func (m *Store) Invite(_ context.Context, tenantID, tokenHash string) (identity.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.invites {
		if inv.TenantID == tenantID && inv.TokenHash == tokenHash {
			return inv, nil
		}
	}
	return identity.Invite{}, identity.ErrNotFound
}

func (m *Store) InviteByID(_ context.Context, tenantID, id string) (identity.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[id]
	if !ok || inv.TenantID != tenantID {
		return identity.Invite{}, identity.ErrNotFound
	}
	return inv, nil
}

func (m *Store) RoleGrants(_ context.Context, tenantID, connectionID, subject string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	prefix := tenantID + "|" + connectionID + "|" + subject + "|"
	for k := range m.grants {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k[len(prefix):])
		}
	}
	return out, nil
}

func (m *Store) UserRefKey(_ context.Context, tenantID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tenants[tenantID]; ok {
		return t.userRefKey, nil
	}
	return nil, nil
}

func (m *Store) ScimUser(_ context.Context, tenantID string, refs []string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ref := range refs {
		canonical := ref
		if c, ok := m.aliases[tenantID+"|"+ref]; ok {
			canonical = c
		}
		if active, ok := m.scim[tenantID+"|"+canonical]; ok {
			return canonical, active, nil
		}
	}
	return "", false, identity.ErrNotFound
}

func (m *Store) CreatePendingConnection(_ context.Context, nc identity.NewConnection, audit identity.AuditEntry) (identity.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	same := func(c identity.Connection) bool {
		if c.Provider != nc.Provider {
			return false
		}
		if nc.Provider == identity.ProviderEntra {
			return c.EntraTenantID == nc.EntraTenantID
		}
		return c.Issuer == nc.Issuer
	}
	for id, c := range m.conns {
		if !same(c) {
			continue
		}
		if c.TenantID != nc.TenantID {
			return identity.Connection{}, identity.ErrLinkedElsewhere
		}
		switch c.Status {
		case identity.StatusDisabled:
			return identity.Connection{}, identity.ErrConnectionDisabled
		case identity.StatusPending:
			if nc.Provider == identity.ProviderOIDC {
				c.ClientID, c.ClientSecretEnc = nc.ClientID, nc.ClientSecretEnc
				m.conns[id] = c
			}
		}
		return c, nil
	}
	id, _ := session.NewUUID()
	scopes := nc.Scopes
	if scopes == "" {
		scopes = "openid profile email"
	}
	c := identity.Connection{
		ID: id, TenantID: nc.TenantID, Provider: nc.Provider, EntraTenantID: nc.EntraTenantID, Issuer: nc.Issuer,
		ClientID: nc.ClientID, ClientSecretEnc: nc.ClientSecretEnc, Scopes: scopes, RolesClaim: "roles",
		Status: identity.StatusPending, CreatedAt: m.now(),
	}
	m.conns[id] = c
	audit.ObjectID = id
	m.audit = append(m.audit, AuditRow{TenantID: nc.TenantID, AuditEntry: audit})
	return c, nil
}

func (m *Store) Activate(_ context.Context, a identity.Activation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[a.InviteID]
	if !ok || inv.TenantID != a.TenantID || !inv.Live(a.At) {
		return identity.ErrInviteUsed
	}
	c, ok := m.conns[a.ConnectionID]
	if !ok || c.TenantID != a.TenantID || (c.Status != identity.StatusPending && c.Status != identity.StatusActive) {
		return identity.ErrConnectionDisabled
	}
	at := a.At
	inv.UsedAt, inv.UsedBy = &at, a.Actor
	m.invites[inv.ID] = inv
	if c.ActivatedAt == nil {
		c.ActivatedAt, c.ActivatedBy = &at, a.Actor
	}
	c.Status = identity.StatusActive
	m.conns[c.ID] = c
	m.grants[a.TenantID+"|"+a.ConnectionID+"|"+a.Subject+"|"+session.RoleAdmin] = "onboarding-invite:" + a.InviteID
	for _, e := range identity.ActivationAudit(a) {
		m.audit = append(m.audit, AuditRow{TenantID: a.TenantID, AuditEntry: e})
	}
	return nil
}

func (m *Store) Audit(_ context.Context, tenantID string, e identity.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, AuditRow{TenantID: tenantID, AuditEntry: e})
	return nil
}
