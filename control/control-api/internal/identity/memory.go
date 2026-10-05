package identity

import (
	"context"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/session"
)

// MemoryStore is the in-memory double of SQLStore, for tests and a local run. It mirrors the SQL
// semantics that matter for security: the definer lookups by Entra tenant return active connections
// only, tenant-scoped reads see one tenant, an attempt is deleted as it is taken, and activation and
// pending-connection creation are all-or-nothing.
type MemoryStore struct {
	mu       sync.Mutex
	tenants  map[string]*memTenant
	domains  map[string]string
	conns    map[string]Connection
	invites  map[string]Invite
	grants   map[string]string // tenant|conn|subject|role -> granted_by
	attempts map[string]Attempt
	scim     map[string]bool   // tenant|user_ref -> active
	aliases  map[string]string // tenant|alias -> user_ref
	audit    []AuditRow
	now      func() time.Time
}

type memTenant struct {
	name       string
	userRefKey []byte
}

// AuditRow is a recorded audit entry, for tests.
type AuditRow struct {
	TenantID string
	AuditEntry
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tenants: map[string]*memTenant{}, domains: map[string]string{}, conns: map[string]Connection{},
		invites: map[string]Invite{}, grants: map[string]string{}, attempts: map[string]Attempt{},
		scim: map[string]bool{}, aliases: map[string]string{}, now: time.Now,
	}
}

// --- seeding, for tests and a local run ---

// AddTenant creates a tenant.
func (m *MemoryStore) AddTenant(id, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[id] = &memTenant{name: name}
}

// SetUserRefKey stores a tenant's sealed user-reference key.
func (m *MemoryStore) SetUserRefKey(tenantID string, sealed []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[tenantID].userRefKey = append([]byte(nil), sealed...)
}

// AddEmailDomain maps a domain to a tenant.
func (m *MemoryStore) AddEmailDomain(domain, tenantID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.domains[domain] = tenantID
}

// AddConnection stores a connection as given, assigning an id when it has none.
func (m *MemoryStore) AddConnection(c Connection) Connection {
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
func (m *MemoryStore) SetConnectionStatus(id, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[id]
	c.Status = status
	m.conns[id] = c
}

// Connection returns a connection by id.
func (m *MemoryStore) Connection(id string) (Connection, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	return c, ok
}

// AddInvite stores an invite, assigning an id when it has none.
func (m *MemoryStore) AddInvite(inv Invite) Invite {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inv.ID == "" {
		inv.ID, _ = session.NewUUID()
	}
	m.invites[inv.ID] = inv
	return inv
}

// InviteState returns an invite by id.
func (m *MemoryStore) InviteState(id string) Invite {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invites[id]
}

// AddRoleGrant records a grant.
func (m *MemoryStore) AddRoleGrant(tenantID, connectionID, subject, role, by string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[tenantID+"|"+connectionID+"|"+subject+"|"+role] = by
}

// SetScimUser records a SCIM-provisioned person.
func (m *MemoryStore) SetScimUser(tenantID, userRef string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scim[tenantID+"|"+userRef] = active
}

// DeleteScimUser removes one, as a SCIM DELETE does.
func (m *MemoryStore) DeleteScimUser(tenantID, userRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.scim, tenantID+"|"+userRef)
}

// AddAlias maps an alias ref to a canonical ref.
func (m *MemoryStore) AddAlias(tenantID, alias, userRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aliases[tenantID+"|"+alias] = userRef
}

// AuditRows returns every audit row written.
func (m *MemoryStore) AuditRows() []AuditRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditRow(nil), m.audit...)
}

// Attempts is the number of attempts held, for the sweep test.
func (m *MemoryStore) Attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.attempts)
}

// --- Store ---

func (m *MemoryStore) ConnectionForEntraTenant(_ context.Context, tid string) (Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		if c.Provider == ProviderEntra && c.EntraTenantID == tid && c.Status == StatusActive {
			return c, nil
		}
	}
	return Connection{}, ErrNotFound
}

func (m *MemoryStore) ConnectionByID(_ context.Context, id string) (Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return Connection{}, ErrNotFound
	}
	return c, nil
}

func (m *MemoryStore) TenantForEmailDomain(_ context.Context, domain string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.domains[domain]
	if !ok {
		return "", ErrNotFound
	}
	return t, nil
}

func (m *MemoryStore) PutAttempt(_ context.Context, a Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts[hex.EncodeToString(a.Hash)] = a
	return nil
}

func (m *MemoryStore) TakeAttempt(_ context.Context, hash []byte) (Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := hex.EncodeToString(hash)
	a, ok := m.attempts[k]
	if !ok {
		return Attempt{}, ErrNotFound
	}
	delete(m.attempts, k)
	return a, nil
}

func (m *MemoryStore) SweepAttempts(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, a := range m.attempts {
		if a.ExpiresAt.Before(before) {
			delete(m.attempts, k)
		}
	}
	return nil
}

