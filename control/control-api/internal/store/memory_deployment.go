package store

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// deploymentTables is the in-memory mirror of the deployment and policy tables, transliterating
// deployment_sql.go: the same keys, the same "first revocation stands", the same per-tenant
// uniqueness of an Intune id, and minting under the store's one lock as SQL mints under the
// per-tenant advisory lock.
type deploymentTables struct {
	keys         map[string]DeploymentKey // tenant|key_id
	verification map[string]string        // tenant -> mode (absent means 'none', the column default)
	connections  map[string][]IdentityConnection
	intuneIndex  map[string]string // tenant|intune id -> device id
	audits       []AuditEntry
	ceilings     map[string]string // tenant -> ceiling_mode
	hosts        []string          // ref.tool_catalogue TLS hosts
	releases     []ClassifierRelease
	bundles      map[string][]PolicyBundle // tenant -> rows, ascending version
	scimUsers    map[string]int64
	scimGroups   map[string]int64
	scimLast     map[string]time.Time
}

func newDeploymentTables() deploymentTables {
	return deploymentTables{
		keys:         map[string]DeploymentKey{},
		verification: map[string]string{},
		connections:  map[string][]IdentityConnection{},
		intuneIndex:  map[string]string{},
		ceilings:     map[string]string{},
		bundles:      map[string][]PolicyBundle{},
		scimUsers:    map[string]int64{},
		scimGroups:   map[string]int64{},
		scimLast:     map[string]time.Time{},
	}
}

// AddDeploymentKey registers a key row as a seed would.
func (m *Memory) AddDeploymentKey(k DeploymentKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.keys[k.TenantID+"|"+k.KeyID] = k
}

// DeploymentKeys returns a copy of the tenant's keys, for assertions.
func (m *Memory) DeploymentKeys(tenantID string) []DeploymentKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DeploymentKey
	for _, k := range m.dep.keys {
		if k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// SetVerification sets the tenant's device_verification directly, as a seed would.
func (m *Memory) SetVerification(tenantID, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.verification[tenantID] = mode
}

// AddIdentityConnection registers an ops.identity_connection row.
func (m *Memory) AddIdentityConnection(tenantID string, c IdentityConnection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.connections[tenantID] = append(m.dep.connections[tenantID], c)
}

// SetCeiling sets the tenant's ceiling_mode, the policy input a bundle's default is composed from.
func (m *Memory) SetCeiling(tenantID, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.ceilings[tenantID] = mode
}

// SetCatalogueHosts replaces the tool catalogue's TLS hosts.
func (m *Memory) SetCatalogueHosts(hosts ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.hosts = append([]string(nil), hosts...)
}

// SetClassifierReleases replaces ref.classifier_release, in preference order.
func (m *Memory) SetClassifierReleases(releases ...ClassifierRelease) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.releases = append([]ClassifierRelease(nil), releases...)
}

// SetScimSummary sets the SCIM counts the admin page reads.
func (m *Memory) SetScimSummary(tenantID string, users, groups int64, last time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.scimUsers[tenantID], m.dep.scimGroups[tenantID], m.dep.scimLast[tenantID] = users, groups, last
}

// Audits returns a copy of every audit row written, for assertions.
func (m *Memory) Audits() []AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditEntry(nil), m.dep.audits...)
}

// PolicyBundles returns a copy of the tenant's bundle rows, ascending by version.
func (m *Memory) PolicyBundles(tenantID string) []PolicyBundle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]PolicyBundle(nil), m.dep.bundles[tenantID]...)
}

func (m *Memory) audit(a AuditEntry) {
	if a.OccurredAt.IsZero() {
		a.OccurredAt = m.now()
	}
	if a.Detail != nil {
		// The SQL path serialises the detail; copying it here keeps a later caller mutation out of
		// the recorded row, as the database would.
		b, _ := json.Marshal(a.Detail)
		var d map[string]any
		_ = json.Unmarshal(b, &d)
		a.Detail = d
	}
	m.dep.audits = append(m.dep.audits, a)
}

// DeploymentKeyByHash implements DeploymentStore.
func (m *Memory) DeploymentKeyByHash(_ context.Context, tenantID, keyHash string) (DeploymentKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.dep.keys {
		if k.TenantID == tenantID && k.KeyHash == keyHash {
			return k, nil
		}
	}
	return DeploymentKey{}, ErrDeploymentKeyUnknown
}

