// Package storetest is an in-memory store.Store for tests of the packages that use the store. It
// follows the SQL implementation's semantics: the same idempotency keys, a rotation that revokes
// the live credential in the same step as the insert, a stale health report that changes nothing,
// the first revocation of a deployment key standing, and per-tenant uniqueness of an Intune id.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Memory is the in-memory store.
type Memory struct {
	mu      sync.Mutex
	now     func() time.Time
	tenants map[string]store.Tenant
	devices map[string]store.Device
	creds   map[string]store.Credential

	collectors        map[string]bool
	collectorState    map[string]store.CollectorState
	collectorReportAt map[string]time.Time
	lastSeen          map[string]time.Time

	keys         map[string]store.DeploymentKey
	verification map[string]string
	connections  map[string][]store.IdentityConnection
	audits       []store.AuditEntry
	ceilings     map[string]string
	hosts        []string
	bundles      map[string][]store.PolicyBundle
	scimUsers    map[string]int64
	scimGroups   map[string]int64
	scimLast     map[string]time.Time
}

var _ store.Store = (*Memory)(nil)

// New returns an empty store whose collector vocabulary is the schema's seed.
func New() *Memory {
	m := &Memory{
		now:               time.Now,
		tenants:           map[string]store.Tenant{},
		devices:           map[string]store.Device{},
		creds:             map[string]store.Credential{},
		collectors:        map[string]bool{},
		collectorState:    map[string]store.CollectorState{},
		collectorReportAt: map[string]time.Time{},
		lastSeen:          map[string]time.Time{},
		keys:              map[string]store.DeploymentKey{},
		verification:      map[string]string{},
		connections:       map[string][]store.IdentityConnection{},
		ceilings:          map[string]string{},
		bundles:           map[string][]store.PolicyBundle{},
		scimUsers:         map[string]int64{},
		scimGroups:        map[string]int64{},
		scimLast:          map[string]time.Time{},
	}
	for _, c := range []string{"capture_extension", "egress_proxy", "loopback_broker", "cli_shim", "process_detector", "classifier_host"} {
		m.collectors[c] = true
	}
	return m
}

func key(parts ...string) string { return strings.Join(parts, "|") }

// SetNow overrides the clock that stamps enrolled_at.
func (m *Memory) SetNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// AddTenant seeds a tenant.
func (m *Memory) AddTenant(t store.Tenant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[t.TenantID] = t
}

// AddDevice seeds a device.
func (m *Memory) AddDevice(d store.Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices[key(d.TenantID, d.DeviceID)] = d
}

// AddCredential seeds a credential.
func (m *Memory) AddCredential(c store.Credential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds[key(c.TenantID, c.CredentialID)] = c
}

// RevokeDevice marks a device revoked.
func (m *Memory) RevokeDevice(tenantID, deviceID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.devices[key(tenantID, deviceID)]
	d.RevokedAt = &at
	m.devices[key(tenantID, deviceID)] = d
}

// RevokeCredential marks a credential revoked.
func (m *Memory) RevokeCredential(tenantID, credentialID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.creds[key(tenantID, credentialID)]
	c.RevokedAt = &at
	m.creds[key(tenantID, credentialID)] = c
}

// Devices returns every device.
func (m *Memory) Devices() []store.Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	return out
}

// Credentials returns every credential.
func (m *Memory) Credentials() []store.Credential {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Credential, 0, len(m.creds))
	for _, c := range m.creds {
		out = append(out, c)
	}
	return out
}

// SetCollectors replaces the collector vocabulary.
func (m *Memory) SetCollectors(codes ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectors = map[string]bool{}
	for _, c := range codes {
		m.collectors[c] = true
	}
}

// CollectorStateAt returns a stored health row.
func (m *Memory) CollectorStateAt(tenantID, deviceID, collector string) (store.CollectorState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.collectorState[key(tenantID, deviceID, collector)]
	return r, ok
}

// CollectorReportAt returns a stored health row's last_report_at.
func (m *Memory) CollectorReportAt(tenantID, deviceID, collector string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.collectorReportAt[key(tenantID, deviceID, collector)]
	return t, ok
}

// DeviceLastSeenAt returns a device's last_seen_at.
func (m *Memory) DeviceLastSeenAt(tenantID, deviceID string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastSeen[key(tenantID, deviceID)]
	return t, ok
}

// AddDeploymentKey seeds a deployment key.
func (m *Memory) AddDeploymentKey(k store.DeploymentKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key(k.TenantID, k.KeyID)] = k
}

// DeploymentKeys returns the tenant's keys, newest first.
func (m *Memory) DeploymentKeys(tenantID string) []store.DeploymentKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keysLocked(tenantID)
}

