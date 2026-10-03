package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Memory is the in-memory Store used by the service tests and by a local run. Every branch below is
// a transliteration of the statements in sql.go: a device is found by the same partial-index key, a
// rotation revokes the live credential before inserting the new one in the same critical section,
// and a jti is a one-shot until its window expires. The live test checks the SQL text against a real
// server, so the two cannot drift without a test noticing.
type Memory struct {
	mu        sync.Mutex
	tenants   map[string]Tenant
	tokens    map[string]EnrolmentToken
	devices   map[string]Device
	hwidIndex map[string]string
	creds     map[string]Credential
	liveCred  map[string]string
	now       func() time.Time
}

// NewMemory builds an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		tenants:   map[string]Tenant{},
		tokens:    map[string]EnrolmentToken{},
		devices:   map[string]Device{},
		hwidIndex: map[string]string{},
		creds:     map[string]Credential{},
		liveCred:  map[string]string{},
		now:       time.Now,
	}
}

// SetNow overrides the clock so tests can drive token expiry and device enrolment timestamps
// deterministically.
func (m *Memory) SetNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// AddTenant registers a tenant as a database seed would.
func (m *Memory) AddTenant(t Tenant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[t.TenantID] = t
}

// AddEnrolmentToken registers a token row. The hash is the stored value; the caller has already
// hashed the plaintext.
func (m *Memory) AddEnrolmentToken(t EnrolmentToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[tokenKey(t.TenantID, t.TokenHash)] = t
}

// AddDevice registers a device row and its hardware-identity index entry.
func (m *Memory) AddDevice(d Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices[deviceKey(d.TenantID, d.DeviceID)] = d
	if d.HardwareIdentityHash != "" {
		m.hwidIndex[hwidKey(d.TenantID, d.HardwareIdentityHash)] = d.DeviceID
	}
}

// AddCredential registers a credential and makes it the device's live one unless it is revoked.
func (m *Memory) AddCredential(c Credential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds[credKey(c.TenantID, c.CredentialID)] = c
	if c.RevokedAt == nil {
		m.liveCred[deviceKey(c.TenantID, c.DeviceID)] = c.CredentialID
	}
}

// RevokeDevice flips a device to revoked, as an operator action would.
func (m *Memory) RevokeDevice(tenantID, deviceID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.devices[deviceKey(tenantID, deviceID)]
	d.RevokedAt = &at
	m.devices[deviceKey(tenantID, deviceID)] = d
}

// RevokeCredential flips a credential to revoked and clears it as the live one.
func (m *Memory) RevokeCredential(tenantID, credentialID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.creds[credKey(tenantID, credentialID)]
	c.RevokedAt = &at
	m.creds[credKey(tenantID, credentialID)] = c
	if live := m.liveCred[deviceKey(tenantID, c.DeviceID)]; live == credentialID {
		delete(m.liveCred, deviceKey(tenantID, c.DeviceID))
	}
}

// Devices returns a copy of every registered device, for assertions.
func (m *Memory) Devices() []Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	return out
}

// Credentials returns a copy of every registered credential, for assertions.
func (m *Memory) Credentials() []Credential {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Credential, 0, len(m.creds))
	for _, c := range m.creds {
		out = append(out, c)
	}
	return out
}

// Close implements Store.
func (m *Memory) Close() error { return nil }

// Tenant implements Store.
func (m *Memory) Tenant(_ context.Context, tenantID string) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return Tenant{}, ErrUnknownTenant
	}
	return t, nil
}

// ResolveEnrolmentToken implements Store.
func (m *Memory) ResolveEnrolmentToken(_ context.Context, tenantID, tokenHash string) (EnrolmentToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[tokenKey(tenantID, tokenHash)]
	if !ok {
		return EnrolmentToken{}, ErrTokenUnknown
	}
	return t, nil
}

// FindDeviceByHardwareIdentity implements Store.
func (m *Memory) FindDeviceByHardwareIdentity(_ context.Context, tenantID, hardwareIdentityHash string) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deviceID, ok := m.hwidIndex[hwidKey(tenantID, hardwareIdentityHash)]
	if !ok {
		return Device{}, ErrDeviceUnknown
	}
	d, ok := m.devices[deviceKey(tenantID, deviceID)]
	if !ok {
		return Device{}, ErrDeviceUnknown
	}
	return d, nil
}

