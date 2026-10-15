package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// TestDiagnosticsAgainstPostgres drives a diagnostics request as sac_control: a request waits until
// the device answers it, a second request while one waits returns the first, the device's upload is
// stored once and only for its own request, a bundle is read with its audit row, a revoked or
// unknown device is refused, and nothing is listed or served past its expiry.
func TestDiagnosticsAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	device, other := pgtest.UUID(t), pgtest.UUID(t)
	for _, id := range []string{device, other} {
		if _, err := st.UpsertDevice(ctx, store.Device{TenantID: tenant, DeviceID: id, OS: "windows", ManagedState: "managed", EnrolledAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	audit := func(action string, at time.Time) store.AuditEntry {
		return store.AuditEntry{TenantID: tenant, ActorType: store.ActorUser, ActorID: "admin@example.com",
			Action: action, ObjectType: "device_diagnostics", OccurredAt: at}
	}
	request := func(dev string, at time.Time) (store.Diagnostics, bool) {
		t.Helper()
		id := pgtest.UUID(t)
		d, created, err := st.RequestDiagnostics(ctx, store.Diagnostics{TenantID: tenant, RequestID: id, DeviceID: dev,
			RequestedBy: "admin@example.com", RequestedAt: at, ExpiresAt: at.Add(7 * 24 * time.Hour)}, audit("device.diagnostics.request", at))
		if err != nil {
			t.Fatal(err)
		}
		return d, created
	}

	first, created := request(device, t0)
	if !created || first.ReceivedAt != nil {
		t.Fatalf("first request: %+v created %v", first, created)
	}
	if again, created := request(device, t0.Add(time.Minute)); created || again.RequestID != first.RequestID {
		t.Fatalf("a request while one waits: %+v created %v, want the waiting one", again, created)
	}
	if got, err := st.PendingDiagnostics(ctx, tenant, device, t0.Add(time.Minute)); err != nil || got != first.RequestID {
		t.Fatalf("pending = %q, %v", got, err)
	}
	if got, err := st.PendingDiagnostics(ctx, tenant, other, t0); err != nil || got != "" {
		t.Fatalf("another device's pending = %q, %v", got, err)
	}

	bundle := []byte("PK\x03\x04 diagnostics")
	receive := func(dev, id string, at time.Time) error {
		return st.ReceiveDiagnostics(ctx, store.Diagnostics{TenantID: tenant, DeviceID: dev, RequestID: id, ReceivedAt: &at,
			AgentVersion: "1.0.42", SHA256: "ab" + string(bytes.Repeat([]byte("0"), 62)), ExpiresAt: at.Add(14 * 24 * time.Hour)},
			bundle, store.AuditEntry{TenantID: tenant, ActorType: store.ActorDevice, ActorID: dev, Action: "device.diagnostics.receive",
				ObjectType: "device_diagnostics", ObjectID: id, OccurredAt: at})
	}
	if err := receive(other, first.RequestID, t0.Add(2*time.Minute)); !errors.Is(err, store.ErrDiagnosticsNotPending) {
		t.Fatalf("another device answering: %v", err)
	}
	if err := receive(device, first.RequestID, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := receive(device, first.RequestID, t0.Add(3*time.Minute)); !errors.Is(err, store.ErrDiagnosticsNotPending) {
		t.Fatalf("a second answer: %v", err)
	}
	if got, err := st.PendingDiagnostics(ctx, tenant, device, t0.Add(3*time.Minute)); err != nil || got != "" {
		t.Fatalf("pending after the answer = %q, %v", got, err)
	}

	list, err := st.DeviceDiagnostics(ctx, tenant, device, t0.Add(3*time.Minute), 20)
	if err != nil || len(list) != 1 || list[0].ReceivedAt == nil || list[0].SizeBytes != int64(len(bundle)) || list[0].AgentVersion != "1.0.42" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	read := audit("device.diagnostics.download", t0.Add(4*time.Minute))
	read.ObjectID = first.RequestID
	got, raw, err := st.DiagnosticsBundle(ctx, tenant, device, first.RequestID, read)
	if err != nil || !bytes.Equal(raw, bundle) || got.RequestID != first.RequestID {
		t.Fatalf("bundle = %+v %q, %v", got, raw, err)
	}
	if _, _, err := st.DiagnosticsBundle(ctx, tenant, other, first.RequestID, read); !errors.Is(err, store.ErrDiagnosticsUnknown) {
		t.Fatalf("the bundle read as another device's: %v", err)
	}
	late := audit("device.diagnostics.download", t0.Add(15*24*time.Hour))
	if _, _, err := st.DiagnosticsBundle(ctx, tenant, device, first.RequestID, late); !errors.Is(err, store.ErrDiagnosticsUnknown) {
		t.Fatalf("an expired bundle: %v", err)
	}
	if list, err := st.DeviceDiagnostics(ctx, tenant, device, t0.Add(15*24*time.Hour), 20); err != nil || len(list) != 0 {
		t.Fatalf("listed past expiry: %+v, %v", list, err)
	}

	// A request that lapses unanswered is no longer pending, and a new one can be made.
	second, _ := request(device, t0.Add(5*time.Minute))
	if got, _ := st.PendingDiagnostics(ctx, tenant, device, t0.Add(8*24*time.Hour)); got != "" {
		t.Fatalf("a lapsed request is still pending: %q", got)
	}
	if err := receive(device, second.RequestID, t0.Add(8*24*time.Hour)); !errors.Is(err, store.ErrDiagnosticsNotPending) {
		t.Fatalf("an answer to a lapsed request: %v", err)
	}

	if _, _, err := st.RequestDiagnostics(ctx, store.Diagnostics{TenantID: tenant, RequestID: pgtest.UUID(t), DeviceID: pgtest.UUID(t),
		RequestedBy: "a", RequestedAt: t0, ExpiresAt: t0.Add(time.Hour)}, audit("device.diagnostics.request", t0)); !errors.Is(err, store.ErrDeviceUnknown) {
		t.Fatalf("an unknown device: %v", err)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE ops.device SET revoked_at = now() WHERE tenant_id = $1::uuid AND device_id = $2::uuid`, tenant, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RequestDiagnostics(ctx, store.Diagnostics{TenantID: tenant, RequestID: pgtest.UUID(t), DeviceID: other,
		RequestedBy: "a", RequestedAt: t0, ExpiresAt: t0.Add(time.Hour)}, audit("device.diagnostics.request", t0)); !errors.Is(err, store.ErrDeviceRevoked) {
		t.Fatalf("a revoked device: %v", err)
	}

	var audits int
	if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM ops.audit WHERE tenant_id = $1::uuid AND action LIKE 'device.diagnostics.%'`, tenant).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	// Two requests made, one answer stored, one bundle read: the refusals wrote nothing.
	if audits != 4 {
		t.Fatalf("%d diagnostics audit rows, want 4", audits)
	}
}
