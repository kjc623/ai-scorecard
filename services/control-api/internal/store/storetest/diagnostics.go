package storetest

import (
	"context"
	"sort"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

// memoryDiagnostics is one request and, once received, its bundle.
type memoryDiagnostics struct {
	row    store.Diagnostics
	bundle []byte
}

func (m *Memory) pendingDiagnostics(tenantID, deviceID string, now time.Time) *memoryDiagnostics {
	var oldest *memoryDiagnostics
	for _, d := range m.diagnostics {
		r := d.row
		if r.TenantID != tenantID || r.DeviceID != deviceID || r.ReceivedAt != nil || !r.ExpiresAt.After(now) {
			continue
		}
		if oldest == nil || r.RequestedAt.Before(oldest.row.RequestedAt) {
			oldest = d
		}
	}
	return oldest
}

// RequestDiagnostics implements store.Store.
func (m *Memory) RequestDiagnostics(_ context.Context, d store.Diagnostics, audit store.AuditEntry) (store.Diagnostics, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dev, ok := m.devices[key(d.TenantID, d.DeviceID)]
	if !ok {
		return store.Diagnostics{}, false, store.ErrDeviceUnknown
	}
	if dev.RevokedAt != nil {
		return store.Diagnostics{}, false, store.ErrDeviceRevoked
	}
	if p := m.pendingDiagnostics(d.TenantID, d.DeviceID, d.RequestedAt); p != nil {
		return p.row, false, nil
	}
	d.RequestedAt, d.ExpiresAt = d.RequestedAt.UTC(), d.ExpiresAt.UTC()
	m.diagnostics[key(d.TenantID, d.RequestID)] = &memoryDiagnostics{row: d}
	m.audit(audit)
	return d, true, nil
}

// PendingDiagnostics implements store.Store.
func (m *Memory) PendingDiagnostics(_ context.Context, tenantID, deviceID string, now time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.pendingDiagnostics(tenantID, deviceID, now); p != nil {
		return p.row.RequestID, nil
	}
	return "", nil
}

// ReceiveDiagnostics implements store.Store.
func (m *Memory) ReceiveDiagnostics(_ context.Context, d store.Diagnostics, bundle []byte, audit store.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	got, ok := m.diagnostics[key(d.TenantID, d.RequestID)]
	if !ok || got.row.DeviceID != d.DeviceID || got.row.ReceivedAt != nil || !got.row.ExpiresAt.After(*d.ReceivedAt) {
		return store.ErrDiagnosticsNotPending
	}
	at := d.ReceivedAt.UTC()
	got.row.ReceivedAt, got.row.AgentVersion, got.row.SHA256 = &at, d.AgentVersion, d.SHA256
	got.row.SizeBytes, got.row.ExpiresAt = int64(len(bundle)), d.ExpiresAt.UTC()
	got.bundle = append([]byte(nil), bundle...)
	m.audit(audit)
	return nil
}

// DeviceDiagnostics implements store.Store.
func (m *Memory) DeviceDiagnostics(_ context.Context, tenantID, deviceID string, now time.Time, limit int) ([]store.Diagnostics, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.Diagnostics{}
	for _, d := range m.diagnostics {
		if d.row.TenantID == tenantID && d.row.DeviceID == deviceID && d.row.ExpiresAt.After(now) {
			out = append(out, d.row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DiagnosticsBundle implements store.Store.
func (m *Memory) DiagnosticsBundle(_ context.Context, tenantID, deviceID, requestID string, audit store.AuditEntry) (store.Diagnostics, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.diagnostics[key(tenantID, requestID)]
	if !ok || d.row.DeviceID != deviceID || d.row.ReceivedAt == nil || !d.row.ExpiresAt.After(audit.OccurredAt) {
		return store.Diagnostics{}, nil, store.ErrDiagnosticsUnknown
	}
	m.audit(audit)
	return d.row, append([]byte(nil), d.bundle...), nil
}