func (m *Memory) verificationLocked(tenantID string) (DeviceVerification, error) {
	if _, ok := m.tenants[tenantID]; !ok {
		return DeviceVerification{}, ErrUnknownTenant
	}
	v := DeviceVerification{Mode: VerificationNone}
	if mode, ok := m.dep.verification[tenantID]; ok {
		v.Mode = mode
	}
	for _, c := range m.dep.connections[tenantID] {
		if c.Provider == "entra" && c.Status == "active" {
			v.EntraTenantID = c.EntraTenantID
		}
	}
	return v, nil
}

// DeviceVerification implements DeploymentStore.
func (m *Memory) DeviceVerification(_ context.Context, tenantID string) (DeviceVerification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.verificationLocked(tenantID)
}

// FindDeviceByIntuneID implements DeploymentStore.
func (m *Memory) FindDeviceByIntuneID(_ context.Context, tenantID, intuneDeviceID string) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deviceID, ok := m.dep.intuneIndex[tenantID+"|"+intuneDeviceID]
	if !ok {
		return Device{}, ErrDeviceUnknown
	}
	d, ok := m.devices[deviceKey(tenantID, deviceID)]
	if !ok {
		return Device{}, ErrDeviceUnknown
	}
	return d, nil
}

// SetDeviceIntuneID implements DeploymentStore.
func (m *Memory) SetDeviceIntuneID(_ context.Context, tenantID, deviceID, intuneDeviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := deviceKey(tenantID, deviceID)
	d, ok := m.devices[key]
	if !ok {
		return ErrDeviceUnknown
	}
	if holder, ok := m.dep.intuneIndex[tenantID+"|"+intuneDeviceID]; ok && holder != deviceID {
		return ErrIntuneDeviceConflict
	}
	if d.IntuneDeviceID != "" {
		delete(m.dep.intuneIndex, tenantID+"|"+d.IntuneDeviceID)
	}
	d.IntuneDeviceID = intuneDeviceID
	m.devices[key] = d
	m.dep.intuneIndex[tenantID+"|"+intuneDeviceID] = deviceID
	return nil
}

// RecordDeploymentEnrolment implements DeploymentStore.
func (m *Memory) RecordDeploymentEnrolment(_ context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.dep.keys[tenantID+"|"+keyID]; ok {
		k.EnrolmentCount++
		if k.LastUsedAt == nil || k.LastUsedAt.Before(at) {
			t := at
			k.LastUsedAt = &t
		}
		m.dep.keys[tenantID+"|"+keyID] = k
	}
	m.audit(audit)
	return nil
}

// Audit implements DeploymentStore.
func (m *Memory) Audit(_ context.Context, audit AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit(audit)
	return nil
}

// CreateDeploymentKey implements DeploymentStore.
func (m *Memory) CreateDeploymentKey(_ context.Context, k DeploymentKey, audit AuditEntry) (DeploymentKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[k.TenantID]; !ok {
		return DeploymentKey{}, ErrUnknownTenant
	}
	m.dep.keys[k.TenantID+"|"+k.KeyID] = k
	m.audit(audit)
	return k, nil
}

// RevokeDeploymentKey implements DeploymentStore.
func (m *Memory) RevokeDeploymentKey(_ context.Context, tenantID, keyID string, at time.Time, audit AuditEntry) (DeploymentKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.dep.keys[tenantID+"|"+keyID]
	if !ok {
		return DeploymentKey{}, ErrDeploymentKeyUnknown
	}
	if k.RevokedAt != nil {
		return k, nil
	}
	t := at
	k.RevokedAt = &t
	m.dep.keys[tenantID+"|"+keyID] = k
	m.audit(audit)
	return k, nil
}

// SetDeviceVerification implements DeploymentStore.
func (m *Memory) SetDeviceVerification(_ context.Context, tenantID, mode string, audit AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.verificationLocked(tenantID)
	if err != nil {
		return err
	}
	if mode == VerificationIntune && v.EntraTenantID == "" {
		return ErrNoEntraConnection
	}
	m.dep.verification[tenantID] = mode
	if audit.Detail == nil {
		audit.Detail = map[string]any{}
	}
	audit.Detail["previous"] = v.Mode
	m.audit(audit)
	return nil
}