func (m *Memory) keysLocked(tenantID string) []store.DeploymentKey {
	var out []store.DeploymentKey
	for _, k := range m.keys {
		if k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].KeyID < out[j].KeyID
	})
	return out
}

// SetVerification seeds the tenant's device_verification.
func (m *Memory) SetVerification(tenantID, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.verification[tenantID] = mode
}

// AddIdentityConnection seeds an identity connection.
func (m *Memory) AddIdentityConnection(tenantID string, c store.IdentityConnection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connections[tenantID] = append(m.connections[tenantID], c)
}

// SetCeiling seeds the tenant's ceiling_mode.
func (m *Memory) SetCeiling(tenantID, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ceilings[tenantID] = mode
}

// SetCatalogueHosts replaces the tool catalogue's TLS hosts.
func (m *Memory) SetCatalogueHosts(hosts ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hosts = append([]string(nil), hosts...)
}

// SetScimSummary seeds the SCIM counts the admin page reads.
func (m *Memory) SetScimSummary(tenantID string, users, groups int64, last time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scimUsers[tenantID], m.scimGroups[tenantID], m.scimLast[tenantID] = users, groups, last
}

// Audits returns every audit row written.
func (m *Memory) Audits() []store.AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.AuditEntry(nil), m.audits...)
}

// PolicyBundles returns the tenant's bundles in ascending version.
func (m *Memory) PolicyBundles(tenantID string) []store.PolicyBundle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.PolicyBundle(nil), m.bundles[tenantID]...)
}

// AddPolicyBundle seeds a bundle row.
func (m *Memory) AddPolicyBundle(b store.PolicyBundle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := append(m.bundles[b.TenantID], b)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Version < rows[j].Version })
	m.bundles[b.TenantID] = rows
}

// Ping implements store.Store.
func (m *Memory) Ping(context.Context) error { return nil }

// Tenant implements store.Store.
func (m *Memory) Tenant(_ context.Context, tenantID string) (store.Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return store.Tenant{}, store.ErrUnknownTenant
	}
	return t, nil
}

// FindDeviceByHardwareIdentity implements store.Store.
func (m *Memory) FindDeviceByHardwareIdentity(_ context.Context, tenantID, hwid string) (store.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.devices {
		if d.TenantID == tenantID && d.HardwareIdentityHash == hwid {
			return d, nil
		}
	}
	return store.Device{}, store.ErrDeviceUnknown
}

// Device implements store.Store.
func (m *Memory) Device(_ context.Context, tenantID, deviceID string) (store.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[key(tenantID, deviceID)]
	if !ok {
		return store.Device{}, store.ErrDeviceUnknown
	}
	return d, nil
}

// UpsertDevice implements store.Store.
func (m *Memory) UpsertDevice(_ context.Context, d store.Device) (store.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(d.TenantID, d.DeviceID)
	if existing, ok := m.devices[k]; ok {
		d.EnrolledAt, d.RevokedAt, d.IntuneDeviceID = existing.EnrolledAt, existing.RevokedAt, existing.IntuneDeviceID
		keep := func(v *string, old string) {
			if *v == "" {
				*v = old
			}
		}
		keep(&d.OSVersion, existing.OSVersion)
		keep(&d.HardwareIdentityHash, existing.HardwareIdentityHash)
		keep(&d.Hostname, existing.Hostname)
		keep(&d.HostnameHash, existing.HostnameHash)
		keep(&d.AgentVersion, existing.AgentVersion)
		keep(&d.ManagedState, existing.ManagedState)
		keep(&d.ResidencyRegion, existing.ResidencyRegion)
	} else {
		d.EnrolledAt, d.IntuneDeviceID = m.now().UTC(), ""
		if d.ManagedState == "" {
			d.ManagedState = string(protocol.ManagedStateUnknown)
		}
	}
	if d.HardwareIdentityHash != "" {
		for other, o := range m.devices {
			if other != k && o.TenantID == d.TenantID && o.HardwareIdentityHash == d.HardwareIdentityHash {
				return store.Device{}, errors.New("storetest: duplicate hardware identity")
			}
		}
	}
	m.devices[k] = d
	return d, nil
}

// IssueCredential implements store.Store.
func (m *Memory) IssueCredential(_ context.Context, c store.Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.creds[key(c.TenantID, c.CredentialID)]; ok {
		return errors.New("storetest: duplicate credential id")
	}
	for k, live := range m.creds {
		if live.TenantID == c.TenantID && live.DeviceID == c.DeviceID && live.RevokedAt == nil {
			at := c.IssuedAt
			live.RevokedAt = &at
			m.creds[k] = live
		}
	}
	c.RevokedAt = nil
	m.creds[key(c.TenantID, c.CredentialID)] = c
	return nil
}