// Device implements Store.
func (m *Memory) Device(_ context.Context, tenantID, deviceID string) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[deviceKey(tenantID, deviceID)]
	if !ok {
		return Device{}, ErrDeviceUnknown
	}
	return d, nil
}

// UpsertDevice implements Store.
func (m *Memory) UpsertDevice(_ context.Context, d Device) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := deviceKey(d.TenantID, d.DeviceID)
	if existing, ok := m.devices[key]; ok {
		d.EnrolledAt = existing.EnrolledAt
		d.RevokedAt = existing.RevokedAt
		if d.HardwareIdentityHash == "" {
			d.HardwareIdentityHash = existing.HardwareIdentityHash
		}
		if d.ResidencyRegion == "" {
			d.ResidencyRegion = existing.ResidencyRegion
		}
	} else if d.EnrolledAt.IsZero() {
		d.EnrolledAt = m.now()
	}
	m.devices[key] = d
	if d.HardwareIdentityHash != "" {
		m.hwidIndex[hwidKey(d.TenantID, d.HardwareIdentityHash)] = d.DeviceID
	}
	return d, nil
}

// IssueCredential implements Store: revoke the live credential and insert the new one under the
// same lock, so the two are one atomic step here exactly as they are one transaction in SQL.
func (m *Memory) IssueCredential(_ context.Context, in IssueCredential) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, c := range m.creds {
		if c.TenantID == in.TenantID && c.DeviceID == in.DeviceID && c.RevokedAt == nil {
			at := in.RevokedAt
			c.RevokedAt = &at
			m.creds[key] = c
		}
	}
	c := Credential{
		TenantID:            in.TenantID,
		CredentialID:        in.CredentialID,
		DeviceID:            in.DeviceID,
		Type:                in.Type,
		PublicKeyThumbprint: in.PublicKeyThumbprint,
		PublicKeyJWK:        append(json.RawMessage(nil), in.PublicKeyJWK...),
		IssuedAt:            in.IssuedAt,
		ExpiresAt:           in.ExpiresAt,
	}
	m.creds[credKey(in.TenantID, in.CredentialID)] = c
	m.liveCred[deviceKey(in.TenantID, in.DeviceID)] = in.CredentialID
	return c, nil
}

// DeviceCredential implements Store.
func (m *Memory) DeviceCredential(_ context.Context, tenantID, credentialID string) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[credKey(tenantID, credentialID)]
	if !ok {
		return Credential{}, ErrCredentialUnknown
	}
	return c, nil
}

// DeviceCredentialByDevice implements Store.
func (m *Memory) DeviceCredentialByDevice(_ context.Context, tenantID, deviceID string) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	credentialID, ok := m.liveCred[deviceKey(tenantID, deviceID)]
	if !ok {
		return Credential{}, ErrCredentialUnknown
	}
	c, ok := m.creds[credKey(tenantID, credentialID)]
	if !ok {
		return Credential{}, ErrCredentialUnknown
	}
	return c, nil
}

// MarkEnrolmentTokenUsed implements Store.
func (m *Memory) MarkEnrolmentTokenUsed(_ context.Context, tenantID, tokenHash string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tokenKey(tenantID, tokenHash)
	t, ok := m.tokens[key]
	if !ok {
		return ErrTokenUnknown
	}
	if t.UsedAt != nil {
		return ErrTokenUsed
	}
	t.UsedAt = &at
	m.tokens[key] = t
	return nil
}

func tokenKey(tenantID, hash string) string { return tenantID + "|" + hash }
func deviceKey(tenantID, deviceID string) string {
	return tenantID + "|" + deviceID
}
func hwidKey(tenantID, hwid string) string { return tenantID + "|" + hwid }
func credKey(tenantID, credentialID string) string {
	return tenantID + "|" + credentialID
}

// String is a small convenience so a seed helper can name a store in a failure message.
func (m *Memory) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("memory(tenants=%d devices=%d credentials=%d tokens=%d)",
		len(m.tenants), len(m.devices), len(m.creds), len(m.tokens))
}