// DeploymentSummary implements DeploymentStore.
func (m *Memory) DeploymentSummary(_ context.Context, tenantID string) (DeploymentSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.verificationLocked(tenantID)
	if err != nil {
		return DeploymentSummary{}, err
	}
	out := DeploymentSummary{DeviceVerification: v.Mode}
	conns := m.dep.connections[tenantID]
	for i := len(conns) - 1; i >= 0; i-- { // newest first, active preferred: SQL's ORDER BY
		c := conns[i]
		if out.Connection == nil || (c.Status == "active" && out.Connection.Status != "active") {
			cc := c
			out.Connection = &cc
		}
	}
	for _, k := range m.dep.keys {
		if k.TenantID == tenantID {
			out.Keys = append(out.Keys, k)
		}
	}
	sort.Slice(out.Keys, func(i, j int) bool {
		if !out.Keys[i].CreatedAt.Equal(out.Keys[j].CreatedAt) {
			return out.Keys[i].CreatedAt.After(out.Keys[j].CreatedAt)
		}
		return out.Keys[i].KeyID < out.Keys[j].KeyID
	})
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
	out.ScimUsers, out.ScimGroups = m.dep.scimUsers[tenantID], m.dep.scimGroups[tenantID]
	if t, ok := m.dep.scimLast[tenantID]; ok && !t.IsZero() {
		out.LastProvisionedAt = &t
	}
	return out, nil
}

// PolicyInputs implements PolicyStore.
func (m *Memory) PolicyInputs(_ context.Context, tenantID string) (PolicyInputs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return PolicyInputs{}, ErrUnknownTenant
	}
	ceiling := m.dep.ceilings[tenantID]
	if ceiling == "" {
		ceiling = "m0" // the schema has no default; the narrowest mode is the only safe stand-in
	}
	in := PolicyInputs{Tenant: PolicyTenant{
		TenantID: tenantID, Status: t.Status, IngestEnabled: t.IngestEnabled, CeilingMode: ceiling,
	}}
	seen := map[string]bool{}
	for _, h := range m.dep.hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			in.InterceptionHosts = append(in.InterceptionHosts, h)
		}
	}
	sort.Strings(in.InterceptionHosts)
	for _, r := range m.dep.releases {
		if r.State == "enforcing" || r.State == "shadow" {
			rr := r
			if in.Classifier == nil || (rr.State == "enforcing" && in.Classifier.State != "enforcing") {
				in.Classifier = &rr
			}
		}
	}
	return in, nil
}

// LatestPolicyBundle implements PolicyStore.
func (m *Memory) LatestPolicyBundle(_ context.Context, tenantID string) (PolicyBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.dep.bundles[tenantID]
	if len(rows) == 0 {
		return PolicyBundle{}, ErrNoPolicyBundle
	}
	return rows[len(rows)-1], nil
}

// MintPolicyBundle implements PolicyStore. The callback runs under the store's lock, as the SQL
// callback runs under the per-tenant advisory lock; it must not call back into the store.
func (m *Memory) MintPolicyBundle(_ context.Context, tenantID string, decide func(latest *PolicyBundle) (MintDecision, error)) (PolicyBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *PolicyBundle
	if rows := m.dep.bundles[tenantID]; len(rows) > 0 {
		l := rows[len(rows)-1]
		latest = &l
	}
	d, err := decide(latest)
	if err != nil {
		return PolicyBundle{}, err
	}
	if d.Bundle == nil {
		if latest == nil {
			return PolicyBundle{}, ErrNoPolicyBundle
		}
		return *latest, nil
	}
	nb := *d.Bundle
	nb.TenantID = tenantID
	nb.CreatedAt = nb.EffectiveFrom
	if latest != nil && nb.Version <= latest.Version {
		// The primary key (tenant_id, bundle_version) would refuse it; so does the mirror.
		return PolicyBundle{}, errDuplicateBundleVersion
	}
	m.dep.bundles[tenantID] = append(m.dep.bundles[tenantID], nb)
	if d.Audit != nil {
		m.audit(*d.Audit)
	}
	return nb, nil
}

// AddPolicyBundle registers a bundle row directly, as a seed or a legacy row would.
func (m *Memory) AddPolicyBundle(b PolicyBundle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dep.bundles[b.TenantID] = append(m.dep.bundles[b.TenantID], b)
	sort.Slice(m.dep.bundles[b.TenantID], func(i, j int) bool {
		return m.dep.bundles[b.TenantID][i].Version < m.dep.bundles[b.TenantID][j].Version
	})
}

type storeError string

func (e storeError) Error() string { return string(e) }

const errDuplicateBundleVersion = storeError("store: duplicate policy bundle version")