func (m *MemoryStore) TenantConnections(_ context.Context, tenantID string) ([]Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Connection
	for _, c := range m.conns {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryStore) TenantSummary(_ context.Context, tenantID string) (TenantSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return TenantSummary{}, ErrNotFound
	}
	sum := TenantSummary{Name: t.name}
	for d, owner := range m.domains {
		if owner == tenantID {
			sum.Domains = append(sum.Domains, d)
		}
	}
	sort.Strings(sum.Domains)
	return sum, nil
}

func (m *MemoryStore) Invite(_ context.Context, tenantID, tokenHash string) (Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.invites {
		if inv.TenantID == tenantID && inv.TokenHash == tokenHash {
			return inv, nil
		}
	}
	return Invite{}, ErrNotFound
}

func (m *MemoryStore) InviteByID(_ context.Context, tenantID, id string) (Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[id]
	if !ok || inv.TenantID != tenantID {
		return Invite{}, ErrNotFound
	}
	return inv, nil
}

func (m *MemoryStore) RoleGrants(_ context.Context, tenantID, connectionID, subject string) ([]string, error) {
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

func (m *MemoryStore) UserRefKey(_ context.Context, tenantID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tenants[tenantID]; ok {
		return t.userRefKey, nil
	}
	return nil, nil
}

func (m *MemoryStore) ScimUser(_ context.Context, tenantID string, refs []string) (string, bool, error) {
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
	return "", false, ErrNotFound
}

func (m *MemoryStore) CreatePendingConnection(_ context.Context, nc NewConnection, audit AuditEntry) (Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	same := func(c Connection) bool {
		if c.Provider != nc.Provider {
			return false
		}
		if nc.Provider == ProviderEntra {
			return c.EntraTenantID == nc.EntraTenantID
		}
		return c.Issuer == nc.Issuer
	}
	for id, c := range m.conns {
		if !same(c) {
			continue
		}
		if c.TenantID != nc.TenantID {
			return Connection{}, ErrLinkedElsewhere
		}
		switch c.Status {
		case StatusDisabled:
			return Connection{}, ErrConnectionDisabled
		case StatusPending:
			if nc.Provider == ProviderOIDC {
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
	c := Connection{
		ID: id, TenantID: nc.TenantID, Provider: nc.Provider, EntraTenantID: nc.EntraTenantID, Issuer: nc.Issuer,
		ClientID: nc.ClientID, ClientSecretEnc: nc.ClientSecretEnc, Scopes: scopes, RolesClaim: "roles",
		Status: StatusPending, CreatedAt: m.now(),
	}
	m.conns[id] = c
	audit.ObjectID = id
	m.audit = append(m.audit, AuditRow{TenantID: nc.TenantID, AuditEntry: audit})
	return c, nil
}

func (m *MemoryStore) Activate(_ context.Context, a Activation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[a.InviteID]
	if !ok || inv.TenantID != a.TenantID || !inv.Live(a.At) {
		return ErrInviteUsed
	}
	c, ok := m.conns[a.ConnectionID]
	if !ok || c.TenantID != a.TenantID || (c.Status != StatusPending && c.Status != StatusActive) {
		return ErrConnectionDisabled
	}
	at := a.At
	inv.UsedAt, inv.UsedBy = &at, a.Actor
	m.invites[inv.ID] = inv
	if c.ActivatedAt == nil {
		c.ActivatedAt, c.ActivatedBy = &at, a.Actor
	}
	c.Status = StatusActive
	m.conns[c.ID] = c
	m.grants[a.TenantID+"|"+a.ConnectionID+"|"+a.Subject+"|"+session.RoleAdmin] = "onboarding-invite:" + a.InviteID
	for _, e := range activationAudit(a) {
		m.audit = append(m.audit, AuditRow{TenantID: a.TenantID, AuditEntry: e})
	}
	return nil
}

func (m *MemoryStore) Audit(_ context.Context, tenantID string, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, AuditRow{TenantID: tenantID, AuditEntry: e})
	return nil
}

// activationAudit is the three rows an activation commits, shared by both stores so they cannot
// drift: the invite spent, the connection activated, the first admin granted — each attributed to
// the person whose sign-in did it.
func activationAudit(a Activation) []AuditEntry {
	return []AuditEntry{
		{ActorType: "user", ActorID: a.Actor, Action: "onboarding_invite.use", ObjectType: "onboarding_invite", ObjectID: a.InviteID},
		{ActorType: "user", ActorID: a.Actor, Action: "identity_connection.activate", ObjectType: "identity_connection",
			ObjectID: a.ConnectionID, Detail: map[string]any{"invite_id": a.InviteID}},
		{ActorType: "user", ActorID: a.Actor, Action: "role.grant", ObjectType: "role_grant", ObjectID: a.ConnectionID + ":" + a.Subject,
			Detail: map[string]any{"role": session.RoleAdmin, "granted_by": "onboarding-invite:" + a.InviteID}},
	}
}