// DeviceCredential implements store.Store.
func (m *Memory) DeviceCredential(_ context.Context, tenantID, credentialID string) (store.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[key(tenantID, credentialID)]
	if !ok {
		return store.Credential{}, store.ErrCredentialUnknown
	}
	return c, nil
}

// RecordHealth implements store.Store.
func (m *Memory) RecordHealth(_ context.Context, tenantID, deviceID string, at time.Time, reports []store.CollectorState, dev store.DeviceHealth) error {
	if len(reports) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range reports {
		if !m.collectors[r.Collector] {
			return fmt.Errorf("%w: %q", store.ErrUnknownCollector, r.Collector)
		}
	}
	t, ok := m.tenants[tenantID]
	if !ok {
		return store.ErrUnknownTenant
	}
	for _, r := range reports {
		k := key(tenantID, deviceID, r.Collector)
		if prev, ok := m.collectorReportAt[k]; ok && !prev.Before(at) {
			continue
		}
		m.collectorState[k] = r
		m.collectorReportAt[k] = at
	}
	dk := key(tenantID, deviceID)
	if prev, ok := m.lastSeen[dk]; ok && prev.After(at) {
		return nil
	}
	m.lastSeen[dk] = at
	if d, ok := m.devices[dk]; ok {
		if t.DeviceIdentity == protocol.DeviceIdentityHashed {
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
		m.devices[dk] = d
	}
	return nil
}

func (m *Memory) audit(a store.AuditEntry) {
	if a.OccurredAt.IsZero() {
		a.OccurredAt = m.now()
	}
	if a.Detail != nil {
		b, _ := json.Marshal(a.Detail)
		var d map[string]any
		_ = json.Unmarshal(b, &d)
		a.Detail = d
	}
	m.audits = append(m.audits, a)
}

// DeploymentKeyByHash implements store.Store.
func (m *Memory) DeploymentKeyByHash(_ context.Context, tenantID, keyHash string) (store.DeploymentKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if k.TenantID == tenantID && k.KeyHash == keyHash {
			return k, nil
		}
	}
	return store.DeploymentKey{}, store.ErrDeploymentKeyUnknown
}

func (m *Memory) verificationLocked(tenantID string) (store.DeviceVerification, error) {
	if _, ok := m.tenants[tenantID]; !ok {
		return store.DeviceVerification{}, store.ErrUnknownTenant
	}
	v := store.DeviceVerification{Mode: store.VerificationNone}
	if mode, ok := m.verification[tenantID]; ok {
		v.Mode = mode
	}
	for _, c := range m.connections[tenantID] {
		if c.Provider == "entra" && c.Status == "active" {
			v.EntraTenantID = c.EntraTenantID
		}
	}
	return v, nil
}

// DeviceVerification implements store.Store.
func (m *Memory) DeviceVerification(_ context.Context, tenantID string) (store.DeviceVerification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.verificationLocked(tenantID)
}

// FindDeviceByIntuneID implements store.Store.
func (m *Memory) FindDeviceByIntuneID(_ context.Context, tenantID, intuneDeviceID string) (store.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.devices {
		if d.TenantID == tenantID && d.IntuneDeviceID == intuneDeviceID {
			return d, nil
		}
	}
	return store.Device{}, store.ErrDeviceUnknown
}

// SetDeviceIntuneID implements store.Store.
func (m *Memory) SetDeviceIntuneID(_ context.Context, tenantID, deviceID, intuneDeviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[key(tenantID, deviceID)]
	if !ok {
		return store.ErrDeviceUnknown
	}
	for _, o := range m.devices {
		if o.TenantID == tenantID && o.DeviceID != deviceID && o.IntuneDeviceID == intuneDeviceID {
			return store.ErrIntuneDeviceConflict
		}
	}
	d.IntuneDeviceID = intuneDeviceID
	m.devices[key(tenantID, deviceID)] = d
	return nil
}

// RecordDeploymentEnrolment implements store.Store.
func (m *Memory) RecordDeploymentEnrolment(_ context.Context, tenantID, keyID string, at time.Time, audit store.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[key(tenantID, keyID)]; ok {
		k.EnrolmentCount++
		if k.LastUsedAt == nil || k.LastUsedAt.Before(at) {
			t := at
			k.LastUsedAt = &t
		}
		m.keys[key(tenantID, keyID)] = k
	}
	m.audit(audit)
	return nil
}

// Audit implements store.Store.
func (m *Memory) Audit(_ context.Context, audit store.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit(audit)
	return nil
}

// CreateDeploymentKey implements store.Store.
func (m *Memory) CreateDeploymentKey(_ context.Context, k store.DeploymentKey, audit store.AuditEntry) (store.DeploymentKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[k.TenantID]; !ok {
		return store.DeploymentKey{}, store.ErrUnknownTenant
	}
	m.keys[key(k.TenantID, k.KeyID)] = k
	m.audit(audit)
	return k, nil
}

