package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
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
	// collectors is the ref.collector vocabulary; collectorState and lastSeen mirror
	// ops.collector_state and ops.device.last_seen_at for the health channel. collectorReportAt is
	// the stored last_report_at, which the stale-report guard compares against.
	collectors        map[string]bool
	collectorState    map[string]CollectorState
	collectorReportAt map[string]time.Time
	lastSeen          map[string]time.Time
	// dep mirrors the deployment and policy tables (memory_deployment.go).
	dep deploymentTables
}

// seededCollectors is the ref.collector seed in database/schema.sql. A test may override it with
// SetCollectors; the default keeps the double honest about what the schema holds.
var seededCollectors = map[string]bool{
	"capture_extension": true, "egress_proxy": true, "loopback_broker": true,
	"cli_shim": true, "process_detector": true, "classifier_host": true,
}

// NewMemory builds an empty in-memory store.
func NewMemory() *Memory {
	collectors := make(map[string]bool, len(seededCollectors))
	for code := range seededCollectors {
		collectors[code] = true
	}
	return &Memory{
		tenants:           map[string]Tenant{},
		tokens:            map[string]EnrolmentToken{},
		devices:           map[string]Device{},
		hwidIndex:         map[string]string{},
		creds:             map[string]Credential{},
		liveCred:          map[string]string{},
		now:               time.Now,
		collectors:        collectors,
		collectorState:    map[string]CollectorState{},
		collectorReportAt: map[string]time.Time{},
		lastSeen:          map[string]time.Time{},
		dep:               newDeploymentTables(),
	}
}

// SetCollectors replaces the collector vocabulary, for a test that wants to drive the unknown-name
// refusal.
func (m *Memory) SetCollectors(codes ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectors = map[string]bool{}
	for _, c := range codes {
		m.collectors[c] = true
	}
}

// AddCollector registers one collector in the vocabulary.
func (m *Memory) AddCollector(code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectors[code] = true
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
	if existing, ok := m.devices[key]; ok {
		// The Intune binding is written only by SetDeviceIntuneID, as the SQL upsert never touches
		// the column.
		d.IntuneDeviceID = existing.IntuneDeviceID
	} else {
		d.IntuneDeviceID = ""
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

// RecordHealth implements Store, mirroring the SQL transaction: validate the collector vocabulary,
// upsert each row if it is newer, and stamp last_seen_at monotonically. The whole call is under one
// lock, so a partly-invalid report changes nothing.
func (m *Memory) RecordHealth(_ context.Context, tenantID, deviceID string, at time.Time, reports []CollectorState, dev DeviceHealth) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range reports {
		if !m.collectors[r.Collector] {
			return fmt.Errorf("%w: %q", ErrUnknownCollector, r.Collector)
		}
	}
	for _, r := range reports {
		key := collectorKey(tenantID, deviceID, r.Collector)
		if prev, ok := m.collectorReportAt[key]; ok && !prev.Before(at) {
			continue // a stale or equal report never overwrites a newer one
		}
		m.collectorState[key] = r
		m.collectorReportAt[key] = at
	}
	seenKey := deviceKey(tenantID, deviceID)
	fresh := true
	if prev, ok := m.lastSeen[seenKey]; ok && !prev.Before(at) {
		fresh = false
	} else {
		m.lastSeen[seenKey] = at
	}
	// Device identity fields are applied only for a fresh report, matching the SQL guard, and the
	// clear hostname is gated on the tenant's setting so the double mirrors the server's authority.
	if fresh {
		if d, ok := m.devices[seenKey]; ok {
			identity := protocol.DeviceIdentityClear
			if t, ok := m.tenants[tenantID]; ok && t.DeviceIdentity != "" {
				identity = t.DeviceIdentity
			}
			if identity == protocol.DeviceIdentityHashed {
				if dev.HostnameHash != "" {
					d.HostnameHash = dev.HostnameHash
				}
			} else if dev.Hostname != "" {
				d.Hostname = dev.Hostname
			}
			if dev.AgentVersion != "" {
				d.AgentVersion = dev.AgentVersion
			}
			if dev.ManagedState != "" {
				d.ManagedState = dev.ManagedState
			}
			// collection_mode has no field on Device; the read path reads it from the column.
			m.devices[seenKey] = d
		}
	}
	return nil
}

// CollectorStateAt returns the stored health row, for tests.
func (m *Memory) CollectorStateAt(tenantID, deviceID, collector string) (CollectorState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.collectorState[collectorKey(tenantID, deviceID, collector)]
	return r, ok
}

// CollectorReportAt returns the row's last_report_at, for tests.
func (m *Memory) CollectorReportAt(tenantID, deviceID, collector string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.collectorReportAt[collectorKey(tenantID, deviceID, collector)]
	return t, ok
}

// DeviceLastSeenAt returns the stamped device activity, for tests.
func (m *Memory) DeviceLastSeenAt(tenantID, deviceID string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastSeen[deviceKey(tenantID, deviceID)]
	return t, ok
}

func collectorKey(tenantID, deviceID, collector string) string {
	return tenantID + "|" + deviceID + "|" + collector
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