// RevokeDeploymentKey implements store.Store.
func (m *Memory) RevokeDeploymentKey(_ context.Context, tenantID, keyID string, at time.Time, audit store.AuditEntry) (store.DeploymentKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[key(tenantID, keyID)]
	if !ok {
		return store.DeploymentKey{}, store.ErrDeploymentKeyUnknown
	}
	if k.RevokedAt != nil {
		return k, nil
	}
	t := at
	k.RevokedAt = &t
	m.keys[key(tenantID, keyID)] = k
	m.audit(audit)
	return k, nil
}

// SetDeviceVerification implements store.Store.
func (m *Memory) SetDeviceVerification(_ context.Context, tenantID, mode string, audit store.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.verificationLocked(tenantID)
	if err != nil {
		return err
	}
	if mode == store.VerificationIntune && v.EntraTenantID == "" {
		return store.ErrNoEntraConnection
	}
	m.verification[tenantID] = mode
	if audit.Detail == nil {
		audit.Detail = map[string]any{}
	}
	audit.Detail["previous"] = v.Mode
	m.audit(audit)
	return nil
}

// DeploymentSummary implements store.Store.
func (m *Memory) DeploymentSummary(_ context.Context, tenantID string) (store.DeploymentSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.verificationLocked(tenantID)
	if err != nil {
		return store.DeploymentSummary{}, err
	}
	out := store.DeploymentSummary{DeviceVerification: v.Mode, Keys: m.keysLocked(tenantID)}
	conns := m.connections[tenantID]
	for i := len(conns) - 1; i >= 0; i-- {
		c := conns[i]
		if out.Connection == nil || (c.Status == "active" && out.Connection.Status != "active") {
			out.Connection = &c
		}
	}
	for _, d := range m.devices {
		if d.TenantID != tenantID || d.RevokedAt != nil {
			continue
		}
		out.DevicesEnrolled++
		if out.LastEnrolledAt == nil || d.EnrolledAt.After(*out.LastEnrolledAt) {
			t := d.EnrolledAt
			out.LastEnrolledAt = &t
		}
	}
	out.ScimUsers, out.ScimGroups = m.scimUsers[tenantID], m.scimGroups[tenantID]
	if t, ok := m.scimLast[tenantID]; ok && !t.IsZero() {
		out.LastProvisionedAt = &t
	}
	return out, nil
}

// PolicyInputs implements store.Store.
func (m *Memory) PolicyInputs(_ context.Context, tenantID string) (store.PolicyInputs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return store.PolicyInputs{}, store.ErrUnknownTenant
	}
	ceiling := m.ceilings[tenantID]
	if ceiling == "" {
		ceiling = string(protocol.ModeM0)
	}
	in := store.PolicyInputs{Tenant: store.PolicyTenant{
		TenantID: tenantID, Status: t.Status, IngestEnabled: t.IngestEnabled, CeilingMode: ceiling,
	}}
	seen := map[string]bool{}
	for _, h := range m.hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			in.InterceptionHosts = append(in.InterceptionHosts, h)
		}
	}
	sort.Strings(in.InterceptionHosts)
	return in, nil
}

// LatestPolicyBundle implements store.Store.
func (m *Memory) LatestPolicyBundle(_ context.Context, tenantID string) (store.PolicyBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.bundles[tenantID]
	if len(rows) == 0 {
		return store.PolicyBundle{}, store.ErrNoPolicyBundle
	}
	return rows[len(rows)-1], nil
}

// MintPolicyBundle implements store.Store. decide runs under the store's lock and must not call back
// into the store.
func (m *Memory) MintPolicyBundle(_ context.Context, tenantID string, decide func(latest *store.PolicyBundle) (store.MintDecision, error)) (store.PolicyBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *store.PolicyBundle
	if rows := m.bundles[tenantID]; len(rows) > 0 {
		l := rows[len(rows)-1]
		latest = &l
	}
	d, err := decide(latest)
	if err != nil {
		return store.PolicyBundle{}, err
	}
	if d.Bundle == nil {
		if latest == nil {
			return store.PolicyBundle{}, store.ErrNoPolicyBundle
		}
		return *latest, nil
	}
	nb := *d.Bundle
	nb.TenantID = tenantID
	nb.CreatedAt = nb.EffectiveFrom
	if latest != nil && nb.Version <= latest.Version {
		return store.PolicyBundle{}, errors.New("storetest: duplicate policy bundle version")
	}
	m.bundles[tenantID] = append(m.bundles[tenantID], nb)
	if d.Audit != nil {
		m.audit(*d.Audit)
	}
	return nb, nil
}
